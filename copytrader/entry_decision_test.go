package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"nofx/mcp"
	"nofx/store"
	"nofx/trader/types"
)

type marketReferenceFixture struct {
	Name                   string          `json:"name"`
	Content                string          `json:"content"`
	EmbedsJSON             string          `json:"embeds_json"`
	MarketPrice            float64         `json:"market_price"`
	Response               json.RawMessage `json:"response"`
	HistoricalResponse     json.RawMessage `json:"historical_response"`
	ExpectedClassification Classification  `json:"expected_classification"`
	ExpectedReference      float64         `json:"expected_reference"`
}

func marketReferenceFixtures(t *testing.T) []marketReferenceFixture {
	t.Helper()
	b, err := os.ReadFile("../testdata/copytrading/market_reference_regressions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []marketReferenceFixture
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestMarketReferenceThresholdBoundaries(t *testing.T) {
	for _, direction := range []Direction{DirectionLong, DirectionShort} {
		for _, specType := range []PriceSpecType{PriceMarket, PriceFixed} {
			for _, tc := range []struct {
				name      string
				market    float64 // long price; reflected for shorts
				threshold float64
				convert   bool
				kind      EntryPlanType
				reason    string
			}{
				{"equal zero", 11.073, 0, false, EntryPlanMarket, "favorable_price"},
				{"favorable", 11.05, 0, false, EntryPlanMarket, "favorable_price"},
				{"zero", 11.08, 0, true, EntryPlanLimit, "adverse_tolerance_disabled"},
				{"zero tiny adverse", 11.073000000001, 0, true, EntryPlanLimit, "adverse_tolerance_disabled"},
				{"inside", 11.08, 1, true, EntryPlanMarket, "adverse_within_threshold"},
				{"exact boundary", 11.18373, 1, true, EntryPlanMarket, "adverse_within_threshold"},
				{"beyond rounding tolerance", 11.18373001, 1, true, EntryPlanLimit, "adverse_beyond_threshold"},
				{"outside", 11.20, 1, true, EntryPlanLimit, "adverse_beyond_threshold"},
				{"conversion off", 11.08, 1, false, EntryPlanLimit, "limit_conversion_disabled"},
			} {
				t.Run(string(direction)+"/"+string(specType)+"/"+tc.name, func(t *testing.T) {
					market := tc.market
					if direction == DirectionShort {
						market = 2*11.073 - market
					}
					wantKind, wantReason := tc.kind, tc.reason
					if specType == PriceMarket && tc.name == "conversion off" {
						wantKind, wantReason = EntryPlanMarket, "adverse_within_threshold"
					}
					d, err := decideEntry(direction, PriceSpec{Type: specType, Price: 11.073}, market, tc.threshold, tc.convert)
					if err != nil || d.OrderType != wantKind || d.Reason != wantReason {
						t.Fatalf("decision=%+v err=%v, want %s/%s", d, err, wantKind, wantReason)
					}
					wantPrice := 11.073
					if wantKind == EntryPlanMarket {
						wantPrice = market
					}
					if d.EntryPrice != wantPrice || d.ReferencePrice != 11.073 || d.MarketPrice != market ||
						d.ThresholdPct != tc.threshold || d.LimitToMarketWithin != tc.convert || d.AdverseDeviationPct == nil {
						t.Fatalf("decision snapshot does not match inputs: %+v", d)
					}
				})
			}
		}
	}
	d, err := decideEntry(DirectionLong, PriceSpec{Type: PriceMarket}, 11.08, 0, false)
	if err != nil || d.OrderType != EntryPlanMarket || d.AdverseDeviationPct != nil || d.Reason != "market_without_reference" {
		t.Fatalf("plain market changed: %+v, %v", d, err)
	}
	for _, price := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if kind, _, err := DecideEntryType(DirectionLong, PriceSpec{Type: PriceMarket}, price, 1, true); kind != EntryPlanSkip || err == nil {
			t.Fatalf("invalid market price allowed: %v", price)
		}
	}
}

func TestMarketReferenceValidation(t *testing.T) {
	fixture := marketReferenceFixtures(t)[1]
	for _, tc := range []struct {
		name      string
		ref       float64
		market    float64
		want      SkipReason
		wantError bool
	}{
		{"correct ICP reference", 2.573, 2.575, SkipNone, false},
		{"digit error", 42.573, 2.575, SkipSanityCheck, false},
		{"negative reference", -2.573, 2.575, SkipNone, true},
		{"no reference", 0, 2.575, SkipNone, false},
		{"market unavailable", 2.573, 0, SkipUnsupportedPriceSpec, false},
		{"stop crossed", 2.573, 2.5, SkipNone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			si, err := ParseInterpretation(string(fixture.Response))
			if err != nil {
				t.Fatal(err)
			}
			si.EntryOrders[0].Price.Price = tc.ref
			skip, err := ValidateInterpretation(si, tc.market)
			if skip != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("skip=%q err=%v", skip, err)
			}
		})
	}
	if _, err := ParseInterpretation(`{"classification":"SIGNAL","action":"OPEN","entry_orders":[{"order_type":"MARKET","price":{"type":"MARKET","price":-2.573}}]}`); err == nil {
		t.Fatal("negative market reference must fail parsing")
	}
}

type recordedEntryExchange struct {
	*mockExchange
	price         float64
	marketEntries int
	limitEntries  []*types.LimitOrderRequest
}

func (x *recordedEntryExchange) GetMarketPrice(string) (float64, error) { return x.price, nil }
func (x *recordedEntryExchange) OpenLong(string, float64, int) (map[string]interface{}, error) {
	x.marketEntries++
	return map[string]interface{}{"orderId": "market-entry"}, nil
}
func (x *recordedEntryExchange) OpenShort(s string, q float64, l int) (map[string]interface{}, error) {
	return x.OpenLong(s, q, l)
}
func (x *recordedEntryExchange) PlaceLimitOrder(req *types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	// TP limits are protection orders, not new entries.
	if !req.ReduceOnly {
		copy := *req
		x.limitEntries = append(x.limitEntries, &copy)
	}
	return &types.LimitOrderResult{OrderID: "limit-order"}, nil
}

type fixtureLLM struct {
	mcp.AIClient
	response string
	request  *mcp.Request
	calls    int
}

func (l *fixtureLLM) CallWithRequest(req *mcp.Request) (string, error) {
	l.request, l.calls = req, l.calls+1
	return l.response, nil
}

func fixtureEngine(t *testing.T, f marketReferenceFixture, threshold float64) (*Engine, *recordedEntryExchange, *fixtureLLM, *store.DiscordMessage) {
	t.Helper()
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID, cfg.AltcoinPriceOffsetPct = "123", threshold
	cfg.ParseImages, cfg.SendPositionSnapshot, cfg.SignalContextEnabled = false, false, false
	x := &recordedEntryExchange{mockExchange: &mockExchange{orderStatus: map[string]interface{}{"status": "FILLED"}}, price: f.MarketPrice}
	llm := &fixtureLLM{response: string(f.Response)}
	e := NewEngine(EngineParams{TraderID: "trader-1", Config: &cfg, Store: st, Exchange: x, LLM: llm})
	msg := &store.DiscordMessage{MessageID: f.Name, ChannelID: "123", Content: f.Content, EmbedsJSON: f.EmbedsJSON, MessageTimestamp: time.Now().UTC()}
	return e, x, llm, msg
}

func TestMarketReferenceExecutionAndAudit(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	for _, threshold := range []float64{0, 1} {
		t.Run(fmt.Sprintf("threshold_%g", threshold), func(t *testing.T) {
			e, x, llm, msg := fixtureEngine(t, f, threshold)
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			sigs, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
			if err != nil || len(sigs) != 1 {
				t.Fatalf("signals: %v %v", sigs, err)
			}
			if sigs[0].Status != store.SignalStatusExecuted {
				t.Fatalf("signal: %+v", sigs[0])
			}
			ctx, err := e.st.CopyTrade().GetContext(sigs[0].TradeContextID)
			if err != nil {
				t.Fatal(err)
			}
			kind := EntryPlanMarket
			if threshold == 0 {
				kind = EntryPlanLimit
				if x.marketEntries != 0 || len(x.limitEntries) != 1 || x.limitEntries[0].Price != 11.073 || ctx.State != string(StateEntryPending) {
					t.Fatalf("zero threshold did not wait at reference: %+v", ctx)
				}
			} else if x.marketEntries != 1 || len(x.limitEntries) != 0 || ctx.State != string(StateOpen) || len(x.setStopLossLog) == 0 {
				t.Fatalf("market entry/protection missing: %+v", ctx)
			}
			events, err := e.st.CopyTrade().GetEventsByTrace(e.traderID, sigs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			decisions := 0
			for _, event := range events {
				if event.Event != EvEntryDecision {
					continue
				}
				decisions++
				var record struct {
					Decision        entryDecision `json:"decision"`
					SourceOrderType string        `json:"source_order_type"`
				}
				if err := json.Unmarshal([]byte(event.ContextJSON), &record); err != nil {
					t.Fatal(err)
				}
				d := record.Decision
				if record.SourceOrderType != "MARKET" || d.OrderType != kind || d.ThresholdPct != threshold || d.MarketPrice != f.MarketPrice || d.ReferencePrice != 11.073 || d.EntryPrice != ctx.PlannedEntryPrice || d.AdverseDeviationPct == nil {
					t.Fatalf("incorrect audit snapshot: %+v", record)
				}
			}
			if decisions != 1 {
				t.Fatalf("got %d decision events", decisions)
			}
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			if llm.calls != 1 {
				t.Fatal("duplicate message was reinterpreted")
			}
		})
	}
}

func TestMarketReferenceReplayIsReadOnly(t *testing.T) {
	for _, f := range marketReferenceFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			e, x, llm, msg := fixtureEngine(t, f, 1)
			item := e.replayOne(msg)
			if item.Classification != string(f.ExpectedClassification) || item.Error != "" {
				t.Fatalf("replay: %+v", item)
			}
			if x.marketEntries != 0 || len(x.limitEntries) != 0 {
				t.Fatal("replay placed an order")
			}
			for _, model := range []interface{}{&store.CopyTradeSignal{}, &store.CopyTradeAIRun{}, &store.CopyTradeContext{}, &store.CopyTradeEvent{}} {
				var n int64
				if err := e.st.GormDB().Model(model).Count(&n).Error; err != nil || n != 0 {
					t.Fatalf("replay persisted %T: %d, %v", model, n, err)
				}
			}
			// The replay and live interpreter must both send the updated contract.
			b, err := json.Marshal(llm.request)
			if err != nil || !strings.Contains(string(b), "市價-2.573") {
				t.Fatal("updated contract not sent to LLM")
			}
			if f.ExpectedClassification != ClassificationSignal {
				if err := e.HandleMessage(msg, false); err != nil {
					t.Fatal(err)
				}
				if x.marketEntries != 0 || len(x.limitEntries) != 0 {
					t.Fatal("non-actionable message opened a trade")
				}
			}
		})
	}
}

func TestMarketReferenceLimitLifecycleVisibility(t *testing.T) {
	for _, tc := range []struct{ name, orderStatus, wantState, wantEvent string }{
		{"filled", "FILLED", string(StateOpen), EvEntryFilled},
		{"expired", "NEW", string(StateExpired), EvTradeExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, x, _, msg := fixtureEngine(t, marketReferenceFixtures(t)[0], 0)
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			sigs, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
			if err != nil || len(sigs) != 1 {
				t.Fatalf("signals: %v %v", sigs, err)
			}
			ctx, err := e.st.CopyTrade().GetContext(sigs[0].TradeContextID)
			if err != nil || ctx.State != string(StateEntryPending) {
				t.Fatalf("pending context: %+v %v", ctx, err)
			}
			x.orderStatus = map[string]interface{}{"status": tc.orderStatus}
			if tc.orderStatus == "FILLED" {
				x.orderStatus["executedQty"] = ctx.Quantity
				x.orderStatus["avgPrice"] = ctx.PlannedEntryPrice
			}
			ctx.CreatedAt = time.Now().Add(-241 * time.Minute)
			e.reconcileEntryPending(ctx)
			views, err := e.st.CopyTrade().SignalViews(e.traderID, sigs)
			if err != nil || len(views) != 1 || views[0].TradeState != tc.wantState || views[0].Status != store.SignalStatusExecuted {
				t.Fatalf("lifecycle view: %+v %v", views, err)
			}
			// This is the same trace contract the signal details UI queries.
			events, err := e.st.CopyTrade().GetEventsByTrace(e.traderID, "reconcile-"+ctx.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				found = found || event.Event == tc.wantEvent
			}
			if !found {
				t.Fatalf("missing lifecycle event %s", tc.wantEvent)
			}
			if tc.wantState == string(StateExpired) && len(x.cancelOrderLog) != 1 {
				t.Fatal("expired limit was not cancelled")
			}
			if tc.wantState == string(StateOpen) && len(x.setStopLossLog) == 0 {
				t.Fatal("filled limit was not protected")
			}
		})
	}
}

func TestCorrectedMarketReferenceRevisionCanOpen(t *testing.T) {
	f := marketReferenceFixtures(t)[1]
	e, x, llm, msg := fixtureEngine(t, f, 1)
	llm.response = string(f.HistoricalResponse)
	if err := e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	if x.marketEntries != 0 {
		t.Fatal("ambiguous revision opened")
	}
	msg.Revision++
	llm.response = string(f.Response)
	if err := e.HandleMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 2 || x.marketEntries != 1 {
		t.Fatal("corrected revision was swallowed")
	}
	if err := e.HandleMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 2 || x.marketEntries != 1 {
		t.Fatal("corrected revision executed twice")
	}
}
