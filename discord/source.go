package discord

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"nofx/logger"
	"nofx/store"
)

type MessageHandler func(*store.DiscordMessage, bool) error
type Route struct {
	BaseExecutionKey string    `json:"-"`
	TraderID         string    `json:"trader_id"`
	Channels         []string  `json:"channels"`
	RulesJSON        string    `json:"rules_json"`
	ExecutionKey     string    `json:"execution_key"`
	ActivatedAt      time.Time `json:"activated_at"`
}

// Source separates the trading engine from the transport runtime.
type Source interface {
	SubscribeRoute(Route, MessageHandler) error
	UnsubscribeRoute(string)
	Client() *Client
	CheckExecution(string, *store.DiscordMessage, bool) error
	HistoryComplete(string) bool
}
type ChannelStatus = store.DiscordSourceState
type CollectorStatus struct {
	IPCConnected       bool           `json:"ipc_connected"`
	ErrorDetail        map[string]any `json:"error_detail,omitempty"`
	DiagnosticFailures uint64         `json:"diagnostic_failures"`
	State              string         `json:"state"`
	LastError          string         `json:"last_error,omitempty"`
	LastHeartbeat      time.Time      `json:"last_heartbeat,omitempty,omitzero"`
	LastMessage        time.Time      `json:"last_message,omitempty,omitzero"`
	AppliedVersion     string         `json:"applied_version,omitempty"`
	DesiredVersion     string         `json:"desired_version,omitempty"`
	Backlog            int64          `json:"backlog"`
}
type wireFrame struct {
	StatusCode int             `json:"status_code,omitempty"`
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	Op         string          `json:"op,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Error      string          `json:"error,omitempty"`
}
type GatewayEvent struct {
	ConfigVersion string          `json:"config_version"`
	Session       string          `json:"session,omitempty"`
	Sequence      int64           `json:"sequence,omitempty"`
	EventID       string          `json:"event_id"`
	Kind          string          `json:"kind"`
	ChannelID     string          `json:"channel_id"`
	MessageID     string          `json:"message_id"`
	ReceivedAt    time.Time       `json:"received_at"`
	Baseline      bool            `json:"baseline"`
	Payload       json.RawMessage `json:"payload"`
}
type sourceSubscription struct {
	done        chan struct{}
	predecessor <-chan struct{}
	route       Route
	handler     MessageHandler
	stop        chan struct{}
}
type SourceManager struct {
	st                 *store.Store
	mu                 sync.Mutex
	ingestMu           sync.Mutex
	writeMu            sync.Mutex
	configMu           sync.Mutex
	closed             bool
	conn               net.Conn
	listener           net.Listener
	stop               chan struct{}
	draining           map[string]<-chan struct{}
	retainOnReload     map[string]bool
	pendingRevocations map[string]bool
	subscriptions      map[string]*sourceSubscription
	pending            map[string]chan wireFrame
	cfg                *store.DiscordConfig
	storageError       string
	status             CollectorStatus
	serial             atomic.Uint64
	wg                 sync.WaitGroup
}

func NewSourceManager(st *store.Store) *SourceManager {
	return &SourceManager{pendingRevocations: map[string]bool{}, draining: map[string]<-chan struct{}{}, retainOnReload: map[string]bool{}, st: st, subscriptions: map[string]*sourceSubscription{}, pending: map[string]chan wireFrame{}, stop: make(chan struct{}), status: CollectorStatus{State: "disconnected"}}
}
func (m *SourceManager) Start() error {
	m.configMu.Lock()
	defer m.configMu.Unlock()
	if m.listener != nil {
		return nil
	}
	if err := m.st.DiscordMessage().ResetDeliveries(); err != nil {
		return err
	}
	cfg, err := m.st.DiscordConfig().Get()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	path := os.Getenv("DISCORD_SOCKET_PATH")
	if path == "" {
		path = "data/run/discord.sock"
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if _, err = os.Lstat(path); err == nil {
		probe, e := net.DialTimeout("unix", path, time.Second)
		if e == nil {
			probe.Close()
			return fmt.Errorf("collector socket already owned")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err = os.Chmod(path, 0600); err != nil {
		ln.Close()
		return err
	}
	m.listener = ln
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			m.mu.Lock()
			busy := m.conn != nil || m.closed
			if !busy {
				m.conn = c
				m.wg.Add(1)
			}
			m.mu.Unlock()
			if busy {
				c.Close()
				continue
			}
			go m.serve(c)
		}
	}()
	return nil
}
func (m *SourceManager) Stop() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stop)
	if m.conn != nil {
		m.conn.Close()
	}
	for _, sub := range m.subscriptions {
		close(sub.stop)
	}
	m.subscriptions = map[string]*sourceSubscription{}
	m.mu.Unlock()
	if m.listener != nil {
		m.listener.Close()
	}
	m.wg.Wait()
}
func (m *SourceManager) serve(c net.Conn) {
	defer m.wg.Done()
	defer func() {
		c.Close()
		m.mu.Lock()
		if m.conn == c {
			m.conn = nil
			m.status.State = "disconnected"
			m.status.AppliedVersion = ""
			for id, p := range m.pending {
				p <- wireFrame{Error: "collector disconnected"}
				delete(m.pending, id)
			}
		}
		m.mu.Unlock()
	}()
	scanner := bufio.NewScanner(c)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var f wireFrame
		if json.Unmarshal(scanner.Bytes(), &f) != nil {
			return
		}
		switch f.Type {
		case "hello", "refresh":
			if f.Type == "hello" && string(f.Data) != "1" {
				return
			}
			go func() {
				if err := m.ReloadConfig(); err != nil {
					logger.Warnf("[Discord] apply failed: %v", err)
					m.st.Logs().Record("discord", "config.apply.failed", "error", "采集配置应用失败", map[string]any{"code": "CONFIG_APPLY_FAILED", "stage": "configure"})
				}
			}()
		case "response":
			m.mu.Lock()
			p := m.pending[f.ID]
			delete(m.pending, f.ID)
			m.mu.Unlock()
			if p != nil {
				p <- f
			}
		case "event":
			var ev GatewayEvent
			if json.Unmarshal(f.Data, &ev) != nil {
				return
			}
			var err error
			if ev.ConfigVersion == "" {
				err = fmt.Errorf("collector event missing config version")
			} else {
				err = m.Ingest(ev)
			}
			reply := wireFrame{Type: "ack", ID: ev.EventID}
			if err != nil {
				reply.Error = "event commit failed"
				m.mu.Lock()
				m.storageError = err.Error()
				m.mu.Unlock()
			}
			if m.write(reply) != nil {
				return
			}
		case "diagnostic":
			var d struct {
				EventID    string         `json:"event_id"`
				Event      string         `json:"event"`
				Level      string         `json:"level"`
				Context    map[string]any `json:"context"`
				OccurredAt time.Time      `json:"occurred_at"`
			}
			if json.Unmarshal(f.Data, &d) != nil || d.EventID == "" || len(f.Data) > 16384 {
				return
			}
			b, _ := json.Marshal(d.Context)
			channel, _ := d.Context["channel_id"].(string)
			err := m.st.Logs().Append(&store.SystemEvent{EventID: d.EventID, Component: "collector", Event: d.Event, Level: d.Level, Message: d.Event, ContextJSON: string(b), ChannelID: channel, OccurredAt: d.OccurredAt})
			reply := wireFrame{Type: "diagnostic_ack", ID: d.EventID}
			if err != nil {
				reply.Error = "diagnostic commit failed"
			}
			if m.write(reply) != nil {
				return
			}
		case "status":
			var s CollectorStatus
			if json.Unmarshal(f.Data, &s) == nil {
				m.mu.Lock()
				s.DesiredVersion = m.status.DesiredVersion
				s.LastMessage = m.status.LastMessage
				previous := m.status.State
				m.status = s
				m.mu.Unlock()
				if previous != s.State {
					m.st.Logs().Record("discord", "collector.state", "info", "采集连接状态变化", map[string]any{"state": s.State, "previous_state": previous})
				}
			}
		case "channel":
			var s ChannelStatus
			if json.Unmarshal(f.Data, &s) == nil {
				if err := m.st.DiscordMessage().SaveSourceState(&s); err != nil {
					return
				}
			}
		default:
			return
		}
	}
}
func (m *SourceManager) write(f wireFrame) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil {
		return fmt.Errorf("collector disconnected")
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return json.NewEncoder(c).Encode(f)
}
func (m *SourceManager) call(op string, data interface{}, out interface{}) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	id := strconv.FormatUint(m.serial.Add(1), 10)
	p := make(chan wireFrame, 1)
	m.mu.Lock()
	m.pending[id] = p
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.pending, id); m.mu.Unlock() }()
	if err = m.write(wireFrame{Type: "request", ID: id, Op: op, Data: b}); err != nil {
		return err
	}
	select {
	case f := <-p:
		if f.Error != "" {
			if f.StatusCode != 0 {
				return &StatusError{StatusCode: f.StatusCode, Body: f.Error}
			}
			return errors.New(f.Error)
		}
		if out != nil {
			return json.Unmarshal(f.Data, out)
		}
		return nil
	case <-m.stop:
		return fmt.Errorf("source stopped")
	case <-time.After(40 * time.Second):
		return fmt.Errorf("collector request timed out")
	}
}
func (m *SourceManager) ReloadConfig() error {
	m.configMu.Lock()
	defer m.configMu.Unlock()
	cfg, err := m.st.DiscordConfig().Get()
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &store.DiscordConfig{RunMode: "observe"}
	}
	m.mu.Lock()
	m.cfg = cfg
	routes := make([]Route, 0, len(m.subscriptions))
	for _, s := range m.subscriptions {
		routes = append(routes, s.route)
	}
	m.mu.Unlock()
	sort.Slice(routes, func(i, j int) bool { return routes[i].TraderID < routes[j].TraderID })
	b, _ := json.Marshal(struct {
		Revision uint64
		Routes   []Route
	}{cfg.Revision, routes})
	h := sha256.Sum256(b)
	version := hex.EncodeToString(h[:])
	m.mu.Lock()
	m.status.DesiredVersion = version
	m.mu.Unlock()
	frozenRoutes, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	if err = m.st.DiscordMessage().SaveSourceConfig(version, string(frozenRoutes)); err != nil {
		return err
	}
	channels := map[string]interface{}{}
	cards := []map[string]string{}
	for _, r := range routes {
		for _, ch := range r.Channels {
			if _, ok := channels[ch]; !ok {
				last, e := m.st.DiscordMessage().LatestMessageTime(ch)
				if e != nil {
					return e
				}
				channels[ch] = map[string]interface{}{"since": last}
			}
		}
		contexts, e := m.st.CopyTrade().GetActiveContexts(r.TraderID)
		if e != nil {
			return e
		}
		for _, ctx := range contexts {
			cards = append(cards, map[string]string{"channel_id": ctx.ChannelID, "message_id": ctx.RootMessageID})
		}
	}
	states, err := m.st.DiscordMessage().SourceStates()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.status.DesiredVersion = version
	m.mu.Unlock()
	var result struct {
		AppliedVersion string `json:"applied_version"`
	}
	err = m.call("configure", map[string]interface{}{"version": version, "token": string(cfg.Token), "enabled": cfg.Enabled, "channels": channels, "checkpoints": states, "active_cards": cards, "diagnostics_v1": true}, &result)
	if err != nil {
		return err
	}
	if result.AppliedVersion != version {
		return fmt.Errorf("collector configuration not acknowledged")
	}
	m.mu.Lock()
	m.status.AppliedVersion = version
	m.mu.Unlock()
	m.st.Logs().Record("discord", "config.applied", "info", "采集路由配置已确认应用", map[string]any{"config_version": version})
	return nil
}
func (m *SourceManager) SubscribeRoute(r Route, h MessageHandler) error {
	// A failed stop-generation write must be completed before any reactivation.
	m.mu.Lock()
	if m.pendingRevocations[r.TraderID] {
		if err := m.st.DiscordMessage().RevokeRoute(r.TraderID); err != nil {
			m.mu.Unlock()
			return err
		}
		delete(m.pendingRevocations, r.TraderID)
	}
	m.mu.Unlock()

	record, err := m.st.DiscordMessage().RegisterRoute(r.TraderID, r.ExecutionKey)
	if err != nil {
		return err
	}
	r.ActivatedAt = record.ActivatedAt
	r.BaseExecutionKey = r.ExecutionKey
	r.ExecutionKey = fmt.Sprintf("%s:%d", r.ExecutionKey, record.Generation)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("source stopped")
	}
	m.wg.Add(1)
	predecessor := m.draining[r.TraderID]
	delete(m.draining, r.TraderID)
	if old := m.subscriptions[r.TraderID]; old != nil {
		close(old.stop)
		predecessor = old.done
	}
	s := &sourceSubscription{route: r, handler: h, stop: make(chan struct{}), done: make(chan struct{}), predecessor: predecessor}
	m.subscriptions[r.TraderID] = s
	m.mu.Unlock()
	go m.dispatch(s)
	// An unavailable source must not prevent protection/reconciliation starting.
	go func() {
		if err := m.ReloadConfig(); err != nil {
			logger.Warnf("[Discord] subscription pending: %v", err)
		}
	}()
	return nil
}
func (m *SourceManager) UnsubscribeRoute(id string) {
	m.mu.Lock()
	if s := m.subscriptions[id]; s != nil {
		if !m.retainOnReload[id] {
			if err := m.st.DiscordMessage().RevokeRoute(id); err != nil {
				m.storageError = "route revocation failed"
				m.pendingRevocations[id] = true
			}
		}
		delete(m.retainOnReload, id)
		m.draining[id] = s.done
		close(s.stop)
		delete(m.subscriptions, id)
	}
	m.mu.Unlock()
	go func() { _ = m.ReloadConfig() }()
}
func (m *SourceManager) dispatch(s *sourceSubscription) {
	defer m.wg.Done()
	defer close(s.done)
	if s.predecessor != nil {
		select {
		case <-s.predecessor:
		case <-s.stop:
			return
		case <-m.stop:
			return
		}
	}
	for {
		if err := m.st.DiscordMessage().ResetTraderDeliveries(s.route.TraderID); err == nil {
			break
		}
		m.mu.Lock()
		m.storageError = "delivery recovery failed"
		m.mu.Unlock()
		select {
		case <-s.stop:
			return
		case <-m.stop:
			return
		case <-time.After(time.Second):
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-m.stop:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		enabled := m.cfg != nil && m.cfg.Enabled && m.cfg.Token != ""
		current := m.subscriptions[s.route.TraderID] == s
		connected := m.status.State == "connected" && time.Since(m.status.LastHeartbeat) <= 90*time.Second
		m.mu.Unlock()
		if !current {
			return
		}
		if !enabled || !connected {
			continue
		}
		states, serr := m.st.DiscordMessage().SourceStates()
		if serr != nil {
			continue
		}
		ready := true
		for _, ch := range s.route.Channels {
			found := false
			for _, state := range states {
				if state.ChannelID == ch {
					found = true
					if state.State == "recovering" || state.State == "baseline" {
						ready = false
					}
				}
			}
			if !found {
				ready = false
			}
		}
		if !ready {
			continue
		}
		d, err := m.st.DiscordMessage().NextDelivery(s.route.TraderID)
		if err != nil || d == nil {
			if err != nil {
				m.mu.Lock()
				m.storageError = "delivery read failed"
				m.mu.Unlock()
			}
			continue
		}
		var msg store.DiscordMessage
		err = json.Unmarshal([]byte(d.MessageJSON), &msg)
		// Best-effort audit cannot alter the durable delivery result.
		b, _ := json.Marshal(map[string]any{"delivery_id": d.ID, "source_event_id": d.EventID, "stage": "dispatch"})
		if err := m.st.CopyTrade().AppendEvent(&store.CopyTradeEvent{TraderID: s.route.TraderID, TraceID: "source-" + d.EventID, SourceEventID: d.EventID, DeliveryID: d.ID, ChannelID: msg.ChannelID, MessageID: msg.MessageID, Level: "info", Event: "source.delivery.started", Message: "开始投递不可变消息修订", ContextJSON: string(b)}); err != nil {
			m.st.Logs().NoteFailure()
			m.st.Logs().Record("storage", "trade.audit.failed", "error", "交易事件记录失败", map[string]any{"code": "TRADE_AUDIT_FAILED"})
		}
		if err == nil {
			msg.DeliveryID = d.ID
			msg.SourceEventID = d.EventID
			msg.RulesSnapshot = d.RulesJSON
			msg.ExecutionKey = d.ExecutionKey
			err = s.handler(&msg, msg.Revision > 0)
		}
		status, detail := store.DiscordMsgDone, ""
		if err != nil {
			status, detail = store.DiscordMsgPending, err.Error()
		}
		for {
			err = m.st.DiscordMessage().FinishDelivery(d.ID, status, detail)
			if err == nil {
				m.mu.Lock()
				m.storageError = ""
				m.mu.Unlock()
				break
			}
			m.mu.Lock()
			m.storageError = "delivery outcome commit failed"
			m.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-m.stop:
				return
			case <-time.After(time.Second):
			}
		}
	}
}
func (m *SourceManager) Client() *Client { return &Client{rpc: m.call} }
func (m *SourceManager) Status() []ChannelStatus {
	m.mu.Lock()
	channels := map[string]bool{}
	for _, sub := range m.subscriptions {
		for _, ch := range sub.route.Channels {
			channels[ch] = true
		}
	}
	m.mu.Unlock()
	rows, _ := m.st.DiscordMessage().SourceStates()
	active := make([]ChannelStatus, 0, len(rows))
	for _, row := range rows {
		if channels[row.ChannelID] {
			active = append(active, row)
		}
	}
	return active
}
func (m *SourceManager) CollectorStatus() CollectorStatus {
	m.mu.Lock()
	s := m.status
	s.IPCConnected = m.conn != nil
	s.DiagnosticFailures += m.st.Logs().FailureCount()
	if m.storageError != "" {
		s.State = "storage_failed"
		s.LastError = m.storageError
	}
	m.mu.Unlock()
	if s.State == "connected" && time.Since(s.LastHeartbeat) > 90*time.Second {
		s.State = "stale"
	}
	n, err := m.st.DiscordMessage().PendingDeliveries()
	s.Backlog += n
	if err != nil {
		s.State = "storage_failed"
	}
	return s
}
func (m *SourceManager) CheckExecution(traderID string, msg *store.DiscordMessage, newRisk bool) error {
	m.mu.Lock()
	cfg := m.cfg
	s := m.subscriptions[traderID]
	m.mu.Unlock()
	if cfg == nil || !cfg.Enabled || cfg.Token == "" {
		return fmt.Errorf("SOURCE_DISABLED")
	}
	if cfg.RunMode != "live" || cfg.ExecutionSince == nil {
		return fmt.Errorf("OBSERVE_ONLY")
	}
	ref := msg.MessageTimestamp
	if !newRisk && msg.EditedAt != nil && msg.EditedAt.After(ref) {
		ref = *msg.EditedAt
	}
	if !msg.ReceivedAt.After(*cfg.ExecutionSince) || ref.Before(*cfg.ExecutionSince) {
		return fmt.Errorf("BEFORE_EXECUTION_BOUNDARY")
	}
	if s == nil {
		return fmt.Errorf("CONFIG_REVOKED")
	}
	if newRisk {
		if msg.MessageTimestamp.Before(s.route.ActivatedAt) {
			return fmt.Errorf("BEFORE_EXECUTION_BOUNDARY")
		}
		if msg.ExecutionKey != s.route.ExecutionKey {
			return fmt.Errorf("CONFIG_REVOKED")
		}
		status := m.CollectorStatus()
		if status.State != "connected" || status.AppliedVersion != status.DesiredVersion {
			return fmt.Errorf("SOURCE_NOT_READY")
		}
		if !m.HistoryComplete(traderID) {
			return fmt.Errorf("SOURCE_GAP_OR_PERMISSION")
		}

	}
	return nil
}
func (m *SourceManager) Ingest(ev GatewayEvent) error {
	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()
	if ev.EventID == "" || ev.ChannelID == "" || ev.MessageID == "" {
		return fmt.Errorf("invalid event identity")
	}
	switch ev.Kind {
	case "MESSAGE_CREATE", "MESSAGE_UPDATE", "MESSAGE_DELETE", "MESSAGE_DELETE_BULK":
	default:
		return fmt.Errorf("unsupported event kind")
	}
	var msg *store.DiscordMessage
	if ev.Kind == "MESSAGE_CREATE" || ev.Kind == "MESSAGE_UPDATE" {
		old, err := m.st.DiscordMessage().GetByMessageID(ev.ChannelID, ev.MessageID)
		if err != nil {
			return err
		}
		payload := map[string]json.RawMessage{}
		if old != nil && old.RawPayload != "" {
			if err = json.Unmarshal([]byte(old.RawPayload), &payload); err != nil {
				return err
			}
		}
		var patch map[string]json.RawMessage
		if err = json.Unmarshal(ev.Payload, &patch); err != nil {
			return err
		}
		// Do not allow replayed older updates or CREATE to roll back newer content.
		if old != nil && old.EditedAt != nil {
			var edit time.Time
			_ = json.Unmarshal(patch["edited_timestamp"], &edit)
			if ev.Kind == "MESSAGE_CREATE" || (!edit.IsZero() && edit.Before(*old.EditedAt)) {
				ev.Baseline = true
				patch = map[string]json.RawMessage{}
			}
		}
		for k, v := range patch {
			payload[k] = v
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var apiMsg Message
		if err = json.Unmarshal(b, &apiMsg); err != nil {
			return err
		}
		if apiMsg.ID != ev.MessageID || apiMsg.ChannelID != ev.ChannelID {
			return fmt.Errorf("message identity mismatch")
		}
		if apiMsg.Timestamp.IsZero() || apiMsg.Author.ID == "" {
			return fmt.Errorf("partial event requires hydrated message")
		}
		msg, err = ToStoreMessage(&apiMsg, ev.ChannelID)
		if err != nil {
			return err
		}
		msg.ReceivedAt = ev.ReceivedAt
		msg.RawPayload = string(b)
	}
	if ev.ReceivedAt.IsZero() {
		return fmt.Errorf("event timestamp required")
	}
	recipients := []store.DiscordDelivery{}
	var routes []Route
	if ev.ConfigVersion != "" {
		raw, err := m.st.DiscordMessage().SourceConfig(ev.ConfigVersion)
		if err != nil {
			return fmt.Errorf("frozen collection rules unavailable: %w", err)
		}
		if err = json.Unmarshal([]byte(raw), &routes); err != nil {
			return err
		}
	} else {
		m.mu.Lock()
		for _, sub := range m.subscriptions {
			routes = append(routes, sub.route)
		}
		m.mu.Unlock()
	}
	for _, route := range routes {
		var sourceTime time.Time
		if msg != nil {
			sourceTime = msg.MessageTimestamp
			if msg.EditedAt != nil && msg.EditedAt.After(sourceTime) {
				sourceTime = *msg.EditedAt
			}
		}
		for _, ch := range route.Channels {
			// New management edits of an older active card remain deliverable.
			// CheckExecution still uses the original post time for new risk.
			if ch == ev.ChannelID && msg != nil && !sourceTime.Before(route.ActivatedAt) {
				recipients = append(recipients, store.DiscordDelivery{TraderID: route.TraderID, RulesJSON: route.RulesJSON, ExecutionKey: route.ExecutionKey})
				break
			}
		}
	}
	event := &store.DiscordInbound{ConfigVersion: ev.ConfigVersion, Session: ev.Session, Sequence: ev.Sequence, EventID: ev.EventID, Kind: ev.Kind, ChannelID: ev.ChannelID, MessageID: ev.MessageID, ReceivedAt: ev.ReceivedAt, Payload: string(ev.Payload), Baseline: ev.Baseline}
	if err := m.st.DiscordMessage().CommitInbound(event, msg, recipients); err != nil {
		return err
	}
	m.mu.Lock()
	m.storageError = ""
	m.status.LastMessage = ev.ReceivedAt
	m.mu.Unlock()
	if event.ID > 0 && !event.Baseline {
		var ds []store.DiscordDelivery
		if err := m.st.GormDB().Where("event_id = ?", ev.EventID).Find(&ds).Error; err == nil {
			for _, d := range ds {
				b, _ := json.Marshal(map[string]any{"source_event_id": ev.EventID, "revision": event.Revision, "stage": "receipt", "config_version": ev.ConfigVersion})
				if err := m.st.CopyTrade().AppendEvent(&store.CopyTradeEvent{SourceEventID: ev.EventID, DeliveryID: d.ID, TraderID: d.TraderID, ChannelID: ev.ChannelID, MessageID: ev.MessageID, TraceID: "source-" + ev.EventID, Level: "info", Event: "source.received", Message: "采集事件已持久化，等待交易员投递", ContextJSON: string(b), OccurredAt: ev.ReceivedAt}); err != nil {
					m.st.Logs().NoteFailure()
					m.st.Logs().Record("storage", "trade.audit.failed", "error", "交易事件记录失败", map[string]any{"code": "TRADE_AUDIT_FAILED"})
				}
			}
		} else {
			m.st.Logs().NoteFailure()
			m.st.Logs().Record("storage", "trade.audit.failed", "error", "投递审计读取失败", map[string]any{"code": "TRADE_AUDIT_READ_FAILED"})
		}
	}
	return nil
}

func (m *SourceManager) RouteApplied(traderID string) bool {
	m.mu.Lock()
	exists := m.subscriptions[traderID] != nil
	m.mu.Unlock()
	status := m.CollectorStatus()
	return exists && status.AppliedVersion != "" && status.AppliedVersion == status.DesiredVersion && status.State == "connected"
}

// Called only by the configuration-save path before replacing an engine.
// Wording-only reloads preserve receipt identity and frozen interpretation.
func (m *SourceManager) PrepareReload(traderID, executionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.subscriptions[traderID]; s != nil && s.route.BaseExecutionKey == executionKey {
		m.retainOnReload[traderID] = true
	}
}

func (m *SourceManager) HistoryComplete(traderID string) bool {
	m.mu.Lock()
	sub := m.subscriptions[traderID]
	m.mu.Unlock()
	if sub == nil {
		return false
	}
	states, err := m.st.DiscordMessage().SourceStates()
	if err != nil {
		return false
	}
	if len(sub.route.Channels) == 0 {
		return false
	}
	for _, ch := range sub.route.Channels {
		ready := false
		for _, state := range states {
			if state.ChannelID == ch && state.State == "ready" {
				ready = true
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

// Preserve route generations during a process shutdown (manual Stop still revokes).
func (m *SourceManager) PrepareShutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.subscriptions {
		m.retainOnReload[id] = true
	}
}
