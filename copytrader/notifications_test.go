package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"nofx/store"
	"strings"
	"testing"
	"time"
)

func TestNotificationUpdatedCardUsesReferenceBeforeOldPrices(t *testing.T) {
	for _, action := range []Action{ActionUpdateSL, ActionUpdateTP} {
		t.Run(string(action), func(t *testing.T) {
			st := newTestStore(t)
			cfg := DefaultCopyTradingConfig()
			e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
			root := notificationFixture("added")
			root.MessageID, root.ChannelID = "opening", "201"
			root.MessageTimestamp = time.Now().Add(-time.Minute)
			if _, err := st.DiscordMessage().Upsert(root); err != nil {
				t.Fatal(err)
			}
			ctx := &store.CopyTradeContext{ID: "target", TraderID: "t", RootMessageID: root.MessageID, ChannelID: root.ChannelID, Symbol: "BTCUSDT", Direction: "SHORT", State: "OPEN"}
			if err := st.CopyTrade().CreateContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "open", TraderID: "t", ChannelID: root.ChannelID, MessageID: root.MessageID, MessageTimestamp: root.MessageTimestamp, Action: "OPEN", Classification: "SIGNAL", Symbol: "BTCUSDT", Direction: "SHORT", Status: store.SignalStatusExecuted, TradeContextID: ctx.ID}); err != nil {
				t.Fatal(err)
			}
			update := notificationFixture("updated")
			update.ReplyToMessageID = root.MessageID
			if action == ActionUpdateSL {
				update.EmbedsJSON = strings.ReplaceAll(update.EmbedsJSON, "82629.6", "82500.0")
			} else {
				update.EmbedsJSON = strings.ReplaceAll(update.EmbedsJSON, "49242.7", "50000.0")
			}
			ins := &SourceInterpretation{Action: action, Symbol: "BTCUSDT", Direction: DirectionShort}
			got, _, err := e.notificationTarget(update, ins, notificationRules())
			if err != nil || got == nil || got.ID != ctx.ID {
				t.Fatalf("changed card lost explicit target: %+v %v", got, err)
			}
			update.ReplyToMessageID = "another-original-card"
			if _, _, err := e.notificationTarget(update, ins, notificationRules()); err == nil {
				t.Fatal("conflicting reference fell back to symbol")
			}
			update.ReplyToMessageID = ""
			got, _, err = e.notificationTarget(update, ins, notificationRules())
			if err != nil || got == nil || got.ID != ctx.ID {
				t.Fatalf("unique history fallback lost: %+v %v", got, err)
			}
		})
	}
}

func TestTerminalInboxDoesNotCancelAnotherOriginalCard(t *testing.T) {
	for _, reference := range []string{"other", "opening", ""} {
		t.Run("reference_"+reference, func(t *testing.T) {
			st := newTestStore(t)
			cfg := DefaultCopyTradingConfig()
			e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
			open := notificationFixture("added")
			open.MessageID = "opening"
			open.ReceivedAt = time.Now().Add(-time.Minute)
			open.MessageTimestamp = open.ReceivedAt
			end := notificationFixture("closed")
			end.MessageID, end.ReplyToMessageID = "end", reference
			end.EmbedsJSON = strings.ReplaceAll(end.EmbedsJSON, "82629.6", "82500.0")
			if err := st.DiscordMessage().CommitInbound(&store.DiscordInbound{EventID: "end", Kind: "MESSAGE_CREATE", ChannelID: end.ChannelID, MessageID: end.MessageID, ReceivedAt: end.ReceivedAt}, end, nil); err != nil {
				t.Fatal(err)
			}
			err := e.notificationOpenGate(open, InterpretChromaNotification(open, notificationRules()), notificationRules())
			switch reference {
			case "other":
				if err != nil {
					t.Fatal("another card revoked opening", err)
				}
			case "opening":
				if !errors.Is(err, errNotificationEnded) {
					t.Fatal("referenced end not applied", err)
				}
			default:
				if !errors.Is(err, errNotificationPending) {
					t.Fatal("unresolved ending guessed as matched", err)
				}
			}
		})
	}
}

func notificationFixture(kind string) *store.DiscordMessage {
	return &store.DiscordMessage{ChannelID: "202", MessageID: "900", AuthorID: "77", AuthorName: "JONZi", Content: "**🔴 BTC/USDT short was " + kind + "**\n-# **CC:**", EmbedsJSON: `[{"title":"Trade settings","description":"Entry: 82463.5 (LIMIT)\nTP: 49242.7\nStop/loss: 82629.6"}]`, MessageTimestamp: time.Now().UTC(), ReceivedAt: time.Now().UTC()}
}
func notificationRules() MessageRules {
	c := DefaultCopyTradingConfig()
	c.SourceMode = "chroma"
	c.SourceChannelIDs = []string{"201", "202"}
	c.SourceAuthorNames = []string{"jonzi"}
	c.InterpretationProfile = "jonzi_v1"
	return c.MessageRules()
}
func TestNotificationFullInterpretationChain(t *testing.T) {
	for _, kind := range []string{"added", "closed"} {
		t.Run(kind, func(t *testing.T) {
			msg := notificationFixture(kind)
			rules := notificationRules()
			st := newTestStore(t)
			cfg := DefaultCopyTradingConfig()
			cfg.SourceMode = "chroma"
			cfg.InterpretationProfile = rules.Profile
			e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
			msg.RulesSnapshot = rules.Snapshot()
			segments := e.messageSources(msg, rules.Profile)
			ins := InterpretChromaNotification(msg, rules)
			if ins == nil {
				t.Fatal("missing deterministic result")
			}
			b, _ := json.Marshal(ins)
			parsed, err := ParseInterpretation(string(b))
			if err != nil {
				t.Fatal(err)
			}
			parsed = ApplyNotificationPolicy(parsed, msg, rules)
			parsed = ApplySourcePolicy(parsed, msg, segments, rules.Profile)
			parsed = ApplyMessageRules(parsed, msg, segments, rules)
			if !parsed.IsActionable() {
				t.Fatalf("not actionable: %+v", parsed)
			}
			if skip, detail, err := ValidateActionEvidenceDetailed(parsed, segments); skip != SkipNone || err != nil {
				t.Fatalf("evidence: %s %s %v %+v", skip, detail, err, parsed)
			}
			if kind == "added" && (parsed.EntryOrders[0].Price.Price != 82463.5 || parsed.EntryOrders[0].OrderType != EntryLimit) {
				t.Fatalf("bad plan: %+v", parsed)
			}
			if kind == "closed" && parsed.Action != ActionClose {
				t.Fatal(parsed.Action)
			}
		})
	}
}
func TestNotificationGuardCannotOpenFromClosedOrQuotedCard(t *testing.T) {
	for _, body := range []string{"**🔴 BTC/USDT short was closed**", "> BTC/USDT short was added", "profit +5R", "BTC/USDT long was added"} {
		msg := notificationFixture("added")
		msg.Content = body
		ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "BTC", Direction: DirectionShort}
		got := ApplyNotificationPolicy(ins, msg, notificationRules())
		if got.IsActionable() {
			t.Fatalf("accepted %q", body)
		}
	}
}
func TestNotificationTargetIncludesFailedOpenAndNeverSelectsOlderActive(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	rules := notificationRules()
	e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
	now := time.Now().UTC()
	msg := notificationFixture("closed")
	msg.MessageTimestamp = now
	for i, id := range []string{"old", "new"} {
		root := notificationFixture("added")
		root.MessageID = id
		root.ChannelID = "201"
		root.MessageTimestamp = now.Add(-time.Duration(2-i) * time.Minute)
		if _, err := st.DiscordMessage().Upsert(root); err != nil {
			t.Fatal(err)
		}
		sig := &store.CopyTradeSignal{ID: id, TraderID: "t", ChannelID: "201", MessageID: id, Action: "OPEN", Classification: "SIGNAL", Symbol: "BTCUSDT", Direction: "SHORT", Status: store.SignalStatusFailed, MessageTimestamp: root.MessageTimestamp}
		if err := st.CopyTrade().CreateSignal(sig); err != nil {
			t.Fatal(err)
		}
	}
	ins := InterpretChromaNotification(msg, rules)
	if _, _, err := e.notificationTarget(msg, ins, rules); err == nil {
		t.Fatal("multiple histories incorrectly accepted")
	}
	msg.ReplyToMessageID = "new"
	ctx, reason, err := e.notificationTarget(msg, ins, rules)
	if err != nil || ctx != nil || reason == "" {
		t.Fatalf("failed-open outcome: %+v %s %v", ctx, reason, err)
	}
	msg.AuthorID = "other"
	if _, _, err = e.notificationTarget(msg, ins, rules); err == nil {
		t.Fatal("cross-author association")
	}
}
func TestTerminalInboxFencesDelayedOpen(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
	rules := notificationRules()
	open := notificationFixture("added")
	open.ChannelID = "201"
	open.ReceivedAt = time.Now().Add(-time.Minute)
	open.MessageTimestamp = open.ReceivedAt
	end := notificationFixture("closed")
	end.MessageID = "901"
	raw, _ := json.Marshal(end)
	if err := st.DiscordMessage().CommitInbound(&store.DiscordInbound{EventID: "end", Kind: "MESSAGE_CREATE", ChannelID: end.ChannelID, MessageID: end.MessageID, ReceivedAt: end.ReceivedAt, MessageJSON: string(raw)}, end, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.notificationOpenGate(open, InterpretChromaNotification(open, rules), rules); err == nil {
		t.Fatal("ended opening admitted")
	}
}
func TestNotificationAuthorConfig(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "100"
	cfg.SourceMode = "chroma"
	if cfg.Validate() == nil {
		t.Fatal("missing sources accepted")
	}
	cfg.SourceChannelIDs = []string{"200", "201"}
	cfg.SourceAuthorNames = []string{" JONZi "}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	msg := notificationFixture("added")
	if !cfg.MessageRules().AllowsAuthor(msg) {
		t.Fatal("normalized exact name rejected")
	}
	msg.AuthorName = "fake-JONZi"
	if cfg.MessageRules().AllowsAuthor(msg) {
		t.Fatal("substring accepted")
	}
	if InterpretChromaNotification(msg, MessageRules{}) != nil {
		t.Fatal("notification rules leaked to ordinary source")
	}
}

func TestNotificationCardDirectionConflict(t *testing.T) {
	msg := notificationFixture("added")
	msg.EmbedsJSON = `[{"title":"BTC/USDT LONG","description":"Entry: 82463.5 (LIMIT)\nTP: 49242.7\nStop/loss: 82629.6"}]`
	got := ApplyNotificationPolicy(InterpretChromaNotification(msg, notificationRules()), msg, notificationRules())
	if got.IsActionable() {
		t.Fatal("opposite direction card accepted")
	}
}

func TestNotificationCloseUsesTrackedOrdersAndPersistsUnknownOutcome(t *testing.T) {
	for _, queryDown := range []bool{false, true} {
		t.Run(fmt.Sprint("query_down_", queryDown), func(t *testing.T) {
			x, v, st := managedExecutor(t)
			p := splitPlan()
			p.ChannelID = "201"
			ctx, err := x.ExecuteOpen("opening", "open-signal", p)
			if err != nil {
				t.Fatal(err)
			}
			root := notificationFixture("added")
			root.ChannelID = "201"
			root.MessageID = p.RootMsgID
			root.Content = "BTC/USDT long was added"
			root.MessageTimestamp = time.Now().Add(-time.Minute)
			if _, err = st.DiscordMessage().Upsert(root); err != nil {
				t.Fatal(err)
			}
			if err = st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "open-signal", TraderID: "trader-1", ChannelID: "201", MessageID: root.MessageID, Action: "OPEN", Classification: "SIGNAL", Symbol: "BTCUSDT", Direction: "LONG", Status: store.SignalStatusExecuted, TradeContextID: ctx.ID, MessageTimestamp: root.MessageTimestamp, ExecutionVersion: 1}); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultCopyTradingConfig()
			cfg.PrimaryChannelID = "201"
			cfg.SourceChannelIDs = []string{"201", "202"}
			cfg.SourceMode = "chroma"
			cfg.SourceAuthorNames = []string{"JONZi"}
			cfg.ParseImages = false
			cfg.SendPositionSnapshot = false
			e := NewEngine(EngineParams{TraderID: "trader-1", Store: st, Exchange: v, Config: &cfg})
			end := notificationFixture("closed")
			end.Content = "BTC/USDT long was closed"
			end.ReplyToMessageID = root.MessageID
			v.queryDown = queryDown
			if err = e.HandleMessage(end, false); err != nil {
				t.Fatal(err)
			}
			sig, _ := st.CopyTrade().LatestSignal("trader-1", end.MessageID, 0)
			if queryDown {
				if sig.Status != "execution_wait" || sig.NextRetryAt == nil || v.qty <= 0 {
					t.Fatalf("unknown outcome treated as flat: %+v qty %g", sig, v.qty)
				}
				actions, _ := st.CopyTrade().GetActionsForMessage("trader-1", end.MessageID)
				if len(actions) != 1 || actions[0].Status != "uncertain" {
					t.Fatalf("lost action journal %+v", actions)
				}
				before := len(v.calls)
				if err = e.HandleMessage(end, false); err != nil {
					t.Fatal(err)
				}
				if len(v.calls) != before {
					t.Fatal("unknown action blindly resubmitted")
				}
			} else {
				saved, _ := st.CopyTrade().GetContext(ctx.ID)
				if sig.Status != store.SignalStatusExecuted || saved.State != "CLOSED" || v.qty != 0 {
					t.Fatalf("notification did not close tracked remainder: %s %s %g", sig.Status, saved.State, v.qty)
				}
				end.MessageID = "another-ended-notice"
				if err = e.HandleMessage(end, false); err != nil {
					t.Fatal(err)
				}
				again, _ := st.CopyTrade().LatestSignal("trader-1", end.MessageID, 0)
				if again.SkipReason != string(SkipAlreadyFlat) {
					t.Fatalf("ended state lost semantic outcome: %+v", again)
				}
			}
		})
	}
}

func TestNotificationWaitingRetainsInterpretationAndExpires(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "201"
	cfg.SourceChannelIDs = []string{"201", "202"}
	cfg.SourceMode = "chroma"
	cfg.ParseImages = false
	cfg.SendPositionSnapshot = false
	cfg.MgmtSignalTTLSeconds = 60
	e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
	root := notificationFixture("added")
	root.ChannelID = "201"
	root.MessageID = "pending-open"
	root.MessageTimestamp = time.Now().Add(-30 * time.Second)
	if _, err := st.DiscordMessage().Upsert(root); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "pending", TraderID: "t", ChannelID: "201", MessageID: root.MessageID, Action: "OPEN", Classification: "SIGNAL", Symbol: "BTCUSDT", Direction: "SHORT", Status: store.SignalStatusExecuting, MessageTimestamp: root.MessageTimestamp, ExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	end := notificationFixture("closed")
	end.ReplyToMessageID = root.MessageID
	if err := e.HandleMessage(end, false); err != nil {
		t.Fatal(err)
	}
	sig, _ := st.CopyTrade().LatestSignal("t", end.MessageID, 0)
	if sig.Status != "execution_wait" || sig.InterpretationJSON == "" {
		t.Fatalf("missing durable waiting result %+v", sig)
	}
	past := time.Now().Add(-time.Second)
	if err := st.CopyTrade().UpdateSignal(sig.ID, map[string]interface{}{"next_retry_at": past}); err != nil {
		t.Fatal(err)
	}
	// A restarted engine needs no model to resume its saved explanation.
	e = NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg})
	end.MessageTimestamp = time.Now().Add(-2 * time.Minute)
	if err := e.HandleMessage(end, false); err != nil {
		t.Fatal(err)
	}
	sig, _ = st.CopyTrade().LatestSignal("t", end.MessageID, 0)
	if sig.SkipReason != string(SkipExpired) || sig.NextRetryAt != nil {
		t.Fatalf("waiting task bypassed TTL %+v", sig)
	}
}
