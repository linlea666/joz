package copytrader

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// TraderType distinguishes the autonomous market-scan trader from the
// Discord copy-trading trader. The two modes are mutually exclusive.
type TraderType string

const (
	TraderTypeAIScan TraderType = "ai_scan"
	TraderTypeCopy   TraderType = "copy_trading"
)

// RiskMode determines how position size is derived.
type RiskMode string

const (
	// RiskModeByLoss ("以损定量"): qty = riskAmount / |entry - stopLoss|.
	RiskModeByLoss RiskMode = "by_loss"
	// RiskModePercent: margin = equity * riskAmount%, qty = margin*leverage/entry.
	RiskModePercent RiskMode = "percent"
	// RiskModeFixed: margin = riskAmount USD, qty = margin*leverage/entry.
	RiskModeFixed RiskMode = "fixed"
)

// CopyTradingConfig is the per-trader copy-trading configuration.
// Stored as JSON in traders.copy_trading_config; parse via ParseCopyTradingConfig.
type CopyTradingConfig struct {
	// Signal source
	PrimaryChannelID string `json:"primary_channel_id"`
	// SourceChannelIDs is RESERVED and not wired anywhere yet: extra channels
	// feeding the same trades (multi-channel authors). Kept in the schema so
	// stored configs stay forward-compatible; setting it has NO effect in V1.
	SourceChannelIDs []string `json:"source_channel_ids,omitempty"`
	SourceAuthorIDs  []string `json:"source_author_ids,omitempty"` // empty = accept all authors
	ChannelNotes     string   `json:"channel_notes,omitempty"`     // free-text channel profile injected into the prompt

	// AI parsing
	ParseImages          bool `json:"parse_images"`
	SendPositionSnapshot bool `json:"send_position_snapshot"`
	SignalContextEnabled bool `json:"signal_context_enabled"`
	ContextLookbackDays  int  `json:"context_lookback_days"`
	// ReasoningEffort is RESERVED and not wired into the LLM layer yet
	// ("", "low", "medium", "high"). Setting it has NO effect in V1.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Risk
	RiskMode               RiskMode `json:"risk_mode"`
	RiskAmountUSD          float64  `json:"risk_amount_usd"`           // by_loss: USD risk; percent: % of equity; fixed: margin USD
	MaxPositionNotionalUSD float64  `json:"max_position_notional_usd"` // hard cap on single-position notional (0 = default)
	MaxOpenPositions       int      `json:"max_open_positions"`
	MajorLeverage          int      `json:"major_leverage"`
	AltcoinLeverage        int      `json:"altcoin_leverage"`

	// Entry
	MajorPriceOffsetPct   float64 `json:"major_price_offset_pct"`   // market entry deviation threshold for BTC/ETH (0 = disabled)
	AltcoinPriceOffsetPct float64 `json:"altcoin_price_offset_pct"` // same for other coins (0 = disabled)
	LimitToMarketWithin   bool    `json:"limit_to_market_within_threshold"`
	OpenSignalTTLSeconds  int     `json:"open_signal_ttl_seconds"`
	MgmtSignalTTLSeconds  int     `json:"management_signal_ttl_seconds"`
	EntryTimeoutMinutes   int     `json:"entry_timeout_minutes"` // cancel unfilled limit entries after this long (0 = never)

	// Exit. Stop-loss is always required for live opens (not configurable).
	DefaultTPRatios      string `json:"default_tp_ratios"` // e.g. "50,30,20" — allocation when the author gives multiple TPs without ratios
	AutoBreakevenAfterTP bool   `json:"auto_breakeven_after_tp"`

	// Safety
	DuplicateOpenProtection bool `json:"duplicate_open_protection"`
	Paused                  bool `json:"paused"` // emergency pause: blocks OPEN/ADD, still allows CLOSE/UPDATE_SL
}

// DefaultCopyTradingConfig returns the recommended defaults
// (mirrors the reference project's proven configuration).
func DefaultCopyTradingConfig() CopyTradingConfig {
	return CopyTradingConfig{
		ParseImages:             true,
		SendPositionSnapshot:    true,
		SignalContextEnabled:    true,
		ContextLookbackDays:     5,
		RiskMode:                RiskModeByLoss,
		RiskAmountUSD:           50,
		MaxPositionNotionalUSD:  30000,
		MaxOpenPositions:        5,
		MajorLeverage:           20,
		AltcoinLeverage:         10,
		MajorPriceOffsetPct:     0.3,
		AltcoinPriceOffsetPct:   1.0,
		LimitToMarketWithin:     true,
		OpenSignalTTLSeconds:    300,
		MgmtSignalTTLSeconds:    1800,
		EntryTimeoutMinutes:     240,
		DefaultTPRatios:         "50,30,20",
		AutoBreakevenAfterTP:    false,
		DuplicateOpenProtection: true,
	}
}

// ParseCopyTradingConfig decodes the stored JSON, applying defaults for
// fields absent from the payload.
func ParseCopyTradingConfig(raw string) (*CopyTradingConfig, error) {
	cfg := DefaultCopyTradingConfig()
	if strings.TrimSpace(raw) == "" {
		return &cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("invalid copy trading config JSON: %w", err)
	}
	return &cfg, nil
}

// Encode serializes the config for storage.
func (c *CopyTradingConfig) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Validate checks the configuration for a copy-trading trader.
func (c *CopyTradingConfig) Validate() error {
	if strings.TrimSpace(c.PrimaryChannelID) == "" {
		return fmt.Errorf("primary_channel_id is required")
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(c.PrimaryChannelID), 10, 64); err != nil {
		return fmt.Errorf("primary_channel_id must be a numeric Discord channel id")
	}
	switch c.RiskMode {
	case RiskModeByLoss, RiskModePercent, RiskModeFixed:
	default:
		return fmt.Errorf("invalid risk_mode: %q", c.RiskMode)
	}
	if c.RiskAmountUSD <= 0 {
		return fmt.Errorf("risk_amount_usd must be > 0")
	}
	if c.RiskMode == RiskModePercent && c.RiskAmountUSD > 100 {
		return fmt.Errorf("percent risk mode requires risk_amount_usd <= 100 (it is a percentage)")
	}
	if c.MajorLeverage <= 0 || c.AltcoinLeverage <= 0 {
		return fmt.Errorf("leverage must be > 0")
	}
	if c.MajorLeverage > 125 || c.AltcoinLeverage > 125 {
		return fmt.Errorf("leverage must be <= 125")
	}
	if c.MaxOpenPositions <= 0 {
		return fmt.Errorf("max_open_positions must be > 0")
	}
	if c.ContextLookbackDays < 0 || c.ContextLookbackDays > 30 {
		return fmt.Errorf("context_lookback_days must be within 0-30")
	}
	if c.MajorPriceOffsetPct < 0 || c.AltcoinPriceOffsetPct < 0 {
		return fmt.Errorf("price offset thresholds must be >= 0 (0 disables the tolerance)")
	}
	if _, err := ParseTPRatios(c.DefaultTPRatios); err != nil {
		return err
	}
	return nil
}

// LeverageFor returns the configured leverage for a canonical symbol.
func (c *CopyTradingConfig) LeverageFor(canonicalSymbol string) int {
	if IsMajorSymbol(canonicalSymbol) {
		return c.MajorLeverage
	}
	return c.AltcoinLeverage
}

// PriceOffsetPctFor returns the market-entry deviation threshold (percent)
// for a canonical symbol. 0 means the threshold is disabled.
func (c *CopyTradingConfig) PriceOffsetPctFor(canonicalSymbol string) float64 {
	if IsMajorSymbol(canonicalSymbol) {
		return c.MajorPriceOffsetPct
	}
	return c.AltcoinPriceOffsetPct
}

// ParseTPRatios parses "50,30,20" into ratio values, validating each entry
// and the total (must be >0 and <=100, at most 3 levels in V1).
func ParseTPRatios(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []float64{50, 30, 20}, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) > 3 {
		return nil, fmt.Errorf("default_tp_ratios supports at most 3 levels, got %d", len(parts))
	}
	ratios := make([]float64, 0, len(parts))
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid TP ratio %q", p)
		}
		if v <= 0 || v > 100 {
			return nil, fmt.Errorf("TP ratio %v out of range (0, 100]", v)
		}
		ratios = append(ratios, v)
		total += v
	}
	if total > 100+1e-9 {
		return nil, fmt.Errorf("TP ratios sum to %.2f, must not exceed 100", total)
	}
	return ratios, nil
}
