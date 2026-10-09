package store

import (
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nofx/crypto"
)

// EmailConfig deliberately stores only ciphertext, so ORM diagnostics cannot
// log a plaintext password. Recipients and the switch remain in DiscordConfig.
type EmailConfig struct {
	ID         uint   `gorm:"primaryKey" json:"-"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Security   string `json:"security"`
	User       string `json:"user"`
	Ciphertext string `json:"-"`
}

func (s *DiscordConfigStore) GetEmail() (*EmailConfig, string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cfg EmailConfig
	var recipient string
	enabled := true
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var dc DiscordConfig
		if err := tx.Select("alert_email", "monitor_enabled").First(&dc, 1).Error; err == nil {
			recipient, enabled = dc.AlertEmail, dc.MonitorEnabled
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.First(&cfg, 1).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, recipient, enabled, nil
	}
	if err != nil {
		return nil, "", false, errors.New("failed to read email configuration")
	}
	return &cfg, recipient, enabled, nil
}

// SaveEmail changes neither the collector revision nor its execution boundary.
func (s *DiscordConfigStore) SaveEmail(cfg EmailConfig, password, recipient string, enabled bool) error {
	ciphertext, err := crypto.EncryptRequired(password)
	if err != nil {
		return err
	}
	cfg.ID, cfg.Ciphertext = 1, ciphertext
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&cfg).Error; err != nil {
			return err
		}
		// No credential serialization here: preserve existing token and runtime data.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&DiscordConfig{
			ID: 1, RunMode: "observe", Enabled: true, MonitorEnabled: true,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&DiscordConfig{}).Where("id = ?", 1).Updates(map[string]interface{}{
			"alert_email": recipient, "monitor_enabled": enabled,
		}).Error
	})
	if err != nil {
		return errors.New("failed to save email configuration")
	}
	return nil
}
