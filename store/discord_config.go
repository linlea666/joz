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
// message polling (single row, always ID=1). All copy-trading traders share
// this credential. The token is encrypted at rest (crypto.EncryptedString).
type DiscordConfig struct {
	ID                  uint                   `gorm:"primaryKey"`
	Token               crypto.EncryptedString `gorm:"column:token;default:''"`
	PollIntervalSeconds int                    `gorm:"column:poll_interval_seconds;default:6"`
	Enabled             bool                   `gorm:"column:enabled;default:true"`
	// Token status monitoring (email alert when the token goes 401/403).
	AlertEmail             string `gorm:"column:alert_email;default:''"`
	MonitorEnabled         bool   `gorm:"column:monitor_enabled;default:true"`
	MonitorIntervalSeconds int    `gorm:"column:monitor_interval_seconds;default:60"`
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (DiscordConfig) TableName() string { return "discord_configs" }

// String returns a masked representation (never prints the token).
func (dc DiscordConfig) String() string {
	token := "***"
	if dc.Token == "" {
		token = "<not set>"
	}
	return fmt.Sprintf("DiscordConfig{ID:%d, Token:%s, PollInterval:%ds, Enabled:%v}",
		dc.ID, token, dc.PollIntervalSeconds, dc.Enabled)
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
	Token                  string
	PollIntervalSeconds    int
	Enabled                *bool
	AlertEmail             *string
	MonitorEnabled         *bool
	MonitorIntervalSeconds int
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
	cfg.ID = 1
	if u.Token != "" {
		cfg.Token = crypto.EncryptedString(u.Token)
	}
	if u.PollIntervalSeconds > 0 {
		cfg.PollIntervalSeconds = u.PollIntervalSeconds
	} else if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 6
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
	if u.MonitorIntervalSeconds > 0 {
		cfg.MonitorIntervalSeconds = u.MonitorIntervalSeconds
	} else if cfg.MonitorIntervalSeconds <= 0 {
		cfg.MonitorIntervalSeconds = 60
	}
	return s.db.Save(&cfg).Error
}

// ClearToken removes the stored token.
func (s *DiscordConfigStore) ClearToken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Model(&DiscordConfig{}).Where("id = 1").Update("token", "").Error
}

// HasToken reports whether a token is configured.
func (s *DiscordConfigStore) HasToken() (bool, error) {
	cfg, err := s.Get()
	if err != nil {
		return false, err
	}
	return cfg != nil && cfg.Token != "", nil
}
