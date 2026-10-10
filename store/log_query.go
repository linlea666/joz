package store

import (
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"strings"
	"time"
)

type LogFilter struct {
	Scope, UserID, TraderID, ChannelID, Level, Component, Event, Correlation, Search string
	Start, End                                                                       time.Time
	Before, Upper                                                                    int64
	Limit                                                                            int
}
type LogEntry struct {
	ID               int64     `json:"id"`
	Scope            string    `json:"scope"`
	EventID          string    `json:"event_id,omitempty"`
	Component        string    `json:"component"`
	Level            string    `json:"level"`
	Event            string    `json:"event"`
	Message          string    `json:"message"`
	ContextJSON      string    `json:"context_json"`
	TraderID         string    `json:"trader_id,omitempty"`
	ChannelID        string    `json:"channel_id,omitempty"`
	LogicalChannelID string    `json:"logical_channel_id,omitempty"`
	MessageID        string    `json:"message_id,omitempty"`
	SignalID         string    `json:"signal_id,omitempty"`
	TraceID          string    `json:"trace_id,omitempty"`
	SourceEventID    string    `json:"source_event_id,omitempty"`
	DeliveryID       int64     `json:"delivery_id,omitempty"`
	DurationMs       int64     `json:"duration_ms"`
	OccurredAt       time.Time `json:"occurred_at"`
}

func (s *LogStore) query(f LogFilter) *gorm.DB {
	var q *gorm.DB
	if f.Scope == "system" {
		q = s.visible(s.db.Model(&SystemEvent{}), f.UserID)
	} else {
		q = s.db.Model(&CopyTradeEvent{}).Where("trader_id IN (SELECT id FROM traders WHERE user_id = ?)", f.UserID)
	}
	for _, p := range []struct{ k, v string }{{"trader_id", f.TraderID}, {"channel_id", f.ChannelID}, {"level", f.Level}, {"event", f.Event}} {
		if p.v != "" {
			if p.k == "channel_id" && f.Scope == "trade" {
				q = q.Where("(channel_id = ? OR signal_id IN (SELECT id FROM copytrade_signals WHERE channel_id = ?))", p.v, p.v)
			} else {
				q = q.Where(p.k+" = ?", p.v)
			}
		}
	}
	if f.Component != "" {
		if f.Scope == "system" {
			q = q.Where("component = ?", f.Component)
		} else {
			q = q.Where("event LIKE ?", f.Component+".%")
		}
	}
	if f.Correlation != "" {
		if f.Scope == "system" {
			q = q.Where("event_id = ?", f.Correlation)
		} else {
			q = q.Where(`(source_event_id = ? OR trace_id = ? OR signal_id = ? OR message_id = ? OR signal_id IN (SELECT id FROM copytrade_signals WHERE source_event_id = ? OR trade_context_id = ?) OR signal_id IN (SELECT signal_id FROM copytrade_actions WHERE id = ? OR context_id = ?) OR signal_id IN (SELECT signal_id FROM copytrade_orders WHERE id = ? OR order_id = ?))`, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation, f.Correlation)
		}
	}
	if f.Search != "" {
		q = q.Where("message LIKE ? ESCAPE '!'", "%"+strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(f.Search)+"%")
	}
	if !f.Start.IsZero() {
		q = q.Where("occurred_at >= ?", f.Start)
	}
	if !f.End.IsZero() {
		q = q.Where("occurred_at <= ?", f.End)
	}
	if f.Before > 0 {
		q = q.Where("id < ?", f.Before)
	}
	if f.Upper >= 0 {
		q = q.Where("id <= ?", f.Upper)
	}
	return q
}
func (s *LogStore) LogBoundary(f LogFilter) (int64, int64, error) {
	f.Before = 0
	f.Upper = -1
	var upper int64
	if err := s.query(f).Select("coalesce(max(id),0)").Scan(&upper).Error; err != nil {
		return 0, 0, err
	}
	f.Upper = upper
	var n int64
	err := s.query(f).Count(&n).Error
	return upper, n, err
}
func (s *LogStore) ListLogs(f LogFilter) ([]LogEntry, error) {
	if f.Limit < 1 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows := []LogEntry{}
	if f.Scope == "system" {
		var items []SystemEvent
		if err := s.query(f).Order("id DESC").Limit(f.Limit).Find(&items).Error; err != nil {
			return nil, err
		}
		for _, e := range items {
			rows = append(rows, LogEntry{ID: e.ID, Scope: "system", EventID: e.EventID, Component: e.Component, Level: e.Level, Event: e.Event, Message: e.Message, ContextJSON: e.ContextJSON, TraderID: e.TraderID, ChannelID: e.ChannelID, OccurredAt: e.OccurredAt})
		}
		return rows, nil
	}
	var items []CopyTradeEvent
	if err := s.query(f).Order("id DESC").Limit(f.Limit).Find(&items).Error; err != nil {
		return nil, err
	}
	ids := []string{}
	for _, e := range items {
		if e.SignalID != "" {
			ids = append(ids, e.SignalID)
		}
	}
	var signals []CopyTradeSignal
	if len(ids) > 0 {
		if err := s.db.Where("id IN ? AND trader_id IN (SELECT id FROM traders WHERE user_id = ?)", ids, f.UserID).Find(&signals).Error; err != nil {
			return nil, err
		}
	}
	index := map[string]CopyTradeSignal{}
	for _, sig := range signals {
		index[sig.ID] = sig
	}
	for _, e := range items {
		row := LogEntry{ID: e.ID, Scope: "trade", SourceEventID: e.SourceEventID, DeliveryID: e.DeliveryID, LogicalChannelID: e.LogicalChannelID, Component: strings.Split(e.Event, ".")[0], Level: e.Level, Event: e.Event, Message: RedactLogText(e.Message), ContextJSON: SanitizeLogJSON(e.ContextJSON), TraderID: e.TraderID, ChannelID: e.ChannelID, MessageID: e.MessageID, SignalID: e.SignalID, TraceID: e.TraceID, DurationMs: e.DurationMs, OccurredAt: e.OccurredAt}
		if sig, ok := index[e.SignalID]; ok {
			row.SourceEventID = sig.SourceEventID
			row.DeliveryID = sig.DeliveryID
			row.LogicalChannelID = sig.LogicalChannelID
			row.ChannelID = sig.ChannelID
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Trace always starts from an owned record. No global symbol/price guessing.
func (s *LogStore) Trace(user, source, signal, context string) (map[string]any, error) {
	own := func(q *gorm.DB) *gorm.DB {
		return q.Where("trader_id IN (SELECT id FROM traders WHERE user_id = ?)", user)
	}
	sigs := []CopyTradeSignal{}
	q := own(s.db.Model(&CopyTradeSignal{}))
	switch {
	case signal != "":
		q = q.Where("id = ?", signal)
	case source != "":
		q = q.Where("source_event_id = ?", source)
	case context != "":
		q = q.Where("trade_context_id = ? OR id IN (SELECT signal_id FROM copytrade_actions WHERE context_id = ?)", context, context)
	default:
		return nil, fmt.Errorf("trace identifier required")
	}
	if err := q.Limit(501).Find(&sigs).Error; err != nil {
		return nil, err
	}
	truncated := len(sigs) > 500
	if truncated {
		sigs = sigs[:500]
	}
	deliveries := []DiscordDelivery{}
	if source != "" {
		if err := own(s.db).Where("event_id = ?", source).Limit(501).Find(&deliveries).Error; err != nil {
			return nil, err
		}
	}
	contexts := []CopyTradeContext{}
	contextIDs, signalIDs, eventIDs, aiIDs := []string{}, []string{}, []string{}, []int64{}
	if source != "" {
		eventIDs = append(eventIDs, source)
	}
	for _, sig := range sigs {
		signalIDs = append(signalIDs, sig.ID)
		if sig.TradeContextID != "" {
			contextIDs = append(contextIDs, sig.TradeContextID)
		}
		if sig.SourceEventID != "" {
			eventIDs = append(eventIDs, sig.SourceEventID)
		}
		if sig.AIRunID > 0 {
			aiIDs = append(aiIDs, sig.AIRunID)
		}
	}
	if context != "" {
		contextIDs = append(contextIDs, context)
	}
	actions := []CopyTradeAction{}
	orders := []CopyTradeOrder{}
	events := []CopyTradeEvent{}
	runs := []CopyTradeAIRun{}
	inbound := []DiscordInbound{}
	if len(signalIDs) > 0 {
		var linked []CopyTradeAction
		if err := own(s.db).Where("signal_id IN ?", signalIDs).Limit(501).Find(&linked).Error; err != nil {
			return nil, err
		}
		for _, a := range linked {
			if a.ContextID != "" {
				contextIDs = append(contextIDs, a.ContextID)
			}
		}
	}
	if len(contextIDs) > 0 {
		if err := own(s.db).Where("id IN ?", contextIDs).Limit(501).Find(&contexts).Error; err != nil {
			return nil, err
		}
	}
	if len(sigs) == 0 && len(deliveries) == 0 && len(contexts) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	// All secondary ledger queries repeat ownership filters.
	queries := []struct {
		q    *gorm.DB
		dest any
	}{
		{own(s.db).Where("signal_id IN ? OR context_id IN ?", signalIDs, contextIDs), &actions},
		{own(s.db).Where("signal_id IN ? OR context_id IN ?", signalIDs, contextIDs), &orders},
		{own(s.db).Where("signal_id IN ? OR trace_id IN ? OR source_event_id IN ?", signalIDs, reconcileTraces(contextIDs), eventIDs), &events},
		{own(s.db).Where("id IN ?", aiIDs), &runs},
	}
	for _, item := range queries {
		if err := item.q.Order("created_at ASC, id ASC").Limit(501).Find(item.dest).Error; err != nil {
			return nil, err
		}
	}
	if len(deliveries) == 0 && len(eventIDs) > 0 {
		if err := own(s.db).Where("event_id IN ?", eventIDs).Limit(501).Find(&deliveries).Error; err != nil {
			return nil, err
		}
	}
	for _, d := range deliveries {
		eventIDs = append(eventIDs, d.EventID)
	}
	if len(eventIDs) > 0 {
		if err := s.db.Where("event_id IN ?", eventIDs).Limit(501).Find(&inbound).Error; err != nil {
			return nil, err
		}
	}
	// Raw event payload/session is intentionally omitted. MessageJSON is immutable evidence.
	sources := []map[string]any{}
	for _, e := range inbound {
		sources = append(sources, map[string]any{"event_id": e.EventID, "kind": e.Kind, "channel_id": e.ChannelID, "message_id": e.MessageID, "revision": e.Revision, "received_at": e.ReceivedAt, "baseline": e.Baseline, "config_version": e.ConfigVersion, "message_json": e.MessageJSON})
	}
	truncated = truncated || len(actions) > 500 || len(orders) > 500 || len(events) > 500 || len(runs) > 500 || len(inbound) > 500 || len(deliveries) > 500
	result := map[string]any{"sources": sources, "deliveries": deliveries, "signals": sigs, "contexts": contexts, "actions": actions, "orders": orders, "events": events, "ai_runs": runs, "truncated": truncated, "evidence_note": "仅展示持久化证据；空集合表示未产生、未关联或已过保留期，不能证明没有执行。"}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var safe map[string]any
	err = json.Unmarshal([]byte(SanitizeLogJSON(string(b))), &safe)
	return safe, err
}
func reconcileTraces(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, "reconcile-"+id)
	}
	return out
}
