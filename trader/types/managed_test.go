package types

import (
	"errors"
	"testing"
)

func TestManagedNotSentAndRejectionSentinels(t *testing.T) {
	cause := errors.New("exchangeInfo timeout")
	err := NotSent(cause)
	for _, target := range []error{ErrManagedOrderNotSent, ErrManagedOrderRejected, ErrManagedOrderRetryable, cause} {
		if !errors.Is(err, target) {
			t.Fatalf("NotSent lost %v", target)
		}
	}
	if NotSent(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	definite := &ManagedRejection{Code: -4061, Cause: cause}
	if !errors.Is(definite, ErrManagedOrderRejected) || errors.Is(definite, ErrManagedOrderRetryable) || errors.Is(definite, ErrManagedOrderNotSent) {
		t.Fatal("definite rejection sentinels wrong")
	}
	transient := &ManagedRejection{Code: -1003, Cause: cause, Retryable: true}
	if !errors.Is(transient, ErrManagedOrderRejected) || !errors.Is(transient, ErrManagedOrderRetryable) || errors.Is(transient, ErrManagedOrderNotSent) {
		t.Fatal("transient rejection sentinels wrong")
	}
}

func TestLimitPriceAllowed(t *testing.T) {
	band := &ManagedMarketRules{MinPrice: .01, PercentPriceUp: 1.05, PercentPriceDown: .95}
	btc := &ManagedMarketRules{MinPrice: 556.8, MaxPrice: 4529764, PercentPriceUp: 1.05, PercentPriceDown: .95}
	cases := []struct {
		side             string
		price, reference float64
		want             bool
	}{
		{"SELL", 110, 100, true}, // long TP above mark
		{"SELL", 94, 100, false}, // below mark*multiplierDown
		{"SELL", 95, 100, true},  // boundary
		{"BUY", 90, 100, true},   // short TP below mark
		{"BUY", 106, 100, false}, // above mark*multiplierUp
		{"SELL", 94, 0, true},    // no reference: never block
	}
	for _, c := range cases {
		if got := band.LimitPriceAllowed(c.side, c.price, c.reference); got != c.want {
			t.Fatalf("%s %.8g ref %.8g = %v want %v", c.side, c.price, c.reference, got, c.want)
		}
	}
	cases = []struct {
		side             string
		price, reference float64
		want             bool
	}{
		{"BUY", 500, 83664, false},  // below static minPrice
		{"SELL", 5e6, 4e6, false},   // above static maxPrice
		{"BUY", 49236, 83664, true}, // documented filter does not bound passive BUY
	}
	for _, c := range cases {
		if got := btc.LimitPriceAllowed(c.side, c.price, c.reference); got != c.want {
			t.Fatalf("%s %.8g ref %.8g = %v want %v", c.side, c.price, c.reference, got, c.want)
		}
	}
	var none *ManagedMarketRules
	if !none.LimitPriceAllowed("SELL", 1, 100) || !(&ManagedMarketRules{}).LimitPriceAllowed("SELL", 1, 100) {
		t.Fatal("unknown rules must not block")
	}
}
