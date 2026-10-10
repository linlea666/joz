package main

import (
	"nofx/api"
	"nofx/auth"
	"nofx/config"
	"nofx/crypto"
	"nofx/discord"
	"nofx/logger"
	"nofx/manager"
	_ "nofx/mcp/payment"
	_ "nofx/mcp/provider"
	"nofx/store"
	"nofx/telemetry"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

func main() {
	// Local admin subcommands (account recovery) run directly against the
	// database and never start the HTTP server. Recovery therefore requires
	// shell/file access to the host instead of a network request, which keeps
	// it safe even when NOFX is exposed to the public internet. See cli.go.
	if runCLISubcommand(os.Args[1:]) {
		return
	}

	// Load .env environment variables
	_ = godotenv.Load()

	// Initialize logger
	logger.Init(nil)

	logger.Info("╔════════════════════════════════════════════════════════════╗")
	logger.Info("║           🚀 NOFX - AI-Powered Trading System              ║")
	logger.Info("╚════════════════════════════════════════════════════════════╝")

	// Initialize global configuration (loaded from .env).
	// MustInit refuses to start under an insecure config (e.g. missing or default JWT_SECRET).
	config.MustInit()
	cfg := config.Get()
	logger.Info("✅ Configuration loaded")

	// Initialize encryption service BEFORE database (so EncryptedString can decrypt on read)
	logger.Info("🔐 Initializing encryption service...")
	cryptoService, err := crypto.NewCryptoService()
	if err != nil {
		logger.Fatalf("❌ Failed to initialize encryption service: %v", err)
	}
	crypto.SetGlobalCryptoService(cryptoService)
	logger.Info("✅ Encryption service initialized successfully")

	// Initialize database from configuration
	// For backward compatibility: command line arg overrides config (SQLite only)
	if len(os.Args) > 1 {
		cfg.DBPath = os.Args[1]
	}
	// Ensure data directory exists (for SQLite)
	if cfg.DBType == "sqlite" {
		if dir := filepath.Dir(cfg.DBPath); dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				logger.Errorf("Failed to create data directory: %v", err)
			}
		}
	}

	logger.Infof("📋 Initializing database (%s)...", cfg.DBType)
	dbType := store.DBTypeSQLite
	if cfg.DBType == "postgres" {
		dbType = store.DBTypePostgres
	}
	st, err := store.NewWithConfig(store.DBConfig{
		Type:     dbType,
		Path:     cfg.DBPath,
		Host:     cfg.DBHost,
		Port:     cfg.DBPort,
		User:     cfg.DBUser,
		Password: cfg.DBPassword,
		DBName:   cfg.DBName,
		SSLMode:  cfg.DBSSLMode,
	})
	if err != nil {
		logger.Fatalf("❌ Failed to initialize database: %v", err)
	}
	defer st.Close()
	logStop := make(chan struct{})
	logDone := make(chan struct{})
	st.Logs().Record("server", "service.started", "info", "后端服务启动", nil)
	defer func() {
		close(logStop)
		<-logDone
		st.Logs().Record("server", "service.stopped", "info", "后端服务停止", nil)
	}()
	go func() {
		defer close(logDone)
		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()
		lastCleanup := time.Time{}
		for {
			select {
			case <-logStop:
				return
			case <-ticker.C:
				st.Logs().Flush()
				if time.Since(lastCleanup) >= time.Hour {
					if err := st.Logs().Retain(512 << 20); err != nil {
						st.Logs().Record("logs", "logs.retention.failed", "error", "系统日志清理失败", map[string]any{"code": "RETENTION_FAILED"})
					}
					lastCleanup = time.Now()
				}
			}
		}
	}()

	// Initialize installation ID for experience improvement (anonymous statistics)
	initInstallationID(st)

	// Set JWT secret
	auth.SetJWTSecret(cfg.JWTSecret)
	logger.Info("🔑 JWT secret configured")

	// WebSocket market monitor is NO LONGER USED
	// All K-line data now comes from CoinAnk API instead of Binance WebSocket cache
	// Commented out to reduce unnecessary connections:
	// go market.NewWSMonitor(150).Start(nil)
	// logger.Info("📊 WebSocket market monitor started")
	// time.Sleep(500 * time.Millisecond)
	logger.Info("📊 Using CoinAnk API for all market data (WebSocket cache disabled)")

	// Initialize the global Discord event source (copy-trading signal source).
	// Traders subscribe on start; collection begins only when a token is set.
	discordSource := discord.InitGlobal(st)
	if err := discordSource.Start(); err != nil {
		logger.Warnf("⚠️ Discord collector start failed: %v", err)
	}

	// Token status monitor: observes Gateway state and emails the configured
	// recipient when the token goes invalid (and again on recovery).
	discordMonitor := discord.InitGlobalMonitor(st, discordSource)
	discordMonitor.Start()

	// Daily retention cleanup for copy-trading data (events/signals/AI runs/
	// raw messages/media cache) so the tables never grow without bound.
	go runCopyTradeRetentionLoop(st)

	// Create TraderManager
	traderManager := manager.NewTraderManager()

	// Load all traders from database to memory (may auto-start traders with IsRunning=true)
	if err := traderManager.LoadTradersFromStore(st); err != nil {
		logger.Fatalf("❌ Failed to load traders: %v", err)
	}

	// Display loaded trader information
	traders, err := st.Trader().List("default")
	if err != nil {
		logger.Fatalf("❌ Failed to get trader list: %v", err)
	}

	logger.Info("🤖 AI Trader Configurations in Database:")
	if len(traders) == 0 {
		logger.Info("  (No trader configurations, please create via Web interface)")
	} else {
		for _, t := range traders {
			status := "❌ Stopped"
			if t.IsRunning {
				status = "✅ Running"
			}
			idShort := t.ID
			if len(idShort) > 8 {
				idShort = idShort[:8]
			}
			logger.Infof("  • %s [%s] %s - AI Model: %s, Exchange: %s",
				t.Name, idShort, status, t.AIModelID, t.ExchangeID)
		}
	}

	// Start API server
	server := api.NewServer(traderManager, st, cryptoService, cfg.APIServerPort)

	go func() {
		if err := server.Start(); err != nil {
			logger.Fatalf("❌ Failed to start API server: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("✅ System started successfully, waiting for trading commands...")
	logger.Info("📌 Tip: Use Ctrl+C to stop the system")

	<-quit
	logger.Info("📴 Shutdown signal received, closing system...")

	if err := server.Shutdown(); err != nil {
		logger.Warnf("⚠️ HTTP server shutdown error: %v", err)
	}
	logger.Info("✅ HTTP server stopped")

	// nofxiAgent.Stop() is handled by defer above

	// A process restart is not an operator disabling a trader. Preserve the
	// activation generation while draining engines; recovery uses saved plans.
	discordSource.PrepareShutdown()
	// Stop all traders
	traderManager.StopAll()

	// Stop Discord source and connection monitor
	discordMonitor.Stop()
	discordSource.Stop()
	logger.Info("✅ System shut down safely")
}

// retentionDays reads a retention override from the environment, falling back
// to the given default. Values <= 0 are rejected (retention is never "keep forever").
func retentionDays(envKey string, def int) int {
	if v := os.Getenv(envKey); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		logger.Warnf("⚠️ Invalid %s=%q, using default %d days", envKey, v, def)
	}
	return def
}

// runCopyTradeRetentionLoop prunes copy-trading data daily. Retention windows
// are configurable via environment variables; defaults:
//   - execution events / signals: 90 days
//   - AI runs (prompts + raw responses, the bulk of growth): 30 days
//   - raw Discord messages in terminal state: 30 days
//   - cached media files: 30 days
func runCopyTradeRetentionLoop(st *store.Store) {
	eventDays := retentionDays("COPYTRADE_EVENT_RETENTION_DAYS", 90)
	signalDays := retentionDays("COPYTRADE_SIGNAL_RETENTION_DAYS", 90)
	aiRunDays := retentionDays("COPYTRADE_AIRUN_RETENTION_DAYS", 30)
	messageDays := retentionDays("DISCORD_MESSAGE_RETENTION_DAYS", 30)
	mediaDays := retentionDays("DISCORD_MEDIA_RETENTION_DAYS", 30)

	runOnce := func() {
		if n, err := st.CopyTrade().CleanOldEvents(eventDays); err != nil {
			logger.Warnf("⚠️ CopyTrade retention: events cleanup failed: %v", err)
		} else if n > 0 {
			logger.Infof("🧹 CopyTrade retention: removed %d events older than %d days", n, eventDays)
		}
		if n, err := st.CopyTrade().CleanOldSignals(signalDays); err != nil {
			logger.Warnf("⚠️ CopyTrade retention: signals cleanup failed: %v", err)
		} else if n > 0 {
			logger.Infof("🧹 CopyTrade retention: removed %d signals older than %d days", n, signalDays)
		}
		if n, err := st.CopyTrade().CleanOldAIRuns(aiRunDays); err != nil {
			logger.Warnf("⚠️ CopyTrade retention: AI runs cleanup failed: %v", err)
		} else if n > 0 {
			logger.Infof("🧹 CopyTrade retention: removed %d AI runs older than %d days", n, aiRunDays)
		}
		if n, err := st.DiscordMessage().CleanOldIngest(signalDays); err != nil {
			logger.Warnf("Discord receipt retention failed: %v", err)
		} else if n > 0 {
			logger.Infof("Discord receipt retention removed %d receipts", n)
		}
		if n, err := st.DiscordMessage().CleanOldMessages(messageDays); err != nil {
			logger.Warnf("⚠️ CopyTrade retention: messages cleanup failed: %v", err)
		} else if n > 0 {
			logger.Infof("🧹 CopyTrade retention: removed %d terminal messages older than %d days", n, messageDays)
		}
		if protected, err := st.CopyTrade().HasProtectedMedia(); err != nil || protected {
			if err != nil {
				logger.Warn("Media retention skipped: protected evidence query failed")
			}
		} else if n, err := discord.CleanOldMedia(mediaDays); err != nil {
			logger.Warnf("⚠️ CopyTrade retention: media cleanup failed: %v", err)
		} else if n > 0 {
			logger.Infof("🧹 CopyTrade retention: removed %d cached media files older than %d days", n, mediaDays)
		}
	}

	// First pass shortly after boot (off the hot startup path), then daily.
	time.Sleep(2 * time.Minute)
	runOnce()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		runOnce()
	}
}

// initInstallationID initializes the anonymous installation ID for experience improvement
// This ID is persisted in database and used for anonymous usage statistics
func initInstallationID(st *store.Store) {
	const key = "installation_id"

	// Try to load from database
	installationID, err := st.GetSystemConfig(key)
	if err != nil {
		logger.Warnf("⚠️ Failed to load installation ID: %v", err)
	}

	// Generate new ID if not exists
	if installationID == "" {
		installationID = uuid.New().String()
		if err := st.SetSystemConfig(key, installationID); err != nil {
			logger.Warnf("⚠️ Failed to save installation ID: %v", err)
		}
		logger.Infof("📊 Generated new installation ID: %s", installationID[:8]+"...")
	}

	// Set installation ID in experience module
	telemetry.SetInstallationID(installationID)
}
