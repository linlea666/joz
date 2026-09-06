package discord

import (
	"fmt"
	"sync"
	"time"

	"nofx/logger"
	"nofx/notify"
	"nofx/store"
)

// Token status values reported by the monitor.
const (
	TokenStatusUnknown = "unknown" // not configured / monitoring off / not checked yet
	TokenStatusOK      = "ok"
	TokenStatusInvalid = "invalid" // 401/403 — operator must refresh the token
)

// reminderInterval is how often a reminder email is re-sent while the token
// stays invalid (the first alert goes out immediately on the transition).
const reminderInterval = 60 * time.Minute

// monitorDefaultInterval is used when the config has no usable interval or
// monitoring is idle (unconfigured/disabled) and we just re-check the config.
const monitorDefaultInterval = 60 * time.Second

// TokenMonitor periodically probes the global Discord token
// (`GET /quests/@me`) and emails the configured recipient when the token
// becomes invalid (and again when it recovers). It re-reads the config from
// the store every cycle, so settings changes apply without any reload wiring.
type TokenMonitor struct {
	store  *store.Store
	poller *PollerManager // reused for its rate-limited client (may be nil)

	// Seams for unit tests; production defaults are set in NewTokenMonitor.
	probe func(token string) error             // token validity check
	send  func(to, subject, body string) error // email delivery

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	status        string
	lastCheckedAt time.Time
	lastError     string
	invalidSince  time.Time
	lastAlertAt   time.Time
	alertsSent    int
}

// NewTokenMonitor creates the monitor (call Start to begin checking).
func NewTokenMonitor(st *store.Store, poller *PollerManager) *TokenMonitor {
	tm := &TokenMonitor{store: st, poller: poller, status: TokenStatusUnknown}
	tm.probe = func(token string) error {
		// Prefer the poller's client (shares the process-wide serial
		// rate-limit queue). Fall back to a one-off client when the poller
		// is unconfigured.
		var client *Client
		if tm.poller != nil {
			client = tm.poller.Client()
		}
		if client == nil {
			client = NewClient(token)
		}
		return client.CheckQuests()
	}
	tm.send = notify.SendEmail
	return tm
}

// Start launches the check loop.
func (tm *TokenMonitor) Start() {
	tm.mu.Lock()
	if tm.running {
		tm.mu.Unlock()
		return
	}
	tm.running = true
	tm.stopCh = make(chan struct{})
	tm.mu.Unlock()

	tm.wg.Add(1)
	go tm.loop()
	logger.Infof("✅ Discord token monitor started")
}

// Stop terminates the check loop.
func (tm *TokenMonitor) Stop() {
	tm.mu.Lock()
	if !tm.running {
		tm.mu.Unlock()
		return
	}
	tm.running = false
	close(tm.stopCh)
	tm.mu.Unlock()
	tm.wg.Wait()
}

func (tm *TokenMonitor) loop() {
	defer tm.wg.Done()
	for {
		interval := tm.checkOnce()
		tm.mu.Lock()
		stopCh := tm.stopCh
		tm.mu.Unlock()
		select {
		case <-stopCh:
			return
		case <-time.After(interval):
		}
	}
}

// checkOnce runs one monitoring cycle and returns how long to sleep before
// the next one.
func (tm *TokenMonitor) checkOnce() time.Duration {
	cfg, err := tm.store.DiscordConfig().Get()
	if err != nil {
		logger.Warnf("[DiscordMonitor] config read failed: %v", err)
		return monitorDefaultInterval
	}
	// Idle: nothing to monitor. Deliberate operator states (no token /
	// monitoring off) never alert; state resets so a future failure after
	// re-enabling gets a fresh alert.
	if cfg == nil || string(cfg.Token) == "" || !cfg.MonitorEnabled {
		tm.mu.Lock()
		tm.status = TokenStatusUnknown
		tm.invalidSince = time.Time{}
		tm.lastAlertAt = time.Time{}
		tm.lastError = ""
		tm.mu.Unlock()
		return monitorDefaultInterval
	}

	interval := time.Duration(cfg.MonitorIntervalSeconds) * time.Second
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > 10*time.Minute {
		interval = 10 * time.Minute
	}

	checkErr := tm.probe(string(cfg.Token))
	now := time.Now()

	tm.mu.Lock()
	prevStatus := tm.status
	tm.lastCheckedAt = now
	switch {
	case checkErr == nil:
		tm.status = TokenStatusOK
		tm.lastError = ""
	case IsAuthError(checkErr):
		tm.status = TokenStatusInvalid
		tm.lastError = checkErr.Error()
		if prevStatus != TokenStatusInvalid {
			tm.invalidSince = now
		}
	default:
		// Transient (network / 5xx / rate limit): keep the previous status,
		// only record the error. Never alert on non-auth failures.
		tm.lastError = checkErr.Error()
	}
	status := tm.status
	invalidSince := tm.invalidSince
	lastAlertAt := tm.lastAlertAt
	tm.mu.Unlock()

	switch {
	case status == TokenStatusInvalid && prevStatus != TokenStatusInvalid:
		logger.Errorf("[DiscordMonitor] token invalid (quests/@me): %v", checkErr)
		tm.sendAlert(cfg, checkErr, invalidSince, false)
	case status == TokenStatusInvalid && now.Sub(lastAlertAt) >= reminderInterval:
		tm.sendAlert(cfg, checkErr, invalidSince, true)
	case status == TokenStatusOK && prevStatus == TokenStatusInvalid:
		logger.Infof("[DiscordMonitor] token recovered")
		tm.sendRecovery(cfg, invalidSince)
		tm.mu.Lock()
		tm.invalidSince = time.Time{}
		tm.lastAlertAt = time.Time{}
		tm.mu.Unlock()
	}
	return interval
}

// sendAlert emails the invalid-token alert. lastAlertAt advances on every
// attempt (success or not) so a broken SMTP setup logs once per reminder
// window instead of every cycle.
func (tm *TokenMonitor) sendAlert(cfg *store.DiscordConfig, checkErr error, invalidSince time.Time, isReminder bool) {
	tm.mu.Lock()
	tm.lastAlertAt = time.Now()
	tm.mu.Unlock()

	if cfg.AlertEmail == "" {
		logger.Warnf("[DiscordMonitor] token invalid but no alert email configured")
		return
	}
	subject := "【NOFX 告警】Discord Token 已失效，跟单已停摆"
	if isReminder {
		subject = "【NOFX 提醒】Discord Token 仍未恢复，跟单持续停摆"
	}
	errText := ""
	if checkErr != nil {
		errText = checkErr.Error()
	}
	body := fmt.Sprintf(
		"NOFX 检测到全局 Discord Token 已失效（账号可能在其他设备退出登录或 Token 被重置）。\n\n"+
			"状态检查接口：GET /quests/@me\n"+
			"失效开始时间：%s\n"+
			"最近一次检查：%s\n"+
			"错误详情：%s\n\n"+
			"影响：所有 Discord 跟单交易员已无法接收频道消息，新信号不会被执行。\n\n"+
			"处理步骤：\n"+
			"1. 重新登录 Discord 获取新的用户 Token\n"+
			"2. 打开 NOFX 前端 -> 设置 -> Discord -> 粘贴新 Token 并保存\n"+
			"3. 保存后监控会自动确认恢复并发送恢复通知邮件\n",
		formatAlertTime(invalidSince), formatAlertTime(time.Now()), errText,
	)
	tm.deliver(cfg.AlertEmail, subject, body)
}

// sendRecovery emails the all-clear notice after the token works again.
func (tm *TokenMonitor) sendRecovery(cfg *store.DiscordConfig, invalidSince time.Time) {
	if cfg.AlertEmail == "" {
		return
	}
	downFor := ""
	if !invalidSince.IsZero() {
		downFor = time.Since(invalidSince).Round(time.Second).String()
	}
	body := fmt.Sprintf(
		"NOFX 检测到 Discord Token 已恢复有效，跟单消息轮询恢复正常。\n\n"+
			"恢复时间：%s\n"+
			"本次失效时长：%s\n",
		formatAlertTime(time.Now()), downFor,
	)
	tm.deliver(cfg.AlertEmail, "【NOFX 通知】Discord Token 已恢复", body)
}

// deliver sends one email synchronously (alerts are rare and SendEmail has a
// 15s dial timeout, so blocking the check loop briefly is fine).
func (tm *TokenMonitor) deliver(to, subject, body string) {
	if err := tm.send(to, subject, body); err != nil {
		logger.Errorf("[DiscordMonitor] alert email to %s failed: %v", to, err)
		return
	}
	tm.mu.Lock()
	tm.alertsSent++
	tm.mu.Unlock()
	logger.Infof("[DiscordMonitor] alert email sent to %s: %s", to, subject)
}

// MonitorStatus is the API-facing snapshot. Time fields are RFC3339 strings
// (empty when unset) so the frontend needs no zero-time handling.
type MonitorStatus struct {
	Status        string `json:"status"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	InvalidSince  string `json:"invalid_since,omitempty"`
	LastAlertAt   string `json:"last_alert_at,omitempty"`
	AlertsSent    int    `json:"alerts_sent"`
}

// Status returns the current snapshot.
func (tm *TokenMonitor) Status() MonitorStatus {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return MonitorStatus{
		Status:        tm.status,
		LastCheckedAt: formatAlertTimeRFC(tm.lastCheckedAt),
		LastError:     tm.lastError,
		InvalidSince:  formatAlertTimeRFC(tm.invalidSince),
		LastAlertAt:   formatAlertTimeRFC(tm.lastAlertAt),
		AlertsSent:    tm.alertsSent,
	}
}

func formatAlertTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05 MST")
}

func formatAlertTimeRFC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// --- global singleton (same pattern as the poller) ---

var globalMonitor *TokenMonitor

// InitGlobalMonitor creates (once) and returns the monitor singleton.
func InitGlobalMonitor(st *store.Store, poller *PollerManager) *TokenMonitor {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalMonitor == nil {
		globalMonitor = NewTokenMonitor(st, poller)
	}
	return globalMonitor
}

// GlobalMonitor returns the monitor singleton (nil before InitGlobalMonitor).
func GlobalMonitor() *TokenMonitor {
	globalMu.Lock()
	defer globalMu.Unlock()
	return globalMonitor
}
