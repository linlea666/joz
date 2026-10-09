package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"nofx/discord"
	"nofx/store"
	"testing"
	"time"
)

type admissionSource struct{ err error }

func (*admissionSource) SubscribeRoute(discord.Route, discord.MessageHandler) error { return nil }
func (*admissionSource) UnsubscribeRoute(string)                                    {}
func (*admissionSource) Client() *discord.Client                                    { return nil }
func (s *admissionSource) CheckExecution(string, *store.DiscordMessage, bool) error { return s.err }
func (*admissionSource) HistoryComplete(string) bool                                { return true }

func TestEntryRecoveryHonorsFrozenReceiptAndTerminalInbox(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	rules := notificationRules()
	source := &admissionSource{err: fmt.Errorf("SOURCE_NOT_READY")}
	e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg, Source: source})
	root := notificationFixture("added")
	root.ChannelID = "201"
	root.MessageTimestamp = time.Now().Add(-time.Minute)
	root.ReceivedAt = root.MessageTimestamp
	if err := st.DiscordMessage().CommitInbound(&store.DiscordInbound{EventID: "opening", Kind: "MESSAGE_CREATE", ChannelID: root.ChannelID, MessageID: root.MessageID, ReceivedAt: root.ReceivedAt}, root, []store.DiscordDelivery{{TraderID: "t", RulesJSON: rules.Snapshot(), ExecutionKey: "frozen"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "open", TraderID: "t", MessageID: root.MessageID, ChannelID: root.ChannelID, RulesSnapshotJSON: rules.Snapshot(), ExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	row := &store.CopyTradeOrder{SignalID: "open", Symbol: "BTCUSDT", Direction: "SHORT"}
	if err := e.admitEntrySubmit(row); !errors.Is(err, errEntryDeferred) {
		t.Fatal("disconnected source admitted planned entry", err)
	}
	source.err = nil
	if err := e.admitEntrySubmit(row); err != nil {
		t.Fatal(err)
	}
	end := notificationFixture("closed")
	end.MessageID = "ended"
	if err := st.DiscordMessage().CommitInbound(&store.DiscordInbound{EventID: "ended", Kind: "MESSAGE_CREATE", ChannelID: end.ChannelID, MessageID: end.MessageID, ReceivedAt: end.ReceivedAt}, end, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.admitEntrySubmit(row); !errors.Is(err, errEntryRevoked) {
		t.Fatal("recovery bypassed received end", err)
	}
}

func TestDeferredEntryRecoveryDoesNotSubmitAndRestoresAfterReadiness(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	q1, q2, err := splitQuantities(p, &v.rules)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(managedEntryPlan{Version: 1, RiskBudget: p.RiskBudget, StopLoss: p.StopLoss, MaxNotional: p.MaxNotional, MarginBudget: p.AvailableMargin * .9, SignalExpiresAt: p.SignalExpiresAt, Rules: v.rules})
	ctx := &store.CopyTradeContext{ID: "before-submit", TraderID: "trader-1", Symbol: p.Symbol, Direction: string(p.Direction), State: "ENTRY_PENDING", ExecutionVersion: 1, EntryPolicy: EntryPolicySplit, EntryPlanJSON: string(plan), StopLossPrice: p.StopLoss, Leverage: p.Leverage, TPRecipeJSON: planRecipeJSON(p), EntryWorking: true}
	first := x.newManagedOrder(ctx, "open", "ENTRY_1", "MARKET", p.EntryPrice, q1)
	second := x.newManagedOrder(ctx, "open", "ENTRY_2", "LIMIT", p.SplitReference, q2)
	if err = st.CopyTrade().CreateManagedPlan(ctx, []*store.CopyTradeOrder{first, second}); err != nil {
		t.Fatal(err)
	}
	x.beforeEntrySubmit = func(*store.CopyTradeOrder) error { return errEntryDeferred }
	if err = x.reconcileManagedEntry("restart", ctx); !errors.Is(err, errEntryDeferred) {
		t.Fatal(err)
	}
	rows, _ := x.entryOrders(ctx)
	if len(v.calls) != 0 || rows[0].Status != "PLANNED" || rows[1].Status != "PLANNED" {
		t.Fatal("source gate caused unknown submission or unwanted order")
	}
	x.beforeEntrySubmit = func(o *store.CopyTradeOrder) error {
		if o.Role == "ENTRY_2" {
			return errEntryDeferred
		}
		return nil
	}
	if err = x.reconcileManagedEntry("first-ready", ctx); !errors.Is(err, errEntryDeferred) {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != 1 || len(v.openOrders) == 0 || ctx.Quantity <= 0 {
		t.Fatal("first leg protection failed during second-leg wait")
	}
	x.beforeEntrySubmit = nil
	if err = x.reconcileManagedEntry("source-restored", ctx); err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != 2 {
		t.Fatal("recovery repeated or lost an entry")
	}
}
