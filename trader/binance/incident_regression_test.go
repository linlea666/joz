package binance

import (
	"errors"
	"testing"

	"nofx/trader/types"

	"github.com/adshao/go-binance/v2/common"
)

func TestNoPositionStopIsDefiniteRejection(t *testing.T) {
	err := classifyManagedRejection(&common.APIError{Code: -4509, Message: "Time in Force (TIF) GTE can only be used with open positions."})
	if !errors.Is(err, types.ErrManagedOrderRejected) {
		t.Fatalf("explicit rejection classified uncertain: %v", err)
	}
	var original *common.APIError
	if !errors.As(err, &original) || original.Code != -4509 {
		t.Fatal("original code lost")
	}
	if errors.Is(classifyManagedRejection(errors.New("timeout")), types.ErrManagedOrderRejected) {
		t.Fatal("unknown marked rejected")
	}
}
