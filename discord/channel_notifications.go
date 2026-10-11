package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"nofx/notify"
	"nofx/store"
)

// ChannelNotifier consumes committed receipts independently of the trading
// dispatchers. It never calls an interpreter, execution gate or exchange.
type ChannelNotifier struct {
	st    *store.Store
	epoch string
	stop  chan struct{}
	wg    sync.WaitGroup
	send  func(notify.SMTPConfig, string, string, string, time.Time) error
}

func NewChannelNotifier(st *store.Store, epoch string) *ChannelNotifier {
	return &ChannelNotifier{st: st, epoch: epoch, stop: make(chan struct{}), send: notify.SendBefore}
}
func (n *ChannelNotifier) Start() {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ready := false
		cursor := int64(0)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-ticker.C:
			}
			if !ready {
				if err := n.st.ResetChannelNotifications(time.Now().UTC()); err != nil {
					n.failure("startup")
					continue
				}
				n.st.Logs().Record("channel_notify", "channel_notify.started", "info", "通知发送器已启动；旧待发送任务已取消，旧发送中任务标记结果不明", map[string]any{"code": "NEW_RECEPTION_BOUNDARY"})
				ready = true
			}
			next, jobs, err := n.st.QueueChannelNotifications(n.epoch, cursor, time.Now().UTC())
			if err != nil {
				n.failure("queue")
				continue
			}
			cursor = next
			for _, job := range jobs {
				n.record(job, job.Status)
			}
			if err = n.processOne(time.Now().UTC()); err != nil {
				n.failure("delivery_ledger")
			}
		}
	}()
}
func (n *ChannelNotifier) Stop() { close(n.stop); n.wg.Wait() }
func (n *ChannelNotifier) failure(stage string) {
	n.st.Logs().Record("channel_notify", "channel_notify.storage.failed", "error", "频道通知记录不完整", map[string]any{"stage": stage, "code": "NOTIFICATION_STORAGE_FAILED"})
}
func (n *ChannelNotifier) record(job store.ChannelNotification, result string) {
	level := "info"
	if result == "sent" {
		level = "success"
	}
	if result == "failed" || result == "uncertain" || result == "dropped" || result == "expired" {
		level = "warn"
	}
	context, _ := json.Marshal(map[string]any{"source_event_id": job.SourceEventID, "message_id": job.MessageID, "delivery_id": job.ID, "result": result, "code": job.Error})
	if err := n.st.Logs().Append(&store.SystemEvent{Component: "channel_notify", Event: "channel_notify." + result, Level: level, Message: "频道消息邮件通知：" + result, ChannelID: job.ChannelID, ContextJSON: string(context)}); err != nil {
		n.st.Logs().NoteFailure()
	}
}
func (n *ChannelNotifier) processOne(now time.Time) error {
	job, err := n.st.ClaimChannelNotification(now)
	if err != nil || job == nil {
		return err
	}
	if job.Status != "sending" {
		n.record(*job, job.Status)
		return nil
	}
	settings, err := notify.ResolveEmail(n.st)
	if err == nil {
		var subject, body string
		subject, body, err = notificationMail(job)
		if err == nil {
			err = n.send(settings.SMTPConfig, job.Recipient, subject, body, job.ExpiresAt)
		}
	}
	status, detail, next := "sent", "", now
	if err != nil {
		status = "failed"
		detail = "SMTP or email configuration failed; check saved settings"
		var failure *notify.DeliveryError
		if errors.As(err, &failure) {
			detail = failure.Error()
			if failure.Kind == "uncertain" {
				status = "uncertain"
			}
			if failure.Kind == "temporary" && job.Attempts < 3 {
				delay := 15 * time.Second
				if job.Attempts == 2 {
					delay = 45 * time.Second
				}
				next = time.Now().UTC().Add(delay)
				if next.Before(job.ExpiresAt) {
					status = "retry"
				}
			}
		}
	}
	// A failed ledger write leaves 'sending': neither this loop nor a restart
	// will replay an SMTP attempt whose outcome may already have been accepted.
	if err = n.st.FinishChannelNotification(job, status, detail, next); err != nil {
		return err
	}
	job.Error = detail
	n.record(*job, status)
	return nil
}

func notificationMail(job *store.ChannelNotification) (string, string, error) {
	var msg store.DiscordMessage
	if err := json.Unmarshal([]byte(job.MessageJSON), &msg); err != nil {
		return "", "", err
	}
	channel := job.Label
	if channel == "" {
		channel = job.ChannelID
	}
	channel = strings.NewReplacer("\r", " ", "\n", " ").Replace(channel)
	subject := "【NOFX 频道消息】" + channel
	zone := time.FixedZone("北京时间 UTC+8", 8*3600)
	var b strings.Builder
	fmt.Fprintf(&b, "频道：%s（%s）\n作者：%s\n发布时间：%s\n\n正文：\n%s\n", channel, job.ChannelID, msg.AuthorName, msg.MessageTimestamp.In(zone).Format("2006-01-02 15:04:05 MST"), msg.Content)
	var embeds []struct {
		Title       string
		Description string
		Fields      []struct {
			Name  string
			Value string
		}
	}
	if err := json.Unmarshal([]byte(msg.EmbedsJSON), &embeds); err == nil && len(embeds) > 0 {
		b.WriteString("\n附带／引用卡片（仅转发，不作交易解读）：\n")
		for _, embed := range embeds {
			fmt.Fprintf(&b, "%s\n%s\n", embed.Title, embed.Description)
			for _, field := range embed.Fields {
				fmt.Fprintf(&b, "%s：%s\n", field.Name, field.Value)
			}
		}
	}
	var attachments []struct {
		Filename string
		URL      string
	}
	if err := json.Unmarshal([]byte(msg.AttachmentsJSON), &attachments); err == nil && len(attachments) > 0 {
		b.WriteString("\n附件链接（可能过期，请以原消息为准）：\n")
		for i, a := range attachments {
			if i == 10 {
				b.WriteString("附件超过10个，其余请查看原消息。\n")
				break
			}
			u, err := url.Parse(a.URL)
			if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				fmt.Fprintf(&b, "%s：%s\n", a.Filename, a.URL)
			}
		}
	}
	guild := msg.GuildID
	if guild == "" {
		guild = "@me"
	}
	footer := fmt.Sprintf("\n原消息：https://discord.com/channels/%s/%s/%s\n此邮件仅为新消息提醒，未经 AI 分析，不代表跟单结果。\n", guild, job.ChannelID, job.MessageID)
	body := b.String()
	limit := 16*1024 - len(footer)
	if len(body) > limit {
		notice := "\n[内容已截断，请查看原消息]\n"
		body = strings.ToValidUTF8(body[:limit-len(notice)], "") + notice
	}
	return subject, body + footer, nil
}
