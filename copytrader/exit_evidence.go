package copytrader

import (
	"strings"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

// Exit reasons recorded when a trade is finalized after its position left the
// exchange outside of an executor-driven close.
const (
	ExitReasonStopLoss   = "STOP_LOSS"
	ExitReasonTakeProfit = "TAKE_PROFIT"
	ExitReasonAuthor     = "AUTHOR_CLOSE"
	ExitReasonSystem     = "SYSTEM_CLOSE" // emergency / risk-budget closes by the executor
	ExitReasonUnknown    = "MANUAL_OR_UNKNOWN"
)

// exitEvidence is the forensic summary of how a trade's position ended. It is
// recorded in the trade-closed event data (no schema change).
type exitEvidence struct {
	Reason         string     `json:"exit_reason"`
	Source         string     `json:"exit_reason_source"`
	Detail         string     `json:"exit_detail,omitempty"`
	StopOrderID    string     `json:"stop_order_id,omitempty"`
	StopStatus     string     `json:"stop_status,omitempty"`
	ClosingRole    string     `json:"closing_role,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	ClosedAtSource string     `json:"closed_at_source,omitempty"`
	RealizedPnL    *float64   `json:"realized_pnl,omitempty"`
	PnLSource      string     `json:"realized_pnl_source,omitempty"`
	PnLNote        string     `json:"realized_pnl_note,omitempty"`
}

// closedPnLLimit is the largest page requested from GetClosedPnL; a full page
// may be truncated, so it is never attributed.
const closedPnLLimit = 1000

// collectExitEvidence determines why a confirmed-flat trade ended. stopID is
// the trade's stop identity captured BEFORE terminal cleanup cleared it.
// Every lookup is best effort: a failed read only lowers the confidence of
// the result, it never blocks finalization.
func (x *Executor) collectExitEvidence(traceID string, ctx *store.CopyTradeContext, stopID string) exitEvidence {
	ev := exitEvidence{Reason: ExitReasonUnknown, Source: "none", StopOrderID: stopID}

	// 1. The stop order itself: a triggered algo whose child order FILLED.
	if stopID != "" {
		if reader, ok := x.ex.(types.StopOrderReader); ok {
			res, err := reader.GetStopOrder(ctx.Symbol, stopID, "")
			if err != nil {
				ev.StopStatus = "lookup_failed"
			} else if res != nil && res.OrderID == stopID {
				ev.StopStatus = res.Status
				if res.ActualOrderStatus != "" {
					ev.StopStatus += "/" + res.ActualOrderStatus
				}
				if res.ActualOrderID != "" && res.ActualOrderID != "0" && res.ActualOrderStatus == "FILLED" {
					ev.Reason, ev.Source = ExitReasonStopLoss, "stop_order_history"
				}
			}
		}
	}

	// 2. The order ledger (managed venues) or the TP plan (legacy venues):
	// only when the recorded exit fills explain the whole entry does the
	// latest one tell which order closed the trade.
	entryQty := ctx.EntryFilledQuantity
	if entryQty <= 0 {
		entryQty = ctx.Quantity
	}
	explains := func(q float64) bool { return entryQty > 0 && q+1e-9 >= entryQty*(1-1e-6) }
	if ev.Reason == ExitReasonUnknown {
		if _, managed := x.ex.(types.ManagedOrderTrader); managed {
			rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
			if err == nil {
				exited := 0.0
				var latest *store.CopyTradeOrder
				for _, r := range rows {
					if r.ExecutedQty <= 0 || (!strings.HasPrefix(r.Role, "TP_") && !strings.HasPrefix(r.Role, "CLOSE_")) {
						continue
					}
					exited += r.ExecutedQty
					if latest == nil || r.UpdatedAt.After(latest.UpdatedAt) {
						latest = r
					}
				}
				if latest != nil && explains(exited) {
					ev.ClosingRole, ev.Source = latest.Role, "order_ledger"
					ev.Reason = exitReasonForRole(latest.Role)
					if !latest.UpdatedAt.IsZero() {
						t := latest.UpdatedAt.UTC()
						ev.ClosedAt, ev.ClosedAtSource = &t, "ledger_fill_observed"
					}
				}
			}
		} else {
			x.refreshTPProgress(traceID, ctx)
			filled := 0.0
			for _, tp := range readTPPlan(ctx) {
				filled += tpCumulativeFill(tp)
			}
			if filled > 0 && explains(filled) {
				ev.Reason, ev.Source = ExitReasonTakeProfit, "tp_plan"
			}
		}
	}

	// 3. Realized PnL (and the venue's own close time / type) when the
	// exchange reports closed positions for this symbol since the open.
	x.attachClosedPnL(ctx, &ev)

	if ev.ClosedAt == nil {
		now := time.Now().UTC()
		ev.ClosedAt, ev.ClosedAtSource = &now, "reconcile_observed"
	}
	return ev
}

func exitReasonForRole(role string) string {
	switch {
	case strings.HasPrefix(role, "TP_"):
		return ExitReasonTakeProfit
	case strings.HasPrefix(role, "CLOSE_emergency-"), strings.HasPrefix(role, "CLOSE_risk-"):
		return ExitReasonSystem
	case strings.HasPrefix(role, "CLOSE_"):
		return ExitReasonAuthor
	}
	return ExitReasonUnknown
}

// attachClosedPnL sums the venue's closed-position records for this symbol
// since the trade opened. It refuses attribution when another copy trade
// shares the symbol or the page may be truncated; it never adds a venue API.
func (x *Executor) attachClosedPnL(ctx *store.CopyTradeContext, ev *exitEvidence) {
	since := ctx.CreatedAt
	if ctx.OpenedAt != nil {
		since = *ctx.OpenedAt
	}
	if since.IsZero() {
		return
	}
	if active, err := x.st.CopyTrade().GetActiveContexts(x.traderID); err != nil {
		return
	} else {
		for _, o := range active {
			if o.ID != ctx.ID && o.Symbol == ctx.Symbol {
				ev.PnLNote = "another active trade shares this symbol; realized PnL not attributed"
				return
			}
		}
	}
	records, err := x.ex.GetClosedPnL(since, closedPnLLimit)
	if err != nil {
		ev.PnLNote = "closed PnL lookup failed"
		return
	}
	if len(records) >= closedPnLLimit {
		ev.PnLNote = "closed PnL page may be truncated; realized PnL not attributed"
		return
	}
	total, n := 0.0, 0
	var last *types.ClosedPnLRecord
	now := time.Now().UTC()
	for i := range records {
		r := &records[i]
		if !strings.EqualFold(r.Symbol, ctx.Symbol) || r.ExitTime.Before(since) || r.ExitTime.After(now.Add(time.Minute)) {
			continue
		}
		total += r.RealizedPnL
		n++
		if last == nil || r.ExitTime.After(last.ExitTime) {
			last = r
		}
	}
	if n == 0 {
		ev.PnLNote = "no closed PnL record yet for this symbol (venue history may lag)"
		return
	}
	ev.RealizedPnL, ev.PnLSource = &total, "exchange_closed_pnl_symbol_window"
	if !last.ExitTime.IsZero() {
		t := last.ExitTime.UTC()
		ev.ClosedAt, ev.ClosedAtSource = &t, "exchange_close_time"
	}
	if ev.Reason == ExitReasonUnknown {
		switch last.CloseType {
		case "stop_loss":
			ev.Reason, ev.Source = ExitReasonStopLoss, "exchange_close_type"
		case "take_profit":
			ev.Reason, ev.Source = ExitReasonTakeProfit, "exchange_close_type"
		}
	}
}
