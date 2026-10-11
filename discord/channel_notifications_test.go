package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"nofx/notify"
	"nofx/store"
)

func notificationFixture(t *testing.T) (*store.Store, *SourceManager, store.ChannelNotificationConfig) {
	t.Helper()
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC().Add(-time.Second)
	cfg, err := st.SaveChannelNotifications(store.ChannelNotificationConfig{Enabled: true, Recipient: "notify@example.com", Monitors: []store.ChannelMonitor{{ChannelID: "1539560125189062696", Enabled: true, Label: "signals"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(cfg)
	if err = st.DiscordMessage().SaveSourceConfig("v1", "[]", string(frozen)); err != nil {
		t.Fatal(err)
	}
	// No credential is decrypted or used by these tests.
	if err = st.GormDB().Create(&store.DiscordConfig{ID: 1, RunMode: "observe", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	return st, m, cfg
}
func notificationEvent(m *SourceManager, id string, when time.Time) GatewayEvent {
	ev := sourceEvent(id, "MESSAGE_CREATE", "new", when)
	ev.ChannelID = "1539560125189062696"
	ev.ConfigVersion = "v1"
	ev.NotificationRealtime = true
	ev.NotificationEpoch = m.NotificationEpoch()
	ev.Origin = "gateway"
	ev.Payload = json.RawMessage(`{"id":"900","channel_id":"1539560125189062696","author":{"id":"7","username":"author"},"content":"new","timestamp":"` + when.Format(time.RFC3339Nano) + `"}`)
	return ev
}
func TestChannelNotificationFanoutAndNoTrading(t *testing.T) {
	st, m, cfg := notificationFixture(t)
	now := time.Now().UTC()
	ev := notificationEvent(m, "a", now)
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	cursor, jobs, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %+v", err, jobs)
	}
	if _, again, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now); err != nil || len(again) != 0 {
		t.Fatalf("duplicate %+v %v", again, err)
	}
	pending, _ := st.DiscordMessage().PendingDeliveries()
	if pending != 0 {
		t.Fatal("mail became trade")
	}
	msg, _ := st.DiscordMessage().GetByMessageID(ev.ChannelID, ev.MessageID)
	if msg.ProcessingStatus != store.DiscordMsgDone {
		t.Fatal(msg.ProcessingStatus)
	}
	var signals int64
	st.GormDB().Model(&store.CopyTradeSignal{}).Count(&signals)
	if signals != 0 {
		t.Fatal("mail created signal")
	}
	cfg.Recipient = "new@example.com"
	if _, err := st.SaveChannelNotifications(cfg, now); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.LatestChannelNotification(cfg.Monitors[0].ID)
	if latest.Status != "cancelled" {
		t.Fatal(latest)
	}
	if cursor < 1 {
		t.Fatal("cursor did not advance")
	}
}
func TestChannelNotificationRejectsHistoryEditsAndOldEpoch(t *testing.T) {
	for _, kind := range []string{"baseline", "history", "old_epoch", "edit", "system"} {
		t.Run(kind, func(t *testing.T) {
			st, m, _ := notificationFixture(t)
			now := time.Now().UTC()
			ev := notificationEvent(m, "a", now)
			switch kind {
			case "baseline":
				ev.Baseline = true
			case "history":
				ev.NotificationRealtime = false
			case "old_epoch":
				ev.NotificationEpoch = "previous"
			case "edit":
				ev.Kind = "MESSAGE_UPDATE"
			case "system":
				ev.Payload = json.RawMessage(strings.Replace(string(ev.Payload), `"content":"new"`, `"content":"new","type":6`, 1))
			}
			if err := m.Ingest(ev); err != nil {
				t.Fatal(err)
			}
			_, jobs, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("%v %+v", err, jobs)
			}
		})
	}
}
func TestNotificationConfigVersionAndActivation(t *testing.T) {
	st, _, cfg := notificationFixture(t)
	now := time.Now().UTC()
	stale := cfg
	cfg.Monitors[0].Enabled = false
	next, err := st.SaveChannelNotifications(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SaveChannelNotifications(stale, now); !errors.Is(err, store.ErrNotificationConflict) {
		t.Fatal(err)
	}
	next.Monitors[0].Enabled = true
	next, err = st.SaveChannelNotifications(next, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if next.Monitors[0].Generation != 2 || !next.Monitors[0].ActivatedAt.Equal(now.Add(time.Second)) {
		t.Fatal(next)
	}
	dc, _ := st.DiscordConfig().Get()
	if dc.RunMode != "observe" || dc.Revision != 0 {
		t.Fatal("mail changed trading config")
	}
}
func TestNotificationMailBoundAndSnapshot(t *testing.T) {
	msg := store.DiscordMessage{Content: strings.Repeat("消息", 10000), AuthorName: "author", GuildID: "guild", EmbedsJSON: `[{"description":"old card"}]`, AttachmentsJSON: `[{"filename":"a.png","url":"https://example.com/a.png"}]`, MessageTimestamp: time.Now()}
	raw, _ := json.Marshal(msg)
	job := store.ChannelNotification{ChannelID: "123", MessageID: "456", MessageJSON: string(raw)}
	_, body, err := notificationMail(&job)
	if err != nil || len(body) > 16384 || !strings.Contains(body, "已截断") || !strings.Contains(body, "https://discord.com/channels/guild/123/456") {
		t.Fatal(err, len(body))
	}
}
func TestNotificationSenderDoesNotRetryUnknown(t *testing.T) {
	st, m, cfg := notificationFixture(t)
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_USER", "sender@example.com")
	t.Setenv("SMTP_PASS", "private")
	// EncryptedString accepts a raw test value when no crypto is configured.
	if err := st.GormDB().Model(&store.DiscordConfig{}).Where("id=1").Update("token", "test-token").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ev := notificationEvent(m, "a", now)
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now); err != nil {
		t.Fatal(err)
	}
	n := NewChannelNotifier(st, m.NotificationEpoch())
	calls := 0
	n.send = func(notify.SMTPConfig, string, string, string, time.Time) error {
		calls++
		return &notify.DeliveryError{Kind: "uncertain", Stage: "delivery"}
	}
	if err := n.processOne(now); err != nil {
		t.Fatal(err)
	}
	if err := n.processOne(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.LatestChannelNotification(cfg.Monitors[0].ID)
	if calls != 1 || latest.Status != "uncertain" {
		t.Fatalf("%d %+v", calls, latest)
	}
}

func prepareNotificationSend(t *testing.T) (*store.Store, *SourceManager, store.ChannelNotificationConfig, time.Time) {
	t.Helper()
	st, m, cfg := notificationFixture(t)
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_USER", "sender@example.com")
	t.Setenv("SMTP_PASS", "test-only")
	if err := st.GormDB().Model(&store.DiscordConfig{}).Where("id=1").Update("token", "test-token").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := m.Ingest(notificationEvent(m, "first", now)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now); err != nil {
		t.Fatal(err)
	}
	return st, m, cfg, now
}

func TestNotificationRetriesAreBoundedAndClassified(t *testing.T) {
	for _, kind := range []string{"temporary", "permanent", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			st, m, cfg, now := prepareNotificationSend(t)
			n := NewChannelNotifier(st, m.NotificationEpoch())
			calls := 0
			n.send = func(_ notify.SMTPConfig, to, subject, body string, deadline time.Time) error {
				calls++
				if to != "notify@example.com" {
					t.Fatal(to)
				}
				return &notify.DeliveryError{Kind: kind, Stage: "recipient"}
			}
			if err := n.processOne(now); err != nil {
				t.Fatal(err)
			}
			job, _ := st.LatestChannelNotification(cfg.Monitors[0].ID)
			if kind == "temporary" {
				if job.Status != "retry" || job.NextAttemptAt.Sub(now) < 14*time.Second {
					t.Fatal(job)
				}
				if err := n.processOne(now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatal("retried before deadline")
				}
				if err := n.processOne(job.NextAttemptAt); err != nil {
					t.Fatal(err)
				}
				job, _ = st.LatestChannelNotification(cfg.Monitors[0].ID)
				if job.Status != "retry" || time.Until(job.NextAttemptAt) < 44*time.Second {
					t.Fatal(job)
				}
				if err := n.processOne(job.NextAttemptAt); err != nil {
					t.Fatal(err)
				}
				job, _ = st.LatestChannelNotification(cfg.Monitors[0].ID)
				if job.Status != "failed" || calls != 3 {
					t.Fatal(job, calls)
				}
			} else {
				if err := n.processOne(now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || (kind == "uncertain" && job.Status != "uncertain") || (kind == "permanent" && job.Status != "failed") {
					t.Fatal(job, calls)
				}
			}
		})
	}
}
func TestNotificationSendingDoesNotBlockTradingAndFailedCommitDoesNotResend(t *testing.T) {
	st, m, cfg, now := prepareNotificationSend(t)
	n := NewChannelNotifier(st, m.NotificationEpoch())
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	n.send = func(notify.SMTPConfig, string, string, string, time.Time) error {
		calls++
		close(entered)
		<-release
		return nil
	}
	if err := st.GormDB().Exec(`CREATE TRIGGER fail_mail_commit BEFORE UPDATE ON channel_notifications WHEN NEW.status='sent' BEGIN SELECT RAISE(FAIL, 'test ledger unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.processOne(now) }()
	<-entered
	frozen, _ := json.Marshal(cfg)
	routes, _ := json.Marshal([]Route{{TraderID: "t", Channels: []string{cfg.Monitors[0].ChannelID}, ActivatedAt: now.Add(-time.Second), ExecutionKey: "trade"}})
	if err := st.DiscordMessage().SaveSourceConfig("shared", string(routes), string(frozen)); err != nil {
		t.Fatal(err)
	}
	ev := notificationEvent(m, "second", now.Add(time.Millisecond))
	ev.ConfigVersion = "shared"
	ev.MessageID = "901"
	ev.Payload = json.RawMessage(strings.Replace(string(ev.Payload), `"id":"900"`, `"id":"901"`, 1))
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	pending, err := st.DiscordMessage().PendingDeliveries()
	if err != nil || pending != 1 {
		t.Fatal("SMTP blocked trading receipt", pending, err)
	}
	msg, _ := st.DiscordMessage().GetByMessageID(ev.ChannelID, ev.MessageID)
	if msg.ProcessingStatus != store.DiscordMsgPending {
		t.Fatal("shared message was finalized", msg)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("failed commit reported success")
	}
	if err := n.processOne(now); err != nil {
		t.Fatal(err)
	}
	job, _ := st.LatestChannelNotification(cfg.Monitors[0].ID)
	if calls != 1 || job.Status != "sending" {
		t.Fatal(calls, job)
	}
	if err := n.processOne(now.Add(31 * time.Second)); err != nil {
		t.Fatal(err)
	}
	job, _ = st.LatestChannelNotification(cfg.Monitors[0].ID)
	if job.Status != "uncertain" {
		t.Fatal(job)
	}
}
func TestNotificationCapacityExpiryAndRestart(t *testing.T) {
	for _, scenario := range []string{"capacity", "expiry", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			st, m, cfg := notificationFixture(t)
			now := time.Now().UTC()
			if scenario == "capacity" {
				rows := make([]store.ChannelNotification, 1000)
				for i := range rows {
					rows[i] = store.ChannelNotification{MonitorID: "other", MessageID: fmt.Sprint(i), Status: "pending", ExpiresAt: now.Add(time.Minute)}
				}
				if err := st.GormDB().CreateInBatches(rows, 50).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Ingest(notificationEvent(m, "a", now)); err != nil {
				t.Fatal(err)
			}
			scanTime := now
			if scenario == "expiry" {
				scanTime = now.Add(2 * time.Minute)
			}
			_, jobs, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, scanTime)
			if err != nil || len(jobs) != 1 {
				t.Fatal(jobs, err)
			}
			expected := "dropped"
			if scenario == "expiry" {
				expected = "expired"
			}
			if scenario == "restart" {
				if err := st.ResetChannelNotifications(now); err != nil {
					t.Fatal(err)
				}
				expected = "cancelled"
				next := NewSourceManager(st)
				_, again, err := st.QueueChannelNotifications(next.NotificationEpoch(), 0, now)
				if err != nil || len(again) != 0 {
					t.Fatal("restart replayed", again, err)
				}
			}
			job, _ := st.LatestChannelNotification(cfg.Monitors[0].ID)
			if job.Status != expected {
				t.Fatal(job)
			}
		})
	}
}
func TestNotificationSnapshotRecipientBoundaryAndDeletion(t *testing.T) {
	st, m, cfg := notificationFixture(t)
	now := time.Now().UTC()
	ev := notificationEvent(m, "a", now)
	ev.NotificationPayload = append(json.RawMessage(nil), ev.Payload...)
	ev.Payload = json.RawMessage(strings.Replace(string(ev.Payload), `"content":"new"`, `"content":"later edit"`, 1))
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	_, jobs, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now)
	if err != nil || len(jobs) != 1 {
		t.Fatal(err, jobs)
	}
	if !strings.Contains(jobs[0].MessageJSON, `"content":"new"`) || strings.Contains(jobs[0].MessageJSON, "later edit") {
		t.Fatal("mail snapshot overwritten")
	}
	// A newly queued event that predates the recipient change cannot leak to the new recipient.
	ev.EventID = "b"
	ev.MessageID = "901"
	ev.NotificationPayload = nil
	ev.Payload = json.RawMessage(strings.Replace(string(ev.Payload), `"id":"900"`, `"id":"901"`, 1))
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	cfg.Recipient = "changed@example.com"
	cfg, err = st.SaveChannelNotifications(cfg, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, jobs, err = st.QueueChannelNotifications(m.NotificationEpoch(), 0, now.Add(time.Second))
	if err != nil || len(jobs) != 0 {
		t.Fatal("sent old mail to new address", jobs, err)
	}
	cfg.Monitors = nil
	cfg, err = st.SaveChannelNotifications(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Monitors = []store.ChannelMonitor{{ChannelID: ev.ChannelID, Enabled: true}}
	cfg, err = st.SaveChannelNotifications(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	_, jobs, err = st.QueueChannelNotifications(m.NotificationEpoch(), 0, now)
	if err != nil || len(jobs) != 0 {
		t.Fatal("delete/readd replayed", jobs, err)
	}
}
func TestNotificationVersionCannotBlockHealthyTradingRoute(t *testing.T) {
	st, m, cfg := notificationFixture(t)
	boundary := time.Now().Add(-time.Minute)
	m.cfg = &store.DiscordConfig{Enabled: true, Token: "test", RunMode: "live", ExecutionSince: &boundary}
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{cfg.Monitors[0].ChannelID}, ExecutionKey: "trade", ActivatedAt: boundary}}
	m.status = CollectorStatus{State: "connected", LastHeartbeat: time.Now(), DesiredVersion: "notification-new", AppliedVersion: "notification-old"}
	m.desiredTradingVersion = "trade-unchanged"
	m.appliedTradingVersion = "trade-unchanged"
	st.DiscordMessage().SaveSourceState(&store.DiscordSourceState{ChannelID: cfg.Monitors[0].ChannelID, State: "ready"})
	st.DiscordMessage().SaveSourceState(&store.DiscordSourceState{ChannelID: "other-notify", State: "forbidden"})
	msg := &store.DiscordMessage{MessageTimestamp: time.Now(), ReceivedAt: time.Now(), ExecutionKey: "trade"}
	if err := m.CheckExecution("t", msg, true); err != nil {
		t.Fatal(err)
	}
	m.desiredTradingVersion = "trade-changed"
	if err := m.CheckExecution("t", msg, true); err == nil {
		t.Fatal("unapplied trade configuration accepted")
	}
}
func TestNotificationStoreFailureDoesNotRejectReceipt(t *testing.T) {
	st, m, _ := notificationFixture(t)
	now := time.Now().UTC()
	st.GormDB().Exec(`CREATE TRIGGER fail_mail_insert BEFORE INSERT ON channel_notifications BEGIN SELECT RAISE(FAIL, 'test storage failure'); END`)
	if err := m.Ingest(notificationEvent(m, "a", now)); err != nil {
		t.Fatal("notification write entered receipt transaction", err)
	}
	cursor, _, err := st.QueueChannelNotifications(m.NotificationEpoch(), 0, now)
	if err == nil || cursor != 0 {
		t.Fatal("failed queue advanced cursor", cursor, err)
	}
}

func TestNotificationRetentionProtectsNonterminalAndTradingEvidence(t *testing.T) {
	st, m, cfg, now := prepareNotificationSend(t)
	var job store.ChannelNotification
	st.GormDB().First(&job)
	if err := st.GormDB().Model(&job).Update("updated_at", now.AddDate(0, 0, -31)).Error; err != nil {
		t.Fatal(err)
	}
	stale := store.ChannelNotification{MonitorID: cfg.Monitors[0].ID, MessageID: "old", Status: "sent", UpdatedAt: now.AddDate(0, 0, -31)}
	if err := st.GormDB().Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.CleanChannelNotifications(); err != nil {
		t.Fatal(err)
	}
	var rows int64
	st.GormDB().Model(&store.ChannelNotification{}).Count(&rows)
	if rows != 1 {
		t.Fatal(rows)
	}
	var receipts int64
	st.GormDB().Model(&store.DiscordInbound{}).Count(&receipts)
	if receipts != 1 {
		t.Fatal("deleted receipts", receipts)
	}
	if m.NotificationEpoch() == "" {
		t.Fatal("missing boundary")
	}
}

func TestNotificationSubscriptionUnionAndIndependentVersionACK(t *testing.T) {
	st, m, cfg := notificationFixture(t)
	shared := cfg.Monitors[0].ChannelID
	cfg.Monitors = append(cfg.Monitors, store.ChannelMonitor{ChannelID: "1531557737429991525", Enabled: true})
	var err error
	cfg, err = st.SaveChannelNotifications(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.subscriptions["trade"] = &sourceSubscription{route: Route{TraderID: "trade", Channels: []string{shared}, ExecutionKey: "stable-trade", ActivatedAt: time.Now().Add(-time.Hour)}}
	server, client := net.Pipe()
	m.conn = server
	m.wg.Add(1)
	go m.serve(server)
	frames := make(chan map[string]json.RawMessage, 2)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		decoder := json.NewDecoder(client)
		encoder := json.NewEncoder(client)
		for {
			var req wireFrame
			if decoder.Decode(&req) != nil {
				return
			}
			var data map[string]json.RawMessage
			if json.Unmarshal(req.Data, &data) != nil {
				return
			}
			frames <- data
			var version string
			json.Unmarshal(data["version"], &version)
			reply, _ := json.Marshal(map[string]any{"applied_version": version, "channel_notification_v1": true})
			if encoder.Encode(wireFrame{Type: "response", ID: req.ID, Data: reply}) != nil {
				return
			}
		}
	}()
	defer func() { client.Close(); m.wg.Wait(); <-collectorDone }()
	if err = m.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	data := <-frames
	var channels map[string]struct {
		Trade bool `json:"trade_recovery"`
	}
	json.Unmarshal(data["channels"], &channels)
	if len(channels) != 2 || !channels[shared].Trade || channels["1531557737429991525"].Trade {
		t.Fatal(channels)
	}
	if !m.NotificationsApplied(cfg.Revision) || m.NotificationsApplied(cfg.Revision+1) {
		t.Fatal("incorrect application revision")
	}
	before := m.appliedTradingVersion
	cfg.Monitors = nil
	cfg, err = st.SaveChannelNotifications(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	data = <-frames
	channels = nil
	json.Unmarshal(data["channels"], &channels)
	if len(channels) != 1 || !channels[shared].Trade || before != m.appliedTradingVersion || m.subscriptions["trade"].route.ExecutionKey != "stable-trade" {
		t.Fatal("notification delete changed trading", channels)
	}
}
