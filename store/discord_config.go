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
	CreatedAt           time.Time
	UpdatedAt           time.Time
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

// Save upserts the config. An empty token keeps the existing one
// (same convention as AI model API keys).
func (s *DiscordConfigStore) Save(token string, pollIntervalSeconds int, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cfg DiscordConfig
	result := s.db.First(&cfg, 1)
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return result.Error
	}
	cfg.ID = 1
	if token != "" {
		cfg.Token = crypto.EncryptedString(token)
	}
	if pollIntervalSeconds > 0 {
		cfg.PollIntervalSeconds = pollIntervalSeconds
	} else if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 6
	}
	cfg.Enabled = enabled
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
