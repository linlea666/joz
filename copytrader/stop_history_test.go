package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

type historyStopVenue struct {
	*preciseStopVenue
	result     *types.StopOrderResult
	historyErr error
	reject     error
	reads      int
	reappearAt int
}

func (v *historyStopVenue) GetStopOrder(symbol, orderID, clientID string) (*types.StopOrderResult, error) {
	return v.result, v.historyErr
}
func (v *historyStopVenue) SetManagedStopLoss(symbol, side string, qty, price float64, id string) error {
	if v.reject != nil {
		v.submits++
		return v.reject
	}
	return v.preciseStopVenue.SetManagedStopLoss(symbol, side, qty, price, id)
}
func (v *historyStopVenue) GetFreshPositions() ([]map[string]interface{}, error) {
	v.reads++
	if v.reappearAt > 0 && v.reads >= v.reappearAt {
		v.qty = .5
	}
	return v.managedVenue.GetFreshPositions()
}
func historyFixture(t *testing.T) (*Executor, *historyStopVenue, *store.CopyTradeContext) {
	t.Helper()
	st := newTestStore(t)
	v := &historyStopVenue{preciseStopVenue: &preciseStopVenue{managedVenue: newManagedVenue()}}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	c := newTestContext(t, st, StateBreakeven)
	p := &stopIntent{ClientID: "durable-stop", Price: 95, Requested: 95, Quantity: .5, Stage: "UNKNOWN"}
	if err := x.saveStopIntent(c, p); err != nil {
		t.Fatal(err)
	}
	v.result = &types.StopOrderResult{OpenOrder: types.OpenOrder{OrderID: "42", ClientID: p.ClientID, Symbol: c.Symbol, Side: "SELL", PositionSide: c.Direction, Type: "STOP_MARKET"}, Status: "CANCELED"}
	return x, v, c
}

func TestUnknownStopRequiresExactHistory(t *testing.T) {
	for _, scenario := range []string{"terminal", "missing", "network", "wrong_side", "wrong_client", "wrong_symbol", "wrong_account", "live", "live_child", "filled_child", "reappeared"} {
		t.Run(scenario, func(t *testing.T) {
			x, v, c := historyFixture(t)
			switch scenario {
			case "missing":
				v.historyErr = types.ErrManagedOrderNotFound
			case "network":
				v.historyErr = errors.New("history offline")
			case "wrong_side":
				v.result.PositionSide = "SHORT"
			case "wrong_client":
				v.result.ClientID = "another"
			case "wrong_symbol":
				v.result.Symbol = "ETHUSDT"
			case "wrong_account":
				c.ExchangeID = "another-account"
			case "live":
				v.result.Status = "NEW"
			case "live_child":
				v.result.Status = "FINISHED"
				v.result.ActualOrderID = "99"
				v.result.ActualOrderStatus = "NEW"
			case "filled_child":
				v.result.Status = "FINISHED"
				v.result.ActualOrderID = "99"
				v.result.ActualOrderStatus = "FILLED"
			case "reappeared":
				v.reappearAt = 2
			}
			err := x.finishTradeOrders(c)
			good := scenario == "terminal" || scenario == "filled_child"
			if good && (err != nil || c.StopIntentJSON != "") {
				t.Fatalf("terminal evidence not resolved: %v %+v", err, c)
			}
			if !good && (err == nil || c.StopIntentJSON == "") {
				t.Fatalf("uncertain protection discarded: %v", err)
			}
			if v.submits != 0 {
				t.Fatal("cleanup resubmitted stop")
			}
		})
	}
}
func TestNoPositionRejectionRacesClosure(t *testing.T) {
	x, v, c := historyFixture(t)
	if err := x.persistContext(c, map[string]interface{}{"stop_intent_json": ""}); err != nil {
		t.Fatal(err)
	}
	v.reject = &types.ManagedRejection{Code: -4509, Cause: errors.New("no open position")}
	err := x.setStopProtection("trace", "", c, .5, 95)
	if !errors.Is(err, types.ErrManagedOrderRejected) {
		t.Fatalf("%v", err)
	}
	var p stopIntent
	if err = json.Unmarshal([]byte(c.StopIntentJSON), &p); err != nil {
		t.Fatal(err)
	}
	if p.Stage != "REJECTED" || p.ErrorCode != -4509 || p.ErrorStage != "submit_stop" || p.ErrorAt == nil || p.ErrorAt.IsZero() {
		t.Fatalf("diagnostic evidence lost: %+v", p)
	}
	if err = x.finishTradeOrders(c); err != nil {
		t.Fatal(err)
	}
	if v.submits != 1 || c.StopIntentJSON != "" {
		t.Fatal("flat race resubmitted")
	}
}
func TestRejectedStopWithPositionRechecksBeforeNewIdentity(t *testing.T) {
	x, v, c := historyFixture(t)
	p := &stopIntent{ClientID: "rejected-id", Stage: "REJECTED", ErrorCode: -4509, Price: 95, Requested: 95, Quantity: .5}
	if err := x.saveStopIntent(c, p); err != nil {
		t.Fatal(err)
	}
	v.qty = .5
	if err := x.setStopProtection("trace", "", c, .5, 95); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	if v.submits != 0 {
		t.Fatal("resubmitted before new pass")
	}
	if err := x.setStopProtection("trace", "", c, .5, 95); err != nil {
		t.Fatal(err)
	}
	if v.submits != 1 || v.openOrders[0].ClientID == p.ClientID {
		t.Fatal("reused rejected identity")
	}
}
func TestFlatRecoveryLogsBoundedAndPositionReturns(t *testing.T) {
	x, v, c := historyFixture(t)
	v.historyErr = errors.New("history offline")
	e := &Engine{st: x.st, traderID: "trader-1", exec: x, events: x.events, cfg: ptrConfig(DefaultCopyTradingConfig())}
	for i := 0; i < 15; i++ {
		e.reconcileOpenTrade(c)
	}
	if !strings.HasPrefix(c.LastError, "FLAT_RECOVERY_PENDING:") || c.State == string(StateClosed) {
		t.Fatal("flat pending hidden or released")
	}
	var count int64
	x.st.GormDB().Model(&store.CopyTradeEvent{}).Where("event = ?", EvExecutionError).Count(&count)
	if count != 1 {
		t.Fatalf("repeated error flood: %d", count)
	}
	v.historyErr = nil
	e.reconcileOpenTrade(c)
	if c.State != string(StateClosed) || c.StopIntentJSON != "" {
		t.Fatalf("not closed: %+v", c)
	}
	v.qty = .5
	e.recheckClosedTrades()
	current, err := x.st.CopyTrade().GetContext(c.ID)
	if err != nil || current.State == string(StateClosed) {
		t.Fatalf("returning position abandoned: %v %+v", err, current)
	}
}
func ptrConfig(c CopyTradingConfig) *CopyTradingConfig { return &c }
func TestRecoveryEventWindow(t *testing.T) {
	st := newTestStore(t)
	l := NewEventLogger(st, "trader-1", "chan-1")
	now := time.Now()
	for i := 0; i < 10; i++ {
		l.recoveryResult("c", "stop", "failure", now.Add(time.Duration(i)*time.Second))
	}
	l.recoveryResult("c", "stop", "failure", now.Add(time.Minute))
	l.recoveryResult("c", "stop", "", now.Add(61*time.Second))
	var rows []store.CopyTradeEvent
	if err := st.GormDB().Where("trace_id = ?", "reconcile-c").Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || !strings.Contains(rows[1].ContextJSON, `"repeat_count":10`) || rows[2].Message != "recovery check resolved" {
		t.Fatal(fmt.Sprint(rows))
	}
}

func TestTriggeredStopDoesNotSubmitReplacementAfterFlat(t *testing.T) {
	x, v, c := historyFixture(t)
	if err := x.persistContext(c, map[string]interface{}{"stop_intent_json": "", "stop_order_id": "42"}); err != nil {
		t.Fatal(err)
	}
	v.qty = 0
	v.result.Status = "FINISHED"
	v.result.ActualOrderStatus = "FILLED"
	v.result.ActualOrderID = "43"
	err := x.setStopProtection("trace", "", c, .5, 95)
	if !errors.Is(err, ErrProtectionUnconfirmed) || !strings.Contains(err.Error(), "EXIT_OBSERVED") {
		t.Fatalf("wrong race outcome %v", err)
	}
	if v.submits != 0 {
		t.Fatal("submitted replacement after flat")
	}
}
