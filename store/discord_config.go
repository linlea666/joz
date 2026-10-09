package store

import (
	"errors"
	"fmt"
	"nofx/crypto"
	"sync"
	"time"

	"gorm.io/gorm"
)

// DiscordConfig stores the global Discord token used for copy-trading
// event collection (single row, always ID=1). All copy-trading traders share
// this credential. The token is encrypted at rest (crypto.EncryptedString).
type DiscordConfig struct {
	ID             uint                   `gorm:"primaryKey"`
	Token          crypto.EncryptedString `gorm:"column:token;default:''"`
	RunMode        string                 `gorm:"default:observe" json:"run_mode"`
	ExecutionSince *time.Time             `json:"execution_since,omitempty"`
	Revision       uint64                 `gorm:"default:0" json:"revision"`
	Enabled        bool                   `gorm:"column:enabled;default:true"`
	// Token status monitoring (email alert when the token goes 401/403).
	AlertEmail     string `gorm:"column:alert_email;default:''"`
	MonitorEnabled bool   `gorm:"column:monitor_enabled;default:true"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (DiscordConfig) TableName() string { return "discord_configs" }

// String returns a masked representation (never prints the token).
func (dc DiscordConfig) String() string {
	token := "***"
	if dc.Token == "" {
		token = "<not set>"
	}
	return fmt.Sprintf("DiscordConfig{ID:%d, Token:%s, Mode:%s, Enabled:%v}",
		dc.ID, token, dc.RunMode, dc.Enabled)
}

// DiscordConfigStore manages the global Discord credential.
type DiscordConfigStore struct {
	db *gorm.DB
	mu sync.RWMutex
}

// NewDiscordConfigStore creates a new DiscordConfigStore.
func NewDiscordConfigStore(db *gorm.DB) *DiscordConfigStore {
	return &DiscordConfigStore{db: db}
}

func (s *DiscordConfigStore) initTables() error {
	return s.db.AutoMigrate(&DiscordConfig{})
}

// Get returns the current config, or (nil, nil) when not configured yet.
func (s *DiscordConfigStore) Get() (*DiscordConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cfg DiscordConfig
	if err := s.db.First(&cfg, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &cfg, nil
}

// DiscordConfigUpdate carries a partial update for Save.
// Conventions: empty Token keeps the stored one (same as AI model API keys);
// numeric fields <= 0 keep the stored value (or fall back to the default);
// nil pointer fields keep the stored value.
type DiscordConfigUpdate struct {
	Token          string
	RunMode        string
	Enabled        *bool
	AlertEmail     *string
	MonitorEnabled *bool
}

// Save upserts the config, applying only the fields present in the update.
func (s *DiscordConfigStore) Save(u DiscordConfigUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cfg DiscordConfig
	result := s.db.First(&cfg, 1)
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return result.Error
	}
	isNew := errors.Is(result.Error, gorm.ErrRecordNotFound)
	if isNew {
		// First-time defaults (gorm zero values would disable everything).
		cfg.Enabled = true
		cfg.MonitorEnabled = true
	}
	wasEnabled := cfg.Enabled
	previousToken := cfg.Token
	cfg.ID = 1
	if u.Token != "" {
		cfg.Token = crypto.EncryptedString(u.Token)
	}
	if u.RunMode != "" && u.RunMode != "observe" && u.RunMode != "live" {
		return fmt.Errorf("run_mode must be observe or live")
	}
	if cfg.RunMode == "" {
		cfg.RunMode = "observe"
	}
	if u.RunMode != "" && u.RunMode != cfg.RunMode {
		cfg.RunMode = u.RunMode
		if u.RunMode == "live" {
			now := time.Now().UTC()
			cfg.ExecutionSince = &now
		} else {
			cfg.ExecutionSince = nil
		}
	}
	if u.Enabled != nil {
		cfg.Enabled = *u.Enabled
	}
	if u.AlertEmail != nil {
		cfg.AlertEmail = *u.AlertEmail
	}
	if u.MonitorEnabled != nil {
		cfg.MonitorEnabled = *u.MonitorEnabled
	}
	if cfg.RunMode == "live" && cfg.Enabled && (!wasEnabled || (previousToken != "" && previousToken != cfg.Token)) {
		now := time.Now().UTC()
		cfg.ExecutionSince = &now
	}
	cfg.Revision++
	return s.db.Save(&cfg).Error
}

// ClearToken removes the stored token.
func (s *DiscordConfigStore) ClearToken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Model(&DiscordConfig{}).Where("id = 1").Updates(map[string]interface{}{"token": "", "run_mode": "observe", "execution_since": nil, "revision": gorm.Expr("revision + 1")}).Error
}

// HasToken reports whether a token is configured.
func (s *DiscordConfigStore) HasToken() (bool, error) {
	cfg, err := s.Get()
	if err != nil {
		return false, err
	}
	return cfg != nil && cfg.Token != "", nil
}
