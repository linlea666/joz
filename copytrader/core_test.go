package copytrader

import (
	"testing"
	"time"
)

func TestResolveInstrument(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		blocked bool
	}{
		// Formats actually seen in the three test channels + reference logs
		{"BTC", "BTCUSDT", false},
		{"BTC/USDT", "BTCUSDT", false},
		{"BTC/USDT:USDT", "BTCUSDT", false},
		{"BTCUSDT.P", "BTCUSDT", false},
		{"btc-usdt-swap", "BTCUSDT", false},
		{"ZEC/USDT", "ZECUSDT", false},
		{"VELVET/USDT", "VELVETUSDT", false},
		{"EIGEN", "EIGENUSDT", false},
		{"$SOL", "SOLUSDT", false},
		{"eth_usdt", "ETHUSDT", false},
		// TradFi must never reach the exchange (reference project hit these)
		{"NQ", "", true},
		{"NQ/USDT:USDT", "", true},
		{"ES", "", true},
		{"SPX", "", true},
		{"XAUUSD", "", true},
		{"GOLD", "", true},
		{"EURUSD", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := ResolveInstrument(tt.raw)
		if tt.blocked {
			if err == nil {
				t.Errorf("ResolveInstrument(%q) = %q, want unsupported error", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveInstrument(%q) unexpected error: %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolveInstrument(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestStateMachineTransitions(t *testing.T) {
	legal := [][2]TradeState{
		{StateNew, StateEntryPending},
		{StateNew, StateOpen}, // market entry fills immediately
		{StateEntryPending, StateOpen},
		{StateEntryPending, StateCancelled},
		{StateEntryPending, StateExpired},
		{StateOpen, StateBreakeven},
		{StateOpen, StateClosePending},
		{StateOpen, StateClosed}, // SL/TP fill detected by reconcile
		{StateBreakeven, StateClosed},
		{StateClosePending, StateClosed},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("expected legal transition %s -> %s", tr[0], tr[1])
		}
	}
	illegal := [][2]TradeState{
		{StateClosed, StateOpen}, // stale event ordering: must never reopen
		{StateCancelled, StateOpen},
		{StateExpired, StateEntryPending},
		{StateInvalid, StateOpen},
		{StateNew, StateBreakeven},
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("expected illegal transition %s -> %s", tr[0], tr[1])
		}
	}
}

func TestActionApplicable(t *testing.T) {
	// CLOSE on already-closed trade => NOOP_ALREADY_FLAT (never an error).
	// This was the most frequent error in the reference project logs.
	ok, skip := ActionApplicable(StateClosed, ActionClose)
	if ok || skip != SkipAlreadyFlat {
		t.Errorf("CLOSE on CLOSED: got (%v, %s), want (false, NOOP_ALREADY_FLAT)", ok, skip)
	}
	// UPDATE_SL with no live position => SKIPPED_NO_POSITION
	ok, skip = ActionApplicable(StateClosed, ActionUpdateSL)
	if ok || skip != SkipNoPosition {
		t.Errorf("UPDATE_SL on CLOSED: got (%v, %s), want (false, SKIPPED_NO_POSITION)", ok, skip)
	}
	// CLOSE while entry pending => allowed (cancels the entry)
	ok, _ = ActionApplicable(StateEntryPending, ActionClose)
	if !ok {
		t.Error("CLOSE on ENTRY_PENDING should be applicable (cancel entry)")
	}
	// UPDATE_SL on open position => allowed
	ok, _ = ActionApplicable(StateOpen, ActionUpdateSL)
	if !ok {
		t.Error("UPDATE_SL on OPEN should be applicable")
	}
	// OPEN never applies to an existing context (duplicate protection)
	ok, skip = ActionApplicable(StateOpen, ActionOpen)
	if ok || skip != SkipDuplicate {
		t.Errorf("OPEN on OPEN: got (%v, %s), want (false, DUPLICATE_PROTECTION)", ok, skip)
	}
}

func TestSignalTTL(t *testing.T) {
	now := time.Now()
	if !IsExpired(now.Add(-10*time.Minute), now, 5*time.Minute) {
		t.Error("10min old OPEN signal with 5min TTL must be expired")
	}
	if IsExpired(now.Add(-1*time.Minute), now, 5*time.Minute) {
		t.Error("fresh signal must not be expired")
	}
	if IsExpired(now.Add(-24*time.Hour), now, 0) {
		t.Error("TTL 0 disables expiry")
	}
}

func TestCopyTradingConfig(t *testing.T) {
	// defaults round-trip
	cfg, err := ParseCopyTradingConfig("")
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if cfg.RiskMode != RiskModeByLoss || cfg.RiskAmountUSD != 50 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	// missing channel id fails validation
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation failure without channel id")
	}
	cfg.PrimaryChannelID = "1452344484103848026"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
	// non-numeric channel id rejected
	cfg.PrimaryChannelID = "not-a-channel"
	if err := cfg.Validate(); err == nil {
		t.Error("expected rejection of non-numeric channel id")
	}
	// partial JSON keeps defaults for absent fields
	cfg2, err := ParseCopyTradingConfig(`{"primary_channel_id":"123","risk_amount_usd":100}`)
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	if cfg2.RiskAmountUSD != 100 || cfg2.ContextLookbackDays != 5 {
		t.Errorf("partial parse wrong: %+v", cfg2)
	}
	// leverage/ratio validation
	if _, err := ParseTPRatios("50,30,20"); err != nil {
		t.Errorf("valid ratios rejected: %v", err)
	}
	if _, err := ParseTPRatios("60,50"); err == nil {
		t.Error("ratios summing over 100 must be rejected")
	}
	if _, err := ParseTPRatios("10,20,30,40"); err == nil {
		t.Error("more than 3 ratios must be rejected")
	}
}

func TestValidateInterpretationOpen(t *testing.T) {
	base := func() *SourceInterpretation {
		return &SourceInterpretation{
			Classification: ClassificationSignal,
			Action:         ActionOpen,
			Symbol:         "BTC",
			Direction:      DirectionLong,
			EntryOrders:    []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 77005.1}}},
			StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 76851.3}}},
			TakeProfitLevels: []TPLevel{
				{Price: PriceSpec{Type: PriceFixed, Price: 77774}},
			},
		}
	}

	// jonzi's real card values must pass
	if skip, err := ValidateInterpretation(base(), 77000); skip != SkipNone || err != nil {
		t.Errorf("valid open rejected: skip=%s err=%v", skip, err)
	}

	// missing SL => risk-rejected skip (require-SL policy)
	noSL := base()
	noSL.StopLossLevels = nil
	if skip, _ := ValidateInterpretation(noSL, 77000); skip != SkipRiskRejected {
		t.Errorf("open without SL: skip=%s, want RISK_REJECTED", skip)
	}

	// SL on wrong side => hard error
	badSL := base()
	badSL.StopLossLevels = []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 78000}}}
	if _, err := ValidateInterpretation(badSL, 77000); err == nil {
		t.Error("long SL above entry must error")
	}

	// TP below entry for long => hard error
	badTP := base()
	badTP.TakeProfitLevels = []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 76000}}}
	if _, err := ValidateInterpretation(badTP, 77000); err == nil {
		t.Error("long TP below entry must error")
	}

	// OCR digit error (entry 7700 vs market 77000) => sanity skip
	ocr := base()
	ocr.EntryOrders = []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 7700}}}
	if skip, _ := ValidateInterpretation(ocr, 77000); skip != SkipSanityCheck {
		t.Errorf("digit error: skip=%s, want SANITY_CHECK_FAILED", skip)
	}

	// NEEDS_CONTEXT classification passes straight through as a skip
	nc := &SourceInterpretation{Classification: ClassificationNeedsContext}
	if skip, _ := ValidateInterpretation(nc, 77000); skip != SkipNeedsContext {
		t.Errorf("needs context: skip=%s", skip)
	}

	// UPDATE_SL with breakeven spec is allowed (resolved at execution)
	be := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionUpdateSL,
		Symbol:         "BTC",
		StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceBreakeven}}},
	}
	if skip, err := ValidateInterpretation(be, 77000); skip != SkipNone || err != nil {
		t.Errorf("breakeven UPDATE_SL rejected: skip=%s err=%v", skip, err)
	}

	// UPDATE_SL with unsupported R-multiple spec => unsupported skip
	rm := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionUpdateSL,
		Symbol:         "BTC",
		StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceRMultiple, Offset: 2}}},
	}
	if skip, _ := ValidateInterpretation(rm, 77000); skip != SkipUnsupportedPriceSpec {
		t.Errorf("R-multiple SL: skip=%s, want UNSUPPORTED_PRICE_SPEC", skip)
	}
}
