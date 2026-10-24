package binance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
)

func positionModeTrader(t *testing.T, handler http.HandlerFunc) *FuturesTrader {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := futures.NewClient("test", "test")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	return &FuturesTrader{client: client}
}

func TestIsDualSidePosition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
	}{
		{"hedge", 200, `{"dualSidePosition":true}`, true, false},
		{"one-way", 200, `{"dualSidePosition":false}`, false, false},
		{"server error", 500, `{"code":-1000,"msg":"offline"}`, false, true},
		{"auth error", 401, `{"code":-2015,"msg":"Invalid API-key"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := positionModeTrader(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/fapi/v1/positionSide/dual" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			got, err := tr.IsDualSidePosition(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestIsDualSidePositionNilClient(t *testing.T) {
	if _, err := (&FuturesTrader{}).IsDualSidePosition(context.Background()); err == nil {
		t.Fatal("expected error for nil client")
	}
}
