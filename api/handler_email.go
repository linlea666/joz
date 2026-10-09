package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"nofx/config"
	"nofx/crypto"
	"nofx/notify"
	"nofx/store"
)

func (s *Server) handleGetDiscordEmail(c *gin.Context) {
	settings, err := notify.ResolveEmail(s.store)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, settings) // Pass is excluded by its JSON tag.
}

// decodeEmail accepts the existing non-secret email-only test request, while
// enforcing transport encryption on every configuration/draft request.
func (s *Server) decodeEmail(c *gin.Context, target interface{}, legacyTest bool) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("invalid email request")
	}
	var envelope crypto.EncryptedPayload
	if json.Unmarshal(raw, &envelope) != nil {
		return errors.New("invalid email request")
	}
	if envelope.WrappedKey != "" {
		if s.cryptoHandler == nil || s.cryptoHandler.cryptoService == nil {
			return errors.New("transport encryption unavailable")
		}
		plain, err := s.cryptoHandler.cryptoService.DecryptSensitiveData(&envelope)
		if err != nil {
			return errors.New("email request decryption failed")
		}
		raw = []byte(plain)
	} else if config.Get().TransportEncryption {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return errors.New("invalid email request")
		}
		if !legacyTest || len(fields) > 1 || (len(fields) == 1 && fields["email"] == nil) {
			return errors.New("encrypted transmission required")
		}
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("invalid email request")
	}
	return nil
}

func (s *Server) handleSaveDiscordEmail(c *gin.Context) {
	var draft notify.EmailDraft
	if err := s.decodeEmail(c, &draft, false); err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	settings, err := notify.PrepareEmail(s.store, draft)
	if err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	err = s.store.DiscordConfig().SaveEmail(store.EmailConfig{
		Host: settings.Host, Port: settings.Port, Security: settings.Security, User: settings.User,
	}, settings.Pass, settings.Recipient, settings.Enabled)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"saved": true})
}

func (s *Server) handleTestDiscordAlertEmail(c *gin.Context) {
	var req struct {
		Email string             `json:"email"`
		Draft *notify.EmailDraft `json:"draft"`
	}
	if err := s.decodeEmail(c, &req, true); err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	settings, err := notify.ResolveEmail(s.store)
	if err == nil && req.Draft != nil {
		settings, err = notify.PrepareEmail(s.store, *req.Draft)
	}
	if err != nil {
		c.JSON(500, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if req.Draft == nil && req.Email != "" {
		settings.Recipient = req.Email
	}
	// An explicit test is allowed while automatic alerts are disabled.
	if err := notify.SendWithConfig(settings.SMTPConfig, settings.Recipient, "【NOFX 测试】邮件通知", "这是一封 NOFX 测试邮件。SMTP 接受邮件不代表已经进入收件箱。测试不会保存表单。"); err != nil {
		c.JSON(200, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "email": settings.Recipient})
}
