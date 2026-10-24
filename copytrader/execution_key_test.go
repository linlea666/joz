package copytrader

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func executionKeyBase() CopyTradingConfig {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	return cfg
}

// Spellings the runtime treats identically must not revoke a route.
func TestExecutionKeyIgnoresEquivalentSpellings(t *testing.T) {
	base := executionKeyBase()
	want := base.ExecutionKey()
	fifty := 50.0
	for name, mutate := range map[string]func(*CopyTradingConfig){
		"entry_policy legacy":           func(c *CopyTradingConfig) { c.EntryPolicy = EntryPolicyLegacy },
		"interpretation_profile":        func(c *CopyTradingConfig) { c.InterpretationProfile = "default" },
		"market_dual_price_mode legacy": func(c *CopyTradingConfig) { c.MarketDualPriceMode = DualPriceLegacy },
		"default_reduce_ratio 50":       func(c *CopyTradingConfig) { c.DefaultReduceRatio = &fifty },
		"source_mode channel":           func(c *CopyTradingConfig) { c.SourceMode = "channel" },
		"source_channel_ids primary":    func(c *CopyTradingConfig) { c.SourceChannelIDs = []string{"123"} },
		"source_channel_ids empty":      func(c *CopyTradingConfig) { c.SourceChannelIDs = []string{} },
		"channel_notes":                 func(c *CopyTradingConfig) { c.ChannelNotes = "wording only" },
		"reasoning_effort":              func(c *CopyTradingConfig) { c.ReasoningEffort = "high" },
		"all defaults filled by UI": func(c *CopyTradingConfig) {
			c.EntryPolicy, c.InterpretationProfile, c.MarketDualPriceMode, c.SourceMode = EntryPolicyLegacy, "default", DualPriceLegacy, "channel"
			c.DefaultReduceRatio, c.SourceChannelIDs, c.ChannelNotes = &fifty, []string{"123"}, "notes"
		},
	} {
		cfg := executionKeyBase()
		mutate(&cfg)
		if got := cfg.ExecutionKey(); got != want {
			t.Errorf("%s changed the execution key", name)
		}
	}
	// A JSON round trip through storage (omitempty drops empty values) is stable.
	raw, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCopyTradingConfig(raw)
	if err != nil || parsed.ExecutionKey() != want {
		t.Fatalf("stored config key drifted: %v", err)
	}
}

// Execution or source changes still revoke.
func TestExecutionKeyTracksExecutionSettings(t *testing.T) {
	baseCfg := executionKeyBase()
	want := baseCfg.ExecutionKey()
	forty := 40.0
	for name, mutate := range map[string]func(*CopyTradingConfig){
		"entry_policy":           func(c *CopyTradingConfig) { c.EntryPolicy = EntryPolicyParticipate },
		"interpretation_profile": func(c *CopyTradingConfig) { c.InterpretationProfile = "tyler_v1" },
		"market_dual_price_mode": func(c *CopyTradingConfig) { c.MarketDualPriceMode = DualPriceRange },
		"default_reduce_ratio":   func(c *CopyTradingConfig) { c.DefaultReduceRatio = &forty },
		"source_mode":            func(c *CopyTradingConfig) { c.SourceMode = "chroma" },
		"source_channel_ids":     func(c *CopyTradingConfig) { c.SourceChannelIDs = []string{"123", "456"} },
		"primary_channel_id":     func(c *CopyTradingConfig) { c.PrimaryChannelID = "999" },
		"source_author_ids":      func(c *CopyTradingConfig) { c.SourceAuthorIDs = []string{"42"} },
		"risk_amount_usd":        func(c *CopyTradingConfig) { c.RiskAmountUSD = 51 },
		"open_signal_ttl":        func(c *CopyTradingConfig) { c.OpenSignalTTLSeconds = 301 },
		"paused":                 func(c *CopyTradingConfig) { c.Paused = true },
		"parse_images":           func(c *CopyTradingConfig) { c.ParseImages = false },
	} {
		cfg := executionKeyBase()
		mutate(&cfg)
		if cfg.ExecutionKey() == want {
			t.Errorf("%s did not change the execution key", name)
		}
	}
}

// A config already stored in normalized form keeps the key it had before
// normalization was introduced (raw hash minus channel notes), so only
// non-normalized stored configs see a one-time key change.
func TestExecutionKeyUnchangedForNormalizedConfig(t *testing.T) {
	cfg := executionKeyBase()
	fifty := 50.0
	cfg.EntryPolicy, cfg.InterpretationProfile, cfg.MarketDualPriceMode, cfg.SourceMode = EntryPolicyLegacy, "default", DualPriceLegacy, "channel"
	cfg.DefaultReduceRatio, cfg.SourceChannelIDs = &fifty, []string{"123"}
	legacy := cfg
	legacy.ChannelNotes = ""
	b, _ := json.Marshal(legacy)
	if got, old := cfg.ExecutionKey(), fmt.Sprintf("%x", sha256.Sum256(b)); got != old {
		t.Fatalf("normalized config key changed: %s != %s", got, old)
	}
	// ExecutionKey must not mutate its receiver.
	plain := executionKeyBase()
	_ = plain.ExecutionKey()
	if plain.EntryPolicy != "" || plain.DefaultReduceRatio != nil || plain.SourceChannelIDs != nil {
		t.Fatalf("ExecutionKey mutated config: %+v", plain)
	}
}
