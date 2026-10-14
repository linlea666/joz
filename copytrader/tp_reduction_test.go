package copytrader

import (
	"math"
	"nofx/trader/types"
	"testing"
)

func TestTPReplacementRemainingAllocationAndRecovery(t *testing.T) {
	for _, scenario := range []string{"normal", "partial_tp", "lost_ack", "prepare_restart", "filled_tp"} {
		t.Run(scenario, func(t *testing.T) {
			x, v, st := managedExecutor(t)
			ctx, err := x.ExecuteOpen("trace", "open", splitPlan())
			if err != nil {
				t.Fatal(err)
			}
			plan := readTPPlan(ctx)
			first := plan[0]
			next := plan[1]
			if scenario == "partial_tp" {
				v.fill(v.orders[first.ClientID], first.Quantity/4, first.Price)
			}
			if scenario == "filled_tp" {
				v.fill(v.orders[first.ClientID], first.Quantity, first.Price)
			}
			if scenario == "lost_ack" {
				v.lostAck = true
			}
			if scenario == "prepare_restart" {
				if err = x.persistContext(ctx, map[string]interface{}{"close_pending_json": `{"signal_id":"reduce","tp_level":1}`}); err != nil {
					t.Fatal(err)
				}
			}
			before := v.qty
			_, err = x.ExecuteTPReduction("trace", "reduce", ctx, 1)
			if err != nil && scenario != "lost_ack" {
				t.Fatal(err)
			}
			fresh, err := st.CopyTrade().GetContext(ctx.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.ClosePendingJSON != "" {
				if err = NewExecutor(x.traderID, v, st, x.events).reconcileManagedClose("restart", fresh); err != nil {
					t.Fatal(err)
				}
			}
			want := first.Quantity
			if scenario == "partial_tp" {
				want *= .75
			}
			if scenario == "filled_tp" {
				want = 0
			}
			want, _ = types.DecimalFloor(types.SanitizeBaseQuantity(want), v.rules.MarketQuantityStep)
			if math.Abs(before-v.qty-want) > 1e-8 {
				t.Fatalf("reduced %.8f want %.8f", before-v.qty, want)
			}
			if v.orders[next.ClientID].Status != "NEW" {
				t.Fatal("later TP changed")
			}
			count := len(v.calls)
			if _, err = x.ExecuteTPReduction("again", "reduce", fresh, 1); err != nil && scenario != "partial_tp" {
				t.Fatal(err)
			}
			if len(v.calls) != count {
				t.Fatal("duplicate exit on retry")
			}
		})
	}
}

type replacementCancelRaceVenue struct {
	*managedVenue
	target string
}

func (v *replacementCancelRaceVenue) CancelOrder(symbol, id string) error {
	if id == v.target {
		for _, o := range v.orders {
			if o.OrderID == id {
				v.fill(o, o.Quantity/2, o.Price)
				break
			}
		}
		v.target = ""
	}
	return v.managedVenue.CancelOrder(symbol, id)
}
func TestTPReplacementCountsCancellationFill(t *testing.T) {
	x, v, st := managedExecutor(t)
	c, err := x.ExecuteOpen("trace", "open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	tp := readTPPlan(c)[0]
	before := v.qty
	race := &replacementCancelRaceVenue{managedVenue: v, target: tp.OrderID}
	x = NewExecutor(x.traderID, race, st, x.events)
	if _, err = x.ExecuteTPReduction("trace", "reduce", c, 1); err != nil {
		t.Fatal(err)
	}
	if math.Abs(before-v.qty-tp.Quantity) > .001 {
		t.Fatalf("cancel race over-reduced: %g want %g", before-v.qty, tp.Quantity)
	}
	exits := 0
	for _, o := range v.calls {
		if o.ReduceOnly && o.Type == "MARKET" {
			exits++
			if o.Quantity > tp.Quantity/2+.001 {
				t.Fatal("sold original quantity after TP race")
			}
		}
	}
	if exits != 1 {
		t.Fatalf("exits %d", exits)
	}
}
