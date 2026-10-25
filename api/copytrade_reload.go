package api

import (
	"nofx/copytrader"
	"nofx/discord"
	"nofx/store"
)

// copyTradeRouteReloader is the part of the Discord source a restart needs.
type copyTradeRouteReloader interface {
	PrepareReload(traderID, executionKey string)
}

// copyTradeReloadSource is overridable in tests.
var copyTradeReloadSource = func() copyTradeRouteReloader {
	if s := discord.Global(); s != nil {
		return s
	}
	return nil
}

// prepareCopyTradeReload must be called before RemoveTrader when a copy-trading
// trader is restarted only to pick up other settings (AI model, exchange
// credentials). When its stored copy-trading execution settings are unchanged,
// the Discord route keeps its generation and activation time, so cards posted
// before the restart remain eligible and unsubmitted entry legs are not
// revoked. A changed or unreadable config is left to revoke as before.
func prepareCopyTradeReload(t *store.Trader) {
	if t == nil || t.TraderType != string(copytrader.TraderTypeCopy) {
		return
	}
	source := copyTradeReloadSource()
	if source == nil {
		return
	}
	cfg, err := copytrader.ParseCopyTradingConfig(t.CopyTradingConfig)
	if err != nil {
		return
	}
	source.PrepareReload(t.ID, cfg.ExecutionKey())
}
