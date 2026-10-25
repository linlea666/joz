package binance

import (
	"context"
	"fmt"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

// defaultSymbolRulesTTL bounds how long exchangeInfo filters are reused.
// exchangeInfo is several MB and weight-heavy; Binance changes filters rarely
// and announces them in advance, so a few minutes of reuse is safe.
const defaultSymbolRulesTTL = 5 * time.Minute

// cachedSymbolInfo holds one symbol's verified rules plus the raw filter
// strings the legacy precision helpers derive their decimals from.
type cachedSymbolInfo struct {
	rules    types.ManagedMarketRules
	stepSize string
	tickSize string
	at       time.Time
}

func (t *FuturesTrader) rulesTTL() time.Duration {
	if t.symbolRulesTTL > 0 {
		return t.symbolRulesTTL
	}
	return defaultSymbolRulesTTL
}

func (t *FuturesTrader) cachedSymbol(symbol string) (cachedSymbolInfo, bool) {
	t.symbolRulesMu.Lock()
	defer t.symbolRulesMu.Unlock()
	e, ok := t.symbolRules[symbol]
	if !ok || time.Since(e.at) >= t.rulesTTL() {
		return cachedSymbolInfo{}, false
	}
	return e, true
}

// InvalidateMarketRules drops one symbol's cached rules (all when empty).
func (t *FuturesTrader) InvalidateMarketRules(symbol string) {
	t.symbolRulesMu.Lock()
	defer t.symbolRulesMu.Unlock()
	if symbol == "" {
		t.symbolRules = nil
		return
	}
	delete(t.symbolRules, symbol)
}

// parseSymbolRules converts one exchangeInfo symbol. The error marks rules
// that must not be used for submission (and are therefore never cached).
func parseSymbolRules(s *futures.Symbol) (cachedSymbolInfo, error) {
	e := cachedSymbolInfo{}
	r := &e.rules
	*r = types.ManagedMarketRules{Symbol: s.Symbol, Status: s.Status, ContractType: string(s.ContractType), QuoteAsset: s.QuoteAsset}
	for _, f := range s.Filters {
		switch f["filterType"] {
		case "LOT_SIZE":
			r.QuantityStep = managedNumber(f["stepSize"])
			r.MinQuantity = managedNumber(f["minQty"])
			r.MaxQuantity = managedNumber(f["maxQty"])
			if v, ok := f["stepSize"].(string); ok {
				e.stepSize = v
			}
		case "MARKET_LOT_SIZE":
			r.MarketQuantityStep = managedNumber(f["stepSize"])
			r.MarketMinQuantity = managedNumber(f["minQty"])
			r.MarketMaxQuantity = managedNumber(f["maxQty"])
		case "PRICE_FILTER":
			r.PriceTick = managedNumber(f["tickSize"])
			r.MinPrice = managedNumber(f["minPrice"])
			r.MaxPrice = managedNumber(f["maxPrice"])
			if v, ok := f["tickSize"].(string); ok {
				e.tickSize = v
			}
		case "MIN_NOTIONAL":
			r.MinNotional = managedNumber(f["notional"])
		case "PERCENT_PRICE":
			r.PercentPriceUp = managedNumber(f["multiplierUp"])
			r.PercentPriceDown = managedNumber(f["multiplierDown"])
		}
	}
	if r.Status != "TRADING" || (r.ContractType != "PERPETUAL" && r.ContractType != "TRADIFI_PERPETUAL") || r.QuoteAsset != "USDT" || r.QuantityStep <= 0 || r.PriceTick <= 0 {
		return e, fmt.Errorf("%w: %s is not a supported tradable USDT perpetual or has incomplete rules (status=%s, contract_type=%s, quote_asset=%s)", types.ErrInstrumentUnavailable, s.Symbol, r.Status, r.ContractType, r.QuoteAsset)
	}
	return e, nil
}

// loadSymbol returns one symbol's rules, from cache when fresh. found=false
// means the symbol is not listed (or the request failed, err != nil). An error
// with found=true carries diagnostic rules that must not be used for orders.
// Only fully valid rules enter the cache; request failures and rejected
// symbols always refetch on the next call.
func (t *FuturesTrader) loadSymbol(symbol string) (cachedSymbolInfo, bool, error) {
	if e, ok := t.cachedSymbol(symbol); ok {
		return e, true, nil
	}
	// Serialize refreshes so concurrent misses share one exchangeInfo download.
	t.symbolRulesFetch.Lock()
	defer t.symbolRulesFetch.Unlock()
	if e, ok := t.cachedSymbol(symbol); ok {
		return e, true, nil
	}
	info, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return cachedSymbolInfo{}, false, err
	}
	now := time.Now()
	fresh := make(map[string]cachedSymbolInfo, len(info.Symbols))
	var target *cachedSymbolInfo
	var targetErr error
	for i := range info.Symbols {
		s := &info.Symbols[i]
		parsed, perr := parseSymbolRules(s)
		parsed.at = now
		if s.Symbol == symbol {
			p := parsed
			target, targetErr = &p, perr
		}
		if perr == nil {
			fresh[s.Symbol] = parsed
		}
	}
	t.symbolRulesMu.Lock()
	if t.symbolRules == nil {
		t.symbolRules = make(map[string]cachedSymbolInfo, len(fresh))
	}
	for k := range t.symbolRules {
		if _, ok := fresh[k]; !ok {
			delete(t.symbolRules, k) // delisted or no longer tradable
		}
	}
	for k, v := range fresh {
		t.symbolRules[k] = v
	}
	t.symbolRulesMu.Unlock()
	if target == nil {
		return cachedSymbolInfo{}, false, nil
	}
	return *target, true, targetErr
}
