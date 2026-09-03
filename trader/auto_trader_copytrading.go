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
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("copy trading config validation failed: %w", err)
	}

	poller := discord.Global()
	if poller == nil {
		return fmt.Errorf("discord poller not initialized")
	}
	// Make sure the poller runs and has the latest global token.
	if err := poller.Start(); err != nil {
		return fmt.Errorf("discord poller start failed: %w", err)
	}
	if poller.Client() == nil {
		if err := poller.ReloadConfig(); err != nil {
			return fmt.Errorf("discord config load failed: %w", err)
		}
		if poller.Client() == nil {
			return fmt.Errorf("Discord Token 未配置，请先在设置中配置全局 Discord 密钥")
		}
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
		Poller:     poller,
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
