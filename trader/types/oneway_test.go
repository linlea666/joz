package types

import (
	"reflect"
	"testing"
)

func TestClassifyOneWayFill(t *testing.T) {
	for _, c := range []struct {
		name                 string
		side                 string
		qty, pnl, long, shrt float64
		want                 []OneWayLeg
	}{
		{"flat buy opens", "BUY", 1, 0, 0, 0, []OneWayLeg{{"open_long", 1}}},
		{"flat sell opens", "sell", 1, 0, 0, 0, []OneWayLeg{{"open_short", 1}}},
		{"add to long", "BUY", 1, 0, 2, 0, []OneWayLeg{{"open_long", 1}}},
		{"zero pnl close", "SELL", 1, 0, 2, 0, []OneWayLeg{{"close_long", 1}}},
		{"reverse", "BUY", 3, 4, 0, 1, []OneWayLeg{{"close_short", 1}, {"open_long", 2}}},
		{"exact close", "BUY", 1, 4, 0, 1, []OneWayLeg{{"close_short", 1}}},
		{"pnl closes untracked", "SELL", 1, -1, 0, 0, []OneWayLeg{{"close_long", 1}}},
		{"pnl against same side", "SELL", 1, 1, 0, 2, nil},
		{"both sides open", "BUY", 1, 0, 1, 1, nil},
		{"bad side", "HOLD", 1, 0, 0, 0, nil},
		{"bad qty", "BUY", 0, 0, 0, 0, nil},
	} {
		if got := ClassifyOneWayFill(c.side, c.qty, c.pnl, c.long, c.shrt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}
