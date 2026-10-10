package discord

import (
	"encoding/json"
	"fmt"
	"net"
	"nofx/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sourceStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
func sourceEvent(id, kind, content string, ts time.Time) GatewayEvent {
	b, _ := json.Marshal(map[string]interface{}{"id": "900", "channel_id": "200", "author": map[string]string{"id": "7", "username": "author"}, "content": content, "timestamp": ts})
	return GatewayEvent{EventID: id, Kind: kind, ChannelID: "200", MessageID: "900", ReceivedAt: ts, Payload: b}
}
func TestDurableInboundRetryAndImmutableRevision(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC()
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}, RulesJSON: `{"version":3}`, ExecutionKey: "k", ActivatedAt: now.Add(-time.Minute)}}
	first := sourceEvent("a", "MESSAGE_CREATE", "open", now)
	if err := m.Ingest(first); err != nil {
		t.Fatal(err)
	}
	if err := m.Ingest(first); err != nil {
		t.Fatal(err)
	}
	edit := first
	edit.EventID = "b"
	edit.Kind = "MESSAGE_UPDATE"
	edit.Payload = json.RawMessage(`{"id":"900","channel_id":"200","content":"closed","edited_timestamp":"` + now.Add(time.Second).Format(time.RFC3339Nano) + `"}`)
	if err := m.Ingest(edit); err != nil {
		t.Fatal(err)
	}
	msg, err := st.DiscordMessage().DeliveryMessage("t", "900", 0)
	if err != nil || msg == nil || msg.Content != "open" || msg.ExecutionKey != "k" {
		t.Fatalf("lost immutable revision %+v %v", msg, err)
	}
	latest, err := st.DiscordMessage().GetByMessageID("200", "900")
	if err != nil || latest.Content != "closed" || latest.AuthorID != "7" {
		t.Fatalf("partial edit failed %+v %v", latest, err)
	}
	count, _ := st.DiscordMessage().PendingDeliveries()
	if count != 2 {
		t.Fatalf("duplicate delivery count %d", count)
	}
	d, _ := st.DiscordMessage().NextDelivery("t")
	if d.EventID != "a" {
		t.Fatal("order")
	}
	if err := st.DiscordMessage().ResetDeliveries(); err != nil {
		t.Fatal(err)
	}
	d, _ = st.DiscordMessage().NextDelivery("t")
	if d.EventID != "a" {
		t.Fatal("restart lost claim")
	}
}
func TestBaselineDeletionAndStaleEditNeverOpen(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC()
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}}}
	ev := sourceEvent("baseline", "MESSAGE_CREATE", "old", now)
	ev.Baseline = true
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	ev.EventID = "delete"
	ev.Kind = "MESSAGE_DELETE"
	if err := m.Ingest(ev); err != nil {
		t.Fatal(err)
	}
	count, _ := st.DiscordMessage().PendingDeliveries()
	if count != 0 {
		t.Fatalf("baseline/delete created actions %d", count)
	}
}
func TestExecutionBoundaryAndSourceGap(t *testing.T) {
	st := sourceStore(t)
	if err := st.DiscordConfig().Save(store.DiscordConfigUpdate{Token: "test"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := st.DiscordConfig().Get()
	m := NewSourceManager(st)
	m.cfg = cfg
	msg := &store.DiscordMessage{ChannelID: "200", ReceivedAt: time.Now().UTC(), MessageTimestamp: time.Now().UTC(), ExecutionKey: "new"}
	if err := m.CheckExecution("t", msg, true); err == nil || err.Error() != "OBSERVE_ONLY" {
		t.Fatal(err)
	}
	if err := st.DiscordConfig().Save(store.DiscordConfigUpdate{RunMode: "live"}); err != nil {
		t.Fatal(err)
	}
	m.cfg, _ = st.DiscordConfig().Get()
	m.subscriptions["t"] = &sourceSubscription{route: Route{ExecutionKey: "new", Channels: []string{"200"}}}
	if err := m.CheckExecution("t", msg, true); err == nil || err.Error() != "BEFORE_EXECUTION_BOUNDARY" {
		t.Fatal(err)
	}
	msg.ReceivedAt = time.Now().UTC()
	msg.MessageTimestamp = msg.ReceivedAt
	m.status = CollectorStatus{State: "connected", LastHeartbeat: time.Now(), DesiredVersion: "v", AppliedVersion: "v"}
	if err := m.CheckExecution("t", msg, true); err == nil {
		t.Fatal("missing channel passed")
	}
	if err := st.DiscordMessage().SaveSourceState(&store.DiscordSourceState{ChannelID: "200", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckExecution("t", msg, true); err != nil {
		t.Fatal(err)
	}
	msg.ExecutionKey = "old"
	if err := m.CheckExecution("t", msg, true); err == nil {
		t.Fatal("revoked config passed")
	}
	if err := m.CheckExecution("t", msg, false); err != nil {
		t.Fatalf("existing exits blocked: %v", err)
	}
}

func TestOldCardNewEditSurvivesRouteReactivationWithoutReopening(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC()
	liveSince := now.Add(-time.Hour)
	m.cfg = &store.DiscordConfig{Enabled: true, Token: "test", RunMode: "live", ExecutionSince: &liveSince}
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}, ExecutionKey: "new", ActivatedAt: now.Add(-time.Second)}}
	old := sourceEvent("original", "MESSAGE_CREATE", "opening", now.Add(-time.Minute))
	if err := m.Ingest(old); err != nil {
		t.Fatal(err)
	}
	edit := old
	edit.EventID, edit.Kind, edit.ReceivedAt = "edited", "MESSAGE_UPDATE", now
	edit.Payload = json.RawMessage(`{"id":"900","channel_id":"200","content":"move stop","edited_timestamp":"` + now.Format(time.RFC3339Nano) + `"}`)
	if err := m.Ingest(edit); err != nil {
		t.Fatal(err)
	}
	delivery, err := st.DiscordMessage().NextDelivery("t")
	if err != nil || delivery == nil || delivery.EventID != "edited" {
		t.Fatalf("lost fresh edit of older card: %+v %v", delivery, err)
	}
	msg, err := st.DiscordMessage().DeliveryMessage("t", "900", 1)
	if err != nil || msg == nil {
		t.Fatal("missing immutable edit", err)
	}
	if err = m.CheckExecution("t", msg, false); err != nil {
		t.Fatal("management blocked by original timestamp", err)
	}
	if err = m.CheckExecution("t", msg, true); err == nil || err.Error() != "BEFORE_EXECUTION_BOUNDARY" {
		t.Fatal("edited historical card allowed to reopen", err)
	}
}

func TestRouteReloadWaitsForOldHandlerAndKeepsFrozenReceipt(t *testing.T) {
	st := sourceStore(t)
	if err := st.DiscordConfig().Save(store.DiscordConfigUpdate{Token: "test"}); err != nil {
		t.Fatal(err)
	}
	m := NewSourceManager(st)
	m.cfg, _ = st.DiscordConfig().Get()
	m.status = CollectorStatus{State: "connected", LastHeartbeat: time.Now()}
	started, release, replaced := make(chan struct{}), make(chan struct{}), make(chan *store.DiscordMessage, 1)
	if err := st.DiscordMessage().SaveSourceState(&store.DiscordSourceState{ChannelID: "200", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	route := Route{TraderID: "t", Channels: []string{"200"}, ExecutionKey: "risk", RulesJSON: "old rules"}
	if err := m.SubscribeRoute(route, func(msg *store.DiscordMessage, _ bool) error {
		close(started)
		<-release
		return fmt.Errorf("engine replaced")
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Ingest(sourceEvent("new", "MESSAGE_CREATE", "open", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not started")
	}
	m.PrepareReload("t", "risk")
	m.UnsubscribeRoute("t")
	route.RulesJSON = "new wording"
	if err := m.SubscribeRoute(route, func(msg *store.DiscordMessage, _ bool) error { replaced <- msg; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replaced:
		t.Fatal("new handler overtook old handler")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case msg := <-replaced:
		if msg.RulesSnapshot != "old rules" || msg.ExecutionKey != "risk:0" {
			t.Fatalf("snapshot changed: %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receipt not resumed")
	}
	m.UnsubscribeRoute("t")
	record, err := st.DiscordMessage().RegisterRoute("t", "risk")
	if err != nil || record.Generation != 1 {
		t.Fatalf("stop did not revoke pending opens: %+v %v", record, err)
	}
	m.Stop()
}

func TestBufferedEventUsesCaptureTimeRulesAfterConfigChange(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC()
	routes := []Route{{TraderID: "t", Channels: []string{"200"}, RulesJSON: "captured", ExecutionKey: "old", ActivatedAt: now.Add(-time.Minute)}}
	raw, _ := json.Marshal(routes)
	if err := st.DiscordMessage().SaveSourceConfig("v1", string(raw)); err != nil {
		t.Fatal(err)
	}
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"300"}, RulesJSON: "current", ExecutionKey: "new"}}
	event := sourceEvent("captured-event", "MESSAGE_CREATE", "open", now)
	event.ConfigVersion = "v1"
	if err := m.Ingest(event); err != nil {
		t.Fatal(err)
	}
	msg, err := st.DiscordMessage().DeliveryMessage("t", "900", 0)
	if err != nil || msg == nil || msg.RulesSnapshot != "captured" || msg.ExecutionKey != "old" {
		t.Fatalf("rules changed in transit: %+v %v", msg, err)
	}
	event.EventID = "unknown"
	event.ConfigVersion = "unknown"
	if m.Ingest(event) == nil {
		t.Fatal("unknown frozen rules accepted")
	}
}

func TestUnixReceiptAcknowledgesOnlyCommittedTransaction(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	dir, err := os.MkdirTemp("/tmp", "joz-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "source.sock")
	t.Setenv("DISCORD_SOCKET_PATH", path)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	now := time.Now().UTC()
	routes, _ := json.Marshal([]Route{{TraderID: "t", Channels: []string{"200"}, ActivatedAt: now.Add(-time.Minute), ExecutionKey: "frozen"}})
	if err := st.DiscordMessage().SaveSourceConfig("test", string(routes)); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	encoder, decoder := json.NewEncoder(c), json.NewDecoder(c)
	event := sourceEvent("receipt", "MESSAGE_CREATE", "new", now)
	event.ConfigVersion = "test"
	raw, _ := json.Marshal(event)
	if err = st.GormDB().Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON discord_deliveries BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err = encoder.Encode(wireFrame{Type: "event", Data: raw}); err != nil {
		t.Fatal(err)
	}
	var ack wireFrame
	if err = decoder.Decode(&ack); err != nil || ack.Error == "" {
		t.Fatalf("failed transaction acknowledged: %+v %v", ack, err)
	}
	var count int64
	st.GormDB().Model(&store.DiscordInbound{}).Count(&count)
	if count != 0 {
		t.Fatal("partial event transaction escaped rollback")
	}
	if err = st.GormDB().Exec("DROP TRIGGER fail_delivery").Error; err != nil {
		t.Fatal(err)
	}
	if err = encoder.Encode(wireFrame{Type: "event", Data: raw}); err != nil {
		t.Fatal(err)
	}
	ack = wireFrame{}
	if err = decoder.Decode(&ack); err != nil || ack.Error != "" || ack.ID != "receipt" {
		t.Fatalf("commit not acknowledged: %+v %v", ack, err)
	}
	msg, err := st.DiscordMessage().DeliveryMessage("t", "900", 0)
	if err != nil || msg == nil || msg.Content != "new" {
		t.Fatal("ACK preceded durable recipient", err)
	}
	if err = encoder.Encode(wireFrame{Type: "event", Data: raw}); err != nil {
		t.Fatal(err)
	}
	if err = decoder.Decode(&ack); err != nil {
		t.Fatal(err)
	}
	queued, _ := st.DiscordMessage().PendingDeliveries()
	if queued != 1 {
		t.Fatal("ACK loss duplicated delivery", queued)
	}
}

func TestReceiptRetentionKeepsPendingAndActiveEvidence(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC()
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}}}
	for _, id := range []string{"done", "pending", "active"} {
		ev := sourceEvent(id, "MESSAGE_CREATE", id, now)
		ev.MessageID = id
		ev.Payload = json.RawMessage(fmt.Sprintf(`{"id":%q,"channel_id":"200","author":{"id":"7"},"content":%q,"timestamp":%q}`, id, id, now.Format(time.RFC3339Nano)))
		if err := m.Ingest(ev); err != nil {
			t.Fatal(err)
		}
		if id != "pending" {
			d, _ := st.DiscordMessage().NextDelivery("t")
			if d.EventID == "pending" {
				st.DiscordMessage().FinishDelivery(d.ID, store.DiscordMsgProcessing, "")
				d, _ = st.DiscordMessage().NextDelivery("t")
			}
			if err := st.DiscordMessage().FinishDelivery(d.ID, store.DiscordMsgDone, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.CopyTrade().CreateContext(&store.CopyTradeContext{ID: "live", TraderID: "t", RootMessageID: "active", State: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	st.GormDB().Model(&store.DiscordInbound{}).Where("1=1").Update("created_at", now.AddDate(0, 0, -100))
	n, err := st.DiscordMessage().CleanOldIngest(90)
	if err != nil || n != 1 {
		t.Fatalf("retention deleted wrong receipts: %d %v", n, err)
	}
	var remaining []store.DiscordInbound
	st.GormDB().Order("event_id").Find(&remaining)
	if len(remaining) != 2 || remaining[0].EventID != "active" || remaining[1].EventID != "pending" {
		t.Fatalf("lost required evidence %+v", remaining)
	}
}

func TestReenableRouteDoesNotTradeMessagesFromStoppedInterval(t *testing.T) {
	st := sourceStore(t)
	old, err := st.DiscordMessage().RegisterRoute("t", "key")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.DiscordMessage().RevokeRoute("t"); err != nil {
		t.Fatal(err)
	}
	whileStopped := time.Now().UTC()
	time.Sleep(time.Millisecond)
	active, err := st.DiscordMessage().RegisterRoute("t", "key")
	if err != nil {
		t.Fatal(err)
	}
	if !active.ActivatedAt.After(whileStopped) || active.Generation != old.Generation+1 {
		t.Fatal("reactivation did not establish fresh boundary")
	}
	m := NewSourceManager(st)
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}, ActivatedAt: active.ActivatedAt, ExecutionKey: "new"}}
	if err = m.Ingest(sourceEvent("backfilled", "MESSAGE_CREATE", "missed while stopped", whileStopped)); err != nil {
		t.Fatal(err)
	}
	count, _ := st.DiscordMessage().PendingDeliveries()
	if count != 0 {
		t.Fatal("disabled-period history became executable", count)
	}
}

func TestFailedRouteRevocationCannotBeBypassedByReenable(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	route := Route{TraderID: "t", Channels: []string{"200"}, ExecutionKey: "risk"}
	if err := m.SubscribeRoute(route, func(*store.DiscordMessage, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := st.GormDB().Exec(`CREATE TRIGGER fail_revoke BEFORE UPDATE OF generation ON discord_routes BEGIN SELECT RAISE(ABORT, 'injected revocation failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	m.UnsubscribeRoute("t")
	if err := m.SubscribeRoute(route, func(*store.DiscordMessage, bool) error { return nil }); err == nil {
		t.Fatal("failed revocation bypassed")
	}
	if err := st.GormDB().Exec("DROP TRIGGER fail_revoke").Error; err != nil {
		t.Fatal(err)
	}
	if err := m.SubscribeRoute(route, func(*store.DiscordMessage, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	record, err := st.DiscordMessage().RegisterRoute("t", "risk")
	if err != nil || record.Generation != 1 {
		t.Fatal("uncommitted revocation lost", err)
	}
}

func TestDiagnosticProtocolACKAndDedup(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	server, client := net.Pipe()
	m.conn = server
	m.wg.Add(1)
	go m.serve(server)
	defer func() { client.Close(); m.wg.Wait() }()
	payload := json.RawMessage(`{"event_id":"diag1","event":"gateway.error","level":"error","context":{"code":"FAILED","token":"hidden"}}`)
	encoder := json.NewEncoder(client)
	decoder := json.NewDecoder(client)
	for i := 0; i < 2; i++ {
		if err := encoder.Encode(wireFrame{Type: "diagnostic", Data: payload}); err != nil {
			t.Fatal(err)
		}
		var ack wireFrame
		if err := decoder.Decode(&ack); err != nil {
			t.Fatal(err)
		}
		if ack.Type != "diagnostic_ack" || ack.ID != "diag1" || ack.Error != "" {
			t.Fatalf("bad ACK: %+v", ack)
		}
	}
	rows, err := st.Logs().ListLogs(store.LogFilter{Scope: "system", UserID: "u", Upper: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ContextJSON != `{"code":"FAILED"}` {
		t.Fatalf("diagnostic not deduped/redacted: %+v", rows)
	}
}
