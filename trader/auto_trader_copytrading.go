package trader

import (
	"fmt"

	"nofx/copytrader"
	"nofx/discord"
)

// IsCopyTrading reports whether this trader runs in Discord copy-trading mode.
func (at *AutoTrader) IsCopyTrading() bool {
	return at.config.TraderType == string(copytrader.TraderTypeCopy)
}

// CopyEngine returns the running copy-trading engine (nil when the trader is
// stopped or not a copy-trading trader).
func (at *AutoTrader) CopyEngine() *copytrader.Engine {
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
	if err := engine.Start(); err != nil {
		return fmt.Errorf("copy trading engine start failed: %w", err)
	}
	at.copyEngine = engine

	at.logInfof("🎯 Copy trading mode active: channel %s, risk mode %s ($%.2f), leverage %d/%dx",
		cfg.PrimaryChannelID, cfg.RiskMode, cfg.RiskAmountUSD, cfg.MajorLeverage, cfg.AltcoinLeverage)

	// Block until stop (mirrors the scan loop's lifecycle contract).
	<-at.stopMonitorCh
	engine.Stop()
	at.copyEngine = nil
	at.logInfof("⏹ Copy trading mode stopped")
	return nil
}
