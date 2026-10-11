package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/discord"
	"nofx/notify"
	"nofx/store"
)

func (s *Server) channelNotificationStatus(cfg store.ChannelNotificationConfig) (gin.H, error) {
	result := gin.H{"config": cfg, "applied": false, "channels": []gin.H{}}
	email, err := notify.ResolveEmail(s.store)
	if err != nil {
		return nil, err
	}
	result["smtp_configured"] = email.Configured
	dc, err := s.store.DiscordConfig().Get()
	if err != nil {
		return nil, err
	}
	result["collection_enabled"] = dc != nil && dc.Enabled
	result["credential_configured"] = dc != nil && dc.Token != ""
	states := map[string]store.DiscordSourceState{}
	source := discord.Global()
	ready := false
	if source != nil {
		status := source.CollectorStatus()
		ready = status.State == "connected" && status.IPCConnected && status.AppliedVersion != "" && status.AppliedVersion == status.DesiredVersion && source.NotificationsApplied(cfg.Revision)
	}
	storedStates, err := s.store.DiscordMessage().SourceStates()
	if err != nil {
		return nil, err
	}
	for _, state := range storedStates {
		states[state.ChannelID] = state
	}
	rows := []gin.H{}
	for _, monitor := range cfg.Monitors {
		state, exists := states[monitor.ChannelID]
		if !exists {
			state.State = "waiting"
		}
		if !cfg.Enabled || !monitor.Enabled {
			state.State = "disabled"
		}
		row := gin.H{"monitor_id": monitor.ID, "channel_id": monitor.ChannelID, "state": state.State, "last_message_at": state.LastEventAt, "last_error": state.LastError, "applied": ready && state.State == "ready"}
		latest, err := s.store.LatestChannelNotification(monitor.ID)
		if err != nil {
			return nil, err
		}
		row["last_result"] = latest
		rows = append(rows, row)
	}
	result["channels"] = rows
	result["applied"] = ready
	if !cfg.Enabled {
		result["apply_error"] = "频道邮件通知已关闭"
	} else if dc == nil || dc.Token == "" {
		result["apply_error"] = "等待配置 Discord Token"
	} else if !dc.Enabled {
		result["apply_error"] = "全局消息采集已关闭"
	} else if !email.Configured {
		result["apply_error"] = "请先配置 SMTP 发件服务"
	} else if !ready {
		result["apply_error"] = "等待采集器应用配置（需支持频道通知协议）"
	} else {
		for _, m := range cfg.Monitors {
			if m.Enabled && states[m.ChannelID].State != "ready" {
				result["apply_error"] = "部分频道仍在核验或无访问权限"
				break
			}
		}
	}
	return result, nil
}
func (s *Server) handleGetChannelNotifications(c *gin.Context) {
	cfg, err := s.store.ChannelNotifications()
	if err != nil {
		SafeInternalError(c, "频道通知配置读取失败", err)
		return
	}
	result, err := s.channelNotificationStatus(cfg)
	if err != nil {
		SafeInternalError(c, "频道通知状态读取失败", err)
		return
	}
	c.JSON(200, result)
}
func (s *Server) handleSaveChannelNotifications(c *gin.Context) {
	var cfg store.ChannelNotificationConfig
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16384)
	if err := c.ShouldBindJSON(&cfg); err != nil {
		SafeBadRequest(c, "无效的频道通知配置")
		return
	}
	cfg, err := s.store.SaveChannelNotifications(cfg, time.Now().UTC())
	if errors.Is(err, store.ErrNotificationConflict) {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		SafeBadRequest(c, "保存失败，请检查频道 ID、邮箱、监控项及配置版本")
		return
	}
	s.store.Logs().Record("channel_notify", "channel_notify.config.saved", "info", "频道通知配置已保存", map[string]any{"generation": cfg.Revision, "count": len(cfg.Monitors)})
	var applyErr error
	if source := discord.Global(); source != nil {
		applyErr = source.ReloadConfig()
	}
	result, err := s.channelNotificationStatus(cfg)
	if err != nil {
		c.JSON(200, gin.H{"saved": true, "config": cfg, "channels": []gin.H{}, "applied": false, "apply_error": "配置已保存，但读取生效状态失败"})
		return
	}
	result["saved"] = true
	if applyErr != nil {
		result["applied"] = false
		result["apply_error"] = "配置已保存，采集器应用失败；请检查日志中心"
	}
	c.JSON(200, result)
}
func (s *Server) handleTestChannelNotificationEmail(c *gin.Context) {
	var request struct {
		Recipient string `json:"recipient"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if c.ShouldBindJSON(&request) != nil || notify.ValidateAddress(request.Recipient) != nil {
		SafeBadRequest(c, "请填写一个有效的通知收件邮箱")
		return
	}
	settings, err := notify.ResolveEmail(s.store)
	if err != nil {
		SafeInternalError(c, "邮件配置读取失败", err)
		return
	}
	if !settings.Configured {
		SafeBadRequest(c, "请先配置 SMTP 发件服务")
		return
	}
	err = notify.SendWithConfig(settings.SMTPConfig, request.Recipient, "【NOFX 测试】频道消息通知", "频道通知邮箱测试。测试不会保存配置，不会调用 AI 或执行交易。SMTP 接受邮件不代表已进入收件箱。")
	if err != nil {
		c.JSON(200, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
