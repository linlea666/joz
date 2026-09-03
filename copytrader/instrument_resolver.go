package copytrader

import (
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsupportedInstrument marks symbols that must never reach the exchange
// API (TradFi futures, stocks, forex, indices...). The reference project hit
// repeated exchange errors ("bitget does not have market symbol NQ/USDT:USDT")
// because this check was missing; we resolve before execution.
type ErrUnsupportedInstrument struct {
	Raw    string
	Reason string
}

func (e *ErrUnsupportedInstrument) Error() string {
	return fmt.Sprintf("unsupported instrument %q: %s", e.Raw, e.Reason)
}

// nonCryptoSymbols are common TradFi symbols seen in signal channels
// (jonzi posts "NQ short (Tradfi)" cards, for example).
var nonCryptoSymbols = map[string]string{
	// index futures
	"NQ": "index future", "ES": "index future", "YM": "index future",
	"RTY": "index future", "MNQ": "index future", "MES": "index future",
	// indices
	"SPX": "index", "NDX": "index", "DJI": "index", "DXY": "index",
	"US30": "index", "US100": "index", "US500": "index", "GER40": "index",
	// commodities
	"CL": "commodity", "GC": "commodity", "SI": "commodity", "NG": "commodity",
	"XAU": "metal", "XAG": "metal", "XAUUSD": "metal", "XAGUSD": "metal",
	"GOLD": "metal", "SILVER": "metal", "USOIL": "commodity", "UKOIL": "commodity",
}

// forexPairPattern matches classic 6-letter fiat pairs (EURUSD, GBPJPY...).
var forexPairPattern = regexp.MustCompile(`^(EUR|GBP|USD|JPY|AUD|NZD|CAD|CHF|CNH|SGD|HKD|MXN|ZAR|TRY|SEK|NOK)(EUR|GBP|USD|JPY|AUD|NZD|CAD|CHF|CNH|SGD|HKD|MXN|ZAR|TRY|SEK|NOK)$`)

var symbolCleaner = strings.NewReplacer(
	"$", "",
	" ", "",
	"#", "",
)

// ResolveInstrument converts a raw symbol as stated by the channel author
// ("BTC", "btc/usdt", "BTCUSDT.P", "ZEC/USDT:USDT") into the project's
// canonical perp symbol format ("BTCUSDT"), or returns
// *ErrUnsupportedInstrument for non-crypto instruments.
//
// Exchange-level existence (does OKX list this perp?) is verified later by
// the executor capability check; this resolver handles semantic mapping.
func ResolveInstrument(raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return "", &ErrUnsupportedInstrument{Raw: raw, Reason: "empty symbol"}
	}
	s = symbolCleaner.Replace(s)

	// Strip common derivative decorations: "BTC/USDT:USDT", "BTCUSDT.P", "BTC-PERP"
	if idx := strings.Index(s, ":"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSuffix(s, ".P")
	s = strings.TrimSuffix(s, "PERP")
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.TrimSuffix(s, "SWAP")

	// Reduce to base asset for the blocklist check.
	base := s
	for _, suffix := range []string{"USDT", "USDC", "USD"} {
		if strings.HasSuffix(base, suffix) && len(base) > len(suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}

	if reason, blocked := nonCryptoSymbols[base]; blocked {
		return "", &ErrUnsupportedInstrument{Raw: raw, Reason: reason}
	}
	if reason, blocked := nonCryptoSymbols[s]; blocked {
		return "", &ErrUnsupportedInstrument{Raw: raw, Reason: reason}
	}
	if forexPairPattern.MatchString(s) {
		return "", &ErrUnsupportedInstrument{Raw: raw, Reason: "forex pair"}
	}
	if base == "" {
		return "", &ErrUnsupportedInstrument{Raw: raw, Reason: "empty base asset"}
	}

	return base + "USDT", nil
}

// IsMajorSymbol reports whether the canonical symbol uses major-coin
// leverage/thresholds (BTC/ETH), mirroring kernel's convention.
func IsMajorSymbol(canonical string) bool {
	return canonical == "BTCUSDT" || canonical == "ETHUSDT"
}
