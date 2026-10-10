package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"net/http"
	"nofx/discord"
	"nofx/store"
	"strconv"
	"strings"
	"time"
)

func logFilter(c *gin.Context) (store.LogFilter, error) {
	f := store.LogFilter{Scope: c.DefaultQuery("scope", "system"), UserID: c.GetString("user_id"), TraderID: c.Query("trader_id"), ChannelID: c.Query("channel_id"), Level: strings.ToLower(c.Query("level")), Component: c.Query("component"), Event: c.Query("event"), Correlation: c.Query("correlation_id"), Search: c.Query("q"), Limit: 100, Upper: -1}
	if f.UserID == "" {
		return f, fmt.Errorf("authentication required")
	}
	if f.Scope != "system" && f.Scope != "trade" {
		return f, fmt.Errorf("invalid log scope")
	}
	for _, v := range []string{f.TraderID, f.ChannelID, f.Level, f.Component, f.Event, f.Correlation, f.Search} {
		if len(v) > 256 {
			return f, fmt.Errorf("filter too long")
		}
	}
	var err error
	for key, dest := range map[string]*time.Time{"start": &f.Start, "end": &f.End} {
		if v := c.Query(key); v != "" {
			*dest, err = time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return f, fmt.Errorf("invalid %s", key)
			}
		}
	}
	if f.Start.IsZero() {
		f.Start = time.Now().Add(-24 * time.Hour)
	}
	if f.End.IsZero() {
		f.End = time.Now().UTC()
	}
	if f.Start.After(f.End) {
		return f, fmt.Errorf("start must precede end")
	}
	for key, dest := range map[string]*int64{"before": &f.Before, "upper": &f.Upper} {
		if v := c.Query(key); v != "" {
			*dest, err = strconv.ParseInt(v, 10, 64)
			if err != nil || *dest < 0 {
				return f, fmt.Errorf("invalid cursor")
			}
		}
	}
	return f, nil
}
func (s *Server) handleLogEvents(c *gin.Context) {
	f, err := logFilter(c)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if f.Upper < 0 {
		f.Upper, _, err = s.store.Logs().LogBoundary(f)
		if err != nil {
			SafeInternalError(c, "日志查询失败", err)
			return
		}
	}
	f.Limit = 101
	rows, err := s.store.Logs().ListLogs(f)
	if err != nil {
		SafeInternalError(c, "日志查询失败", err)
		return
	}
	next := int64(0)
	if len(rows) > 100 {
		rows = rows[:100]
		next = rows[99].ID
	}
	failures := s.store.Logs().FailureCount()
	if source := discord.Global(); source != nil {
		failures = source.CollectorStatus().DiagnosticFailures
	}
	c.JSON(200, gin.H{"events": rows, "next_cursor": next, "upper": f.Upper, "start": f.Start, "end": f.End, "refreshed_at": time.Now().UTC(), "write_failures": failures})
}
func (s *Server) handleLogTrace(c *gin.Context) {
	identifiers := 0
	for _, k := range []string{"source_event_id", "signal_id", "context_id"} {
		if c.Query(k) != "" {
			identifiers++
		}
		if len(c.Query(k)) > 256 {
			c.JSON(400, gin.H{"error": "invalid identifier"})
			return
		}
	}
	if identifiers != 1 {
		c.JSON(400, gin.H{"error": "需要来源事件、信号或交易 ID"})
		return
	}
	row, err := s.store.Logs().Trace(c.GetString("user_id"), c.Query("source_event_id"), c.Query("signal_id"), c.Query("context_id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(404, gin.H{"error": "记录不存在、已过保留期或无权访问"})
		return
	}
	if err != nil {
		SafeInternalError(c, "链路查询失败", err)
		return
	}
	c.JSON(200, row)
}
func csvSafe(v string) string {
	trim := strings.TrimLeft(v, " \t\r\n")
	if len(trim) > 0 && strings.ContainsRune("=+-@", rune(trim[0])) {
		return "'" + v
	}
	if strings.HasPrefix(v, "\t") || strings.HasPrefix(v, "\r") {
		return "'" + v
	}
	return v
}
func (s *Server) handleLogExport(c *gin.Context) {
	f, err := logFilter(c)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	format := c.DefaultQuery("format", "csv")
	if format != "csv" && format != "jsonl" {
		c.JSON(400, gin.H{"error": "format must be csv or jsonl"})
		return
	}
	var count int64
	f.Upper, count, err = s.store.Logs().LogBoundary(f)
	if err != nil {
		SafeInternalError(c, "导出查询失败", err)
		return
	}
	if count > 100000 {
		c.JSON(400, gin.H{"error": "超过 100,000 条，请缩小时间范围", "count": count})
		return
	}
	f.Before = 0
	f.Limit = 500
	// Fetch first batch before committing response headers.
	rows, err := s.store.Logs().ListLogs(f)
	if err != nil {
		SafeInternalError(c, "导出查询失败", err)
		return
	}
	contentType := "text/csv; charset=utf-8"
	if format == "jsonl" {
		contentType = "application/x-ndjson; charset=utf-8"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-logs-%s-%s.%s"`, f.Scope, f.Start.Format("20060102T150405"), f.End.Format("20060102T150405"), format))
	c.Header("X-Log-Count", strconv.FormatInt(count, 10))
	c.Header("Trailer", "X-Export-Error")
	writer := csv.NewWriter(c.Writer)
	encoder := json.NewEncoder(c.Writer)
	if format == "csv" {
		_ = writer.Write([]string{"id", "time", "level", "component", "event", "message", "trader_id", "source_channel_id", "logical_channel_id", "source_event_id", "delivery_id", "message_id", "signal_id", "trace_id", "duration_ms", "context_json"})
	}
	written := 0
	for {
		for _, r := range rows {
			if c.Request.Context().Err() != nil {
				return
			}
			if format == "jsonl" {
				err = encoder.Encode(r)
			} else {
				values := []string{strconv.FormatInt(r.ID, 10), r.OccurredAt.Format(time.RFC3339Nano), r.Level, r.Component, r.Event, r.Message, r.TraderID, r.ChannelID, r.LogicalChannelID, r.SourceEventID, strconv.FormatInt(r.DeliveryID, 10), r.MessageID, r.SignalID, r.TraceID, strconv.FormatInt(r.DurationMs, 10), r.ContextJSON}
				for i := range values {
					values[i] = csvSafe(values[i])
				}
				err = writer.Write(values)
			}
			if err != nil {
				return
			}
			written++
		}
		if format == "csv" {
			writer.Flush()
			if writer.Error() != nil {
				return
			}
		}
		if len(rows) < f.Limit {
			break
		}
		f.Before = rows[len(rows)-1].ID
		rows, err = s.store.Logs().ListLogs(f)
		if err != nil {
			c.Header("X-Export-Error", "query_failed")
			return
		}
	}
	if int64(written) != count {
		c.Header("X-Export-Error", "records_changed_during_export")
	}
}
func (s *Server) handleLogSettings(c *gin.Context) {
	if c.Request.Method == "PUT" {
		var body struct {
			Days int `json:"system_days"`
		}
		if c.ShouldBindJSON(&body) != nil || (body.Days != 7 && body.Days != 30 && body.Days != 90) {
			c.JSON(400, gin.H{"error": "系统日志保留天数必须为 7、30 或 90"})
			return
		}
		if err := s.store.SetSystemConfig("system_log_retention_days", strconv.Itoa(body.Days)); err != nil {
			SafeInternalError(c, "保留设置保存失败", err)
			return
		}
		s.store.Logs().UserAudit(c.GetString("user_id"), "logs.settings.saved", "系统日志保留设置已保存", map[string]any{"retention_days": body.Days})
	}
	days, err := s.store.Logs().SystemDays()
	if err != nil {
		SafeInternalError(c, "保留设置读取失败", err)
		return
	}
	stats, err := s.store.Logs().Stats()
	if err != nil {
		SafeInternalError(c, "日志容量查询失败", err)
		return
	}
	c.JSON(200, gin.H{"system_days": days, "retention": store.EffectiveRetention(), "soft_limit_bytes": 512 << 20, "stats": stats})
}
func (s *Server) handleLogCleanupPreview(c *gin.Context) {
	var body struct {
		Cutoff time.Time `json:"cutoff"`
	}
	if c.ShouldBindJSON(&body) != nil || body.Cutoff.IsZero() || body.Cutoff.After(time.Now()) {
		c.JSON(400, gin.H{"error": "请选择有效的历史截止时间"})
		return
	}
	upper, n, err := s.store.Logs().CleanupPreview(c.GetString("user_id"), body.Cutoff)
	if err != nil {
		SafeInternalError(c, "清理预览失败", err)
		return
	}
	ticket := store.LogCleanupTicket{ID: uuid.NewString(), UserID: c.GetString("user_id"), Cutoff: body.Cutoff, UpperID: upper, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if err = s.store.GormDB().Create(&ticket).Error; err != nil {
		SafeInternalError(c, "清理预览保存失败", err)
		return
	}
	c.JSON(200, gin.H{"ticket": ticket.ID, "count": n, "cutoff": body.Cutoff, "expires_at": ticket.ExpiresAt})
}
func (s *Server) handleLogCleanup(c *gin.Context) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	if c.ShouldBindJSON(&body) != nil || body.Ticket == "" {
		c.JSON(400, gin.H{"error": "请先预览并确认清理"})
		return
	}
	var ticket store.LogCleanupTicket
	err := s.store.GormDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ? AND expires_at > ? AND consumed = ?", body.Ticket, c.GetString("user_id"), time.Now(), false).First(&ticket).Error; err != nil {
			return err
		}
		r := tx.Model(&store.LogCleanupTicket{}).Where("id = ? AND consumed = ?", ticket.ID, false).Update("consumed", true)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "预览已过期、已使用或不可访问，请重新预览"})
		return
	}
	n, err := s.store.Logs().Cleanup(c.GetString("user_id"), ticket.Cutoff, ticket.UpperID)
	s.store.Logs().UserAudit(c.GetString("user_id"), "logs.cleanup", "清理历史系统日志", map[string]any{"count": n, "upper_id": ticket.UpperID, "cutoff": ticket.Cutoff.Format(time.RFC3339), "result": map[bool]string{true: "partial_failure", false: "complete"}[err != nil]})
	if err != nil {
		SafeInternalError(c, "清理部分完成，请刷新后重新预览", err)
		return
	}
	c.JSON(200, gin.H{"removed": n})
}
