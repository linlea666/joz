package trader

import (
	"fmt"

	"nofx/copytrader"
	"nofx/discord"
	"nofx/store"
)

// IsCopyTrading reports whether this trader runs in Discord copy-trading mode.
func (at *AutoTrader) IsCopyTrading() bool {
	return at.config.TraderType == string(copytrader.TraderTypeCopy)
}

// CopyEngine returns the running copy-trading engine (nil when the trader is
// stopped or not a copy-trading trader).
func (at *AutoTrader) CopyEngine() *copytrader.Engine {
	at.copyLifecycleMu.Lock()
	defer at.copyLifecycleMu.Unlock()
	return at.copyEngine
}

// runCopyTradingMode replaces the market-scan loop for copy-trading traders:
// it starts the copytrader engine (poller subscription + reconcile loop) and
// blocks until Stop() is called. No scan cycles, no kernel decisions.
func (at *AutoTrader) runCopyTradingMode() error {
	cfg, err := copytrader.ParseCopyTradingConfig(at.config.CopyTradingConfig)
	if err != nil {
		return fmt.Errorf("invalid copy trading config: %w", err)
	}
	if err := cfg.ValidateExchange(at.config.Exchange); err != nil {
		return fmt.Errorf("copy trading config validation failed: %w", err)
	}

	poller := discord.Global()
	if poller == nil {
		return fmt.Errorf("Discord source not initialized")
	}

	engine := copytrader.NewEngine(copytrader.EngineParams{
		TraderID:   at.id,
		TraderName: at.name,
		UserID:     at.userID,
		Config:     cfg,
		Store:      at.store,
		LLM:        at.mcpClient,
		ModelID:    at.config.CustomModelName,
		Provider:   at.aiModel,
		Exchange:   at.trader,
		Source:     poller,
	})
	at.copyLifecycleMu.Lock()
	if err := engine.Start(); err != nil {
		at.copyLifecycleMu.Unlock()
		return fmt.Errorf("copy trading engine start failed: %w", err)
	}
	at.copyEngine = engine
	at.startBinanceHistorySync()
	at.copyLifecycleMu.Unlock()

	at.logInfof("🎯 Copy trading mode active: channel %s, risk mode %s ($%.2f), leverage %d/%dx",
		cfg.PrimaryChannelID, cfg.RiskMode, cfg.RiskAmountUSD, cfg.MajorLeverage, cfg.AltcoinLeverage)

	// Block until stop (mirrors the scan loop's lifecycle contract).
	<-at.stopMonitorCh
	at.copyLifecycleMu.Lock()
	engine.Stop()
	at.copyEngine = nil
	at.copyLifecycleMu.Unlock()
	at.logInfof("⏹ Copy trading mode stopped")
	return nil
}

// Serializes manual review with engine start/stop; a stopped trader remains stopped.
func (at *AutoTrader) ReviewCopyRecovery(contextID, actor, ticket, reason string) (*store.CopyTradeRecoveryReview, error) {
	at.copyLifecycleMu.Lock()
	defer at.copyLifecycleMu.Unlock()
	if !at.IsCopyTrading() {
		return nil, fmt.Errorf("not a copy-trading trader")
	}
	binding, err := at.store.CopyTrade().TraderExchangeID(at.id)
	if err != nil {
		return nil, err
	}
	if binding != at.exchangeID {
		return nil, fmt.Errorf("runtime account binding changed; reload configuration")
	}
	if at.copyEngine != nil {
		return at.copyEngine.RecoveryReview(contextID, actor, ticket, reason)
	}
	return copytrader.NewExecutor(at.id, at.trader, at.store, nil).RecoveryReview(contextID, actor, ticket, reason)
}
