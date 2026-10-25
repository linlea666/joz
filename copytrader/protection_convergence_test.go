package copytrader

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

// fakeStopClock freezes stopClock and returns an advance function.
func fakeStopClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	old := stopClock
	stopClock = func() time.Time { return now }
	t.Cleanup(func() { stopClock = old })
	return func(d time.Duration) { now = now.Add(d) }
}

func readStopIntent(t *testing.T, c *store.CopyTradeContext) *stopIntent {
	t.Helper()
	if c.StopIntentJSON == "" {
		return nil
	}
	var p stopIntent
	if err := json.Unmarshal([]byte(c.StopIntentJSON), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func eventsContaining(t *testing.T, st *store.Store, traceID, level, text string) int {
	t.Helper()
	rows, err := st.CopyTrade().GetEventsByTrace("trader-1", traceID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if (level == "" || r.Level == level) && strings.Contains(r.Message, text) {
			n++
		}
	}
	return n
}

func TestStopPreSendFailureStaysPlannedAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int64
	}{
		{"market_rules_not_sent", types.NotSent(errors.New("exchangeInfo timeout")), 0},
		{"rate_limited", &types.ManagedRejection{Code: -1003, Retryable: true, Cause: errors.New("too many requests")}, -1003},
		{"timestamp", &types.ManagedRejection{Code: -1021, Retryable: true, Cause: errors.New("recvWindow")}, -1021},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, v, c := historyFixture(t)
			if err := x.persistContext(c, map[string]interface{}{"stop_intent_json": ""}); err != nil {
				t.Fatal(err)
			}
			v.qty = .5
			v.reject = tc.err
			err := x.setStopProtection("trace", "", c, .5, 95)
			if !errors.Is(err, ErrProtectionUnconfirmed) {
				t.Fatalf("not-executed submission must stay an unconfirmed retry, got %v", err)
			}
			p := readStopIntent(t, c)
			if p == nil || p.Stage != "PLANNED" || p.ErrorCode != tc.code || p.ErrorAt == nil {
				t.Fatalf("pre-send failure must not become UNKNOWN: %+v", p)
			}
			first := p.ClientID
			v.reject = nil
			if err = x.setStopProtection("trace", "", c, .5, 95); err != nil {
				t.Fatalf("retry after not-sent: %v", err)
			}
			if c.StopIntentJSON != "" || c.StopOrderID == "" || len(v.openOrders) != 1 || v.openOrders[0].ClientID != first {
				t.Fatalf("retry did not protect with the same identity: %+v %+v", c, v.openOrders)
			}
		})
	}
}

func TestStopDefiniteRejectionStillRejected(t *testing.T) {
	x, v, c := historyFixture(t)
	if err := x.persistContext(c, map[string]interface{}{"stop_intent_json": ""}); err != nil {
		t.Fatal(err)
	}
	v.qty = .5
	v.reject = &types.ManagedRejection{Code: -4061, Cause: errors.New("position side mismatch")}
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, types.ErrManagedOrderRejected) || errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatalf("definite rejection changed: %v", err)
	}
	if p := readStopIntent(t, c); p == nil || p.Stage != "REJECTED" || p.ErrorCode != -4061 {
		t.Fatalf("%+v", p)
	}
}

func TestUnknownStopNotFoundBeyondGraceResubmitsWithNewIdentity(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.qty = .5
	v.historyErr = types.ErrManagedOrderNotFound
	for i, step := range []time.Duration{0, 30 * time.Second} {
		advance(step)
		if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
			t.Fatalf("pass %d: %v", i, err)
		}
		if v.submits != 0 {
			t.Fatalf("pass %d resubmitted inside the grace period", i)
		}
	}
	if p := readStopIntent(t, c); p.NotFoundCount != 2 || p.Stage != "UNKNOWN" {
		t.Fatalf("not-found evidence not recorded: %+v", p)
	}
	advance(31 * time.Second)
	if err := x.setStopProtection("trace", "", c, .5, 95); err != nil {
		t.Fatalf("converged pass: %v", err)
	}
	if v.submits != 1 || len(v.openOrders) != 1 {
		t.Fatalf("expected exactly one resubmission: %d %+v", v.submits, v.openOrders)
	}
	if id := v.openOrders[0].ClientID; id == "durable-stop" || !strings.HasPrefix(id, "ct") {
		t.Fatalf("resubmission reused the lost identity: %s", id)
	}
	if c.StopIntentJSON != "" || c.StopOrderID != v.openOrders[0].OrderID {
		t.Fatalf("new stop not recorded: %+v", c)
	}
	if eventsContaining(t, x.st, "trace", "warn", "never reached the exchange") != 1 {
		t.Fatal("missing resubmission warning")
	}
}

func TestUnknownStopNetworkErrorNeverResubmits(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.qty = .5
	v.historyErr = types.ErrManagedOrderNotFound
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	// A transport failure breaks the consecutive not-found evidence.
	advance(30 * time.Second)
	v.historyErr = errors.New("history offline")
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	if p := readStopIntent(t, c); p.NotFoundCount != 0 || p.FirstNotFoundAt != nil {
		t.Fatalf("network error did not reset absence evidence: %+v", p)
	}
	advance(40 * time.Second)
	v.historyErr = types.ErrManagedOrderNotFound
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	advance(30 * time.Second)
	v.historyErr = errors.New("history offline")
	for i := 0; i < 3; i++ {
		advance(time.Minute)
		_ = x.setStopProtection("trace", "", c, .5, 95)
	}
	// (The unprotected-position fallback may close the position meanwhile;
	// this test only asserts the resubmission rule.)
	if v.submits != 0 {
		t.Fatalf("network uncertainty resubmitted a stop (%d)", v.submits)
	}
}

func TestUnknownStopWithOtherVisibleStopDoesNotResubmit(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.qty = .5
	v.historyErr = types.ErrManagedOrderNotFound
	v.openOrders = []types.OpenOrder{{OrderID: "manual", ClientID: "manual-1", Symbol: c.Symbol, PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 94, ClosePosition: true}}
	for _, step := range []time.Duration{0, 31 * time.Second, 31 * time.Second} {
		advance(step)
		if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
			t.Fatal(err)
		}
	}
	if v.submits != 0 || v.cancels != 0 || len(v.openOrders) != 1 {
		t.Fatal("ambiguous visible stop was replaced or duplicated")
	}
}

type lateStopVenue struct {
	*historyStopVenue
	hidden *types.OpenOrder
}

func (v *lateStopVenue) SetManagedStopLoss(symbol, side string, qty, price float64, id string) error {
	if v.hidden != nil {
		v.submits++
		v.openOrders = append(v.openOrders, *v.hidden)
		v.hidden = nil
		return &types.ManagedRejection{Code: -4130, Cause: errors.New("closePosition stop exists")}
	}
	return v.historyStopVenue.SetManagedStopLoss(symbol, side, qty, price, id)
}

func lateFixture(t *testing.T, hiddenClient string) (*Executor, *lateStopVenue, *store.CopyTradeContext) {
	x, hv, c := historyFixture(t)
	v := &lateStopVenue{historyStopVenue: hv, hidden: &types.OpenOrder{OrderID: "late-1", ClientID: hiddenClient, OrderKind: "ALGO", Symbol: c.Symbol, PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 95, ClosePosition: true}}
	x = NewExecutor("trader-1", v, x.st, x.events)
	hv.qty = .5
	hv.historyErr = types.ErrManagedOrderNotFound
	return x, v, c
}

func TestStopResubmitMinus4130AdoptsOwnExistingStop(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := lateFixture(t, "durable-stop")
	for _, step := range []time.Duration{0, 61 * time.Second} {
		advance(step)
		_ = x.setStopProtection("trace", "", c, .5, 95)
	}
	if v.submits != 1 {
		t.Fatalf("expected one resubmission attempt, got %d", v.submits)
	}
	if c.StopIntentJSON != "" || c.StopOrderID != "late-1" || c.StopLossPrice != 95 {
		t.Fatalf("existing own stop not adopted: %+v", c)
	}
	if v.cancels != 0 || len(v.openOrders) != 1 {
		t.Fatal("adoption changed exchange orders")
	}
	if err := x.setStopProtection("trace", "", c, .5, 95); err != nil || v.submits != 1 {
		t.Fatalf("adopted stop not recognised as protection: %v", err)
	}
}

func TestStopMinus4130ForeignStopLeftForReview(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := lateFixture(t, "someone-else")
	for _, step := range []time.Duration{0, 61 * time.Second} {
		advance(step)
		_ = x.setStopProtection("trace", "", c, .5, 95)
	}
	p := readStopIntent(t, c)
	if p == nil || p.Stage != "REJECTED" || p.ErrorCode != -4130 || c.StopOrderID == "late-1" {
		t.Fatalf("foreign stop adopted: %+v %+v", p, c)
	}
	err := x.setStopProtection("trace", "", c, .5, 95)
	if !errors.Is(err, ErrProtectionUnconfirmed) || v.cancels != 0 || v.submits != 1 {
		t.Fatalf("foreign stop touched or resubmitted: %v", err)
	}
	// The blocking stop disappears: re-plan under a new identity and protect.
	v.openOrders = nil
	if err = x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	if err = x.setStopProtection("trace", "", c, .5, 95); err != nil {
		t.Fatal(err)
	}
	if len(v.openOrders) != 1 || v.openOrders[0].ClientID == "durable-stop" || v.openOrders[0].ClientID == p.ClientID {
		t.Fatalf("re-planned stop reused an identity: %+v", v.openOrders)
	}
}

func TestUnprotectedPositionRaisesErrorAndEmergencyCloses(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.qty, v.avg = .5, 100
	v.historyErr = errors.New("history offline")
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	advance(2 * time.Minute)
	_ = x.setStopProtection("trace", "", c, .5, 95)
	if v.qty != .5 || eventsContaining(t, x.st, "trace", "error", "持仓无确认止损") != 0 {
		t.Fatal("fallback fired before the threshold")
	}
	advance(61 * time.Second)
	err := x.setStopProtection("trace", "", c, .5, 95)
	if !errors.Is(err, ErrProtectionUnconfirmed) || !strings.Contains(err.Error(), "UNPROTECTED") {
		t.Fatalf("fallback error not surfaced: %v", err)
	}
	if eventsContaining(t, x.st, "trace", "error", "持仓无确认止损") != 1 {
		t.Fatal("missing unprotected-position Error event")
	}
	if v.qty != 0 {
		t.Fatalf("emergency close not executed, qty %.8g", v.qty)
	}
	var closes int
	for _, call := range v.calls {
		if call.ReduceOnly && call.Type == "MARKET" {
			closes++
		}
	}
	if closes != 1 || v.submits != 0 {
		t.Fatalf("expected one market close and no stop resubmission: %d %d", closes, v.submits)
	}
	// Further passes never duplicate the close.
	advance(time.Minute)
	_ = x.setStopProtection("trace", "", c, .5, 95)
	closes = 0
	for _, call := range v.calls {
		if call.ReduceOnly && call.Type == "MARKET" {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("emergency close repeated: %d", closes)
	}
}

func TestUnprotectedFallbackSkipsWhenAnyStopVisible(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.qty = .5
	v.historyErr = errors.New("history offline")
	v.openOrders = []types.OpenOrder{{OrderID: "manual", ClientID: "manual-1", Symbol: c.Symbol, PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 94, ClosePosition: true}}
	_ = x.setStopProtection("trace", "", c, .5, 95)
	advance(10 * time.Minute)
	_ = x.setStopProtection("trace", "", c, .5, 95)
	if v.qty != .5 || len(v.calls) != 0 || eventsContaining(t, x.st, "trace", "error", "持仓无确认止损") != 0 {
		t.Fatal("position with a visible stop was emergency closed")
	}
}

func TestFlatCleanupResolvesUnlandedStopAfterGrace(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := historyFixture(t)
	v.historyErr = types.ErrManagedOrderNotFound
	for _, step := range []time.Duration{0, 30 * time.Second} {
		advance(step)
		if err := x.finishTradeOrders(c); err == nil || c.StopIntentJSON == "" {
			t.Fatal("absence accepted inside grace period")
		}
	}
	advance(31 * time.Second)
	if err := x.finishTradeOrders(c); err != nil || c.StopIntentJSON != "" {
		t.Fatalf("converged absence not resolved: %v", err)
	}
	if v.submits != 0 {
		t.Fatal("flat cleanup submitted a stop")
	}
}

// --- take-profit convergence ---

type tpVenue struct {
	*managedVenue
	mark  float64
	tpErr func(*types.ManagedOrderRequest) error
}

func (v *tpVenue) GetMarketPrice(string) (float64, error) {
	if v.mark > 0 {
		return v.mark, nil
	}
	return v.market, nil
}
func (v *tpVenue) SubmitManagedOrder(r *types.ManagedOrderRequest) (*types.ManagedOrderResult, error) {
	if r.ReduceOnly && r.Type == "LIMIT" && v.tpErr != nil {
		if err := v.tpErr(r); err != nil {
			v.calls = append(v.calls, *r)
			return nil, err
		}
	}
	return v.managedVenue.SubmitManagedOrder(r)
}

func tpSubmits(v *tpVenue, price float64) int {
	n := 0
	for _, c := range v.calls {
		if c.ReduceOnly && c.Type == "LIMIT" && c.Price == price {
			n++
		}
	}
	return n
}

func tpFixture(t *testing.T, direction string, stop float64, tps ...TPPlanEntry) (*Executor, *tpVenue, *store.CopyTradeContext) {
	t.Helper()
	st := newTestStore(t)
	v := &tpVenue{managedVenue: newManagedVenue()}
	v.qty, v.avg, v.direction = 1, 100, direction
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	c := newTestContext(t, st, StateOpen)
	b, _ := json.Marshal(tps)
	if err := x.persistContext(c, map[string]interface{}{"direction": direction, "quantity": 1.0, "stop_loss_price": stop, "requested_stop_loss": stop, "tp_plan_json": string(b), "tp_position_quantity": 1.0}); err != nil {
		t.Fatal(err)
	}
	return x, v, c
}

func TestRejectedTPDoesNotBlockOtherLevelsOrRestore(t *testing.T) {
	x, v, c := tpFixture(t, "LONG", 95, TPPlanEntry{Ordinal: 1, Price: 110, Quantity: .5, Status: "PLANNED"}, TPPlanEntry{Ordinal: 2, Price: 120, Quantity: .5, Status: "PLANNED"})
	v.tpErr = func(r *types.ManagedOrderRequest) error {
		if r.Price == 110 {
			return &types.ManagedRejection{Code: -2022, Cause: errors.New("<APIError> code=-2022, msg=ReduceOnly Order is rejected.")}
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
			t.Fatalf("pass %d: rejected TP blocked restore: %v", i, err)
		}
	}
	plan := readTPPlan(c)
	if plan[0].Status != "REJECTED" || plan[1].OrderID == "" || plan[1].Status != "NEW" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if tpSubmits(v, 110) != 1 || tpSubmits(v, 120) != 1 {
		t.Fatalf("rejected TP resubmitted or second level missing: %+v", v.calls)
	}
	if len(v.openOrders) != 1 || v.openOrders[0].StopPrice != 95 {
		t.Fatalf("stop protection affected: %+v", v.openOrders)
	}
	if eventsContaining(t, x.st, "trace", "warn", "code -2022") != 1 {
		t.Fatal("rejection warning missing or repeated")
	}
}

func TestUnknownTPNotFoundBeyondGraceIsResubmitted(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := tpFixture(t, "LONG", 95, TPPlanEntry{Ordinal: 1, Price: 110, Quantity: 1, Status: "PLANNED"})
	lost := true
	v.tpErr = func(*types.ManagedOrderRequest) error {
		if lost {
			lost = false
			return errors.New("connection reset")
		}
		return nil
	}
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatalf("unknown TP blocked restore: %v", err)
	}
	first := readTPPlan(c)[0].ClientID
	if first == "" {
		t.Fatal("TP identity not retained for reconciliation")
	}
	advance(10 * time.Second)
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatal(err)
	}
	if tpSubmits(v, 110) != 1 {
		t.Fatal("resubmitted inside grace period")
	}
	v.queryDown = true
	advance(2 * time.Minute)
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatalf("TP query outage must only skip the level: %v", err)
	}
	if tpSubmits(v, 110) != 1 {
		t.Fatal("query outage caused resubmission")
	}
	v.queryDown = false
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatal(err)
	}
	tp := readTPPlan(c)[0]
	if tpSubmits(v, 110) != 2 || tp.ClientID == first || tp.OrderID == "" || tp.Status != "NEW" {
		t.Fatalf("unlanded TP not resubmitted with new identity: %+v", tp)
	}
	rows, _ := x.st.CopyTrade().GetOrdersForContext("trader-1", c.ID)
	for _, r := range rows {
		if r.ClientID == first && r.Status != "ABANDONED" {
			t.Fatalf("lost TP row not abandoned: %+v", r)
		}
	}
}

func TestTPOutsidePercentPriceDeferredThenPlaced(t *testing.T) {
	x, v, c := tpFixture(t, "LONG", 95, TPPlanEntry{Ordinal: 1, Price: 110, Quantity: .5, Status: "PLANNED"}, TPPlanEntry{Ordinal: 2, Price: 130, Quantity: .5, Status: "PLANNED"})
	v.rules.PercentPriceUp, v.rules.PercentPriceDown = 1.05, .95
	v.mark = 120 // SELL 110 < 120*0.95: venue would refuse it
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatalf("deferred TP blocked restore: %v", err)
	}
	plan := readTPPlan(c)
	if tpSubmits(v, 110) != 0 || plan[0].ClientID != "" || plan[0].Status != "PLANNED" {
		t.Fatalf("out-of-band TP submitted: %+v", plan[0])
	}
	if plan[1].OrderID == "" || len(v.openOrders) != 1 {
		t.Fatalf("other level or stop affected: %+v %+v", plan, v.openOrders)
	}
	if eventsContaining(t, x.st, "trace", "warn", "超出交易所限价范围") != 1 {
		t.Fatal("deferral warning missing")
	}
	_ = x.restoreRemainingProtections("trace", "", c, 1)
	if eventsContaining(t, x.st, "trace", "warn", "超出交易所限价范围") != 1 {
		t.Fatal("deferral warning repeated every pass")
	}
	v.mark = 112
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatal(err)
	}
	if tpSubmits(v, 110) != 1 || readTPPlan(c)[0].OrderID == "" {
		t.Fatal("TP not placed after entering the band")
	}
}

func TestTPStaticMaxPriceDeferredWithoutBlockingEntryProtection(t *testing.T) {
	x, v, c := tpFixture(t, "LONG", 95, TPPlanEntry{Ordinal: 1, Price: 130, Quantity: 1, Status: "PLANNED"})
	v.rules.MaxPrice = 125
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatal(err)
	}
	if tpSubmits(v, 130) != 0 || len(v.openOrders) != 1 {
		t.Fatal("max-price TP submitted or stop missing")
	}
	v.rules.MaxPrice = 0
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil || tpSubmits(v, 130) != 1 {
		t.Fatalf("TP not placed once allowed: %v", err)
	}
}

// Jonzi swing short: BTC entry 83664, stop 83951, TP 49236 (~-40%). Binance
// refuses the far limit with -4024; the level is deferred and retried later.
func TestJonziFarShortTPPriceBandRejectionDeferred(t *testing.T) {
	advance := fakeStopClock(t)
	x, v, c := tpFixture(t, "SHORT", 83951, TPPlanEntry{Ordinal: 1, Price: 49236, Quantity: 1, Status: "PLANNED"})
	v.avg, v.market = 83664, 83664
	band := true
	v.tpErr = func(r *types.ManagedOrderRequest) error {
		if band {
			return &types.ManagedRejection{Code: -4024, Cause: errors.New("<APIError> code=-4024, msg=Limit price can't be lower than 46013.6.")}
		}
		return nil
	}
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatalf("price-band rejection blocked restore: %v", err)
	}
	tp := readTPPlan(c)[0]
	if tp.ClientID != "" || tp.Status != "PLANNED" || tp.Generation != 1 {
		t.Fatalf("band-rejected TP not deferred: %+v", tp)
	}
	if len(v.openOrders) != 1 || v.openOrders[0].StopPrice != 83951 {
		t.Fatalf("stop affected: %+v", v.openOrders)
	}
	if eventsContaining(t, x.st, "trace", "warn", "超出交易所限价范围") != 1 {
		t.Fatal("deferral warning missing")
	}
	advance(time.Minute)
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil || tpSubmits(v, 49236) != 1 {
		t.Fatalf("deferred TP retried too early: %v", err)
	}
	band = false
	advance(tpRejectedRetry)
	if err := x.restoreRemainingProtections("trace", "", c, 1); err != nil {
		t.Fatal(err)
	}
	if tp = readTPPlan(c)[0]; tpSubmits(v, 49236) != 2 || tp.OrderID == "" {
		t.Fatalf("deferred TP not placed later: %+v", tp)
	}
}
