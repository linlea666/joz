package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Trade contexts: one row per followed trade lifecycle (state machine anchor)
// ---------------------------------------------------------------------------

// CopyTradeContext is a followed trade's living state. RootMessageID is the
// correlation anchor: replies/links/lifecycle edits map back to it.
type CopyTradeContext struct {
	ID            string `gorm:"primaryKey" json:"id"`
	TraderID      string `gorm:"column:trader_id;not null;index:idx_ctc_trader_state,priority:1" json:"trader_id"`
	ChannelID     string `gorm:"column:channel_id;not null" json:"channel_id"`
	RootMessageID string `gorm:"column:root_message_id;not null;index" json:"root_message_id"`

	Symbol    string `gorm:"column:symbol;not null" json:"symbol"` // canonical (BTCUSDT)
	RawSymbol string `gorm:"column:raw_symbol;default:''" json:"raw_symbol"`
	Direction string `gorm:"column:direction;not null" json:"direction"` // LONG / SHORT
	State     string `gorm:"column:state;not null;index:idx_ctc_trader_state,priority:2" json:"state"`
	Version   int    `gorm:"column:version;default:0" json:"version"` // optimistic lock

	PlannedEntryPrice float64 `gorm:"column:planned_entry_price;default:0" json:"planned_entry_price"`
	AvgFillPrice      float64 `gorm:"column:avg_fill_price;default:0" json:"avg_fill_price"`
	Quantity          float64 `gorm:"column:quantity;default:0" json:"quantity"` // remaining position qty
	Leverage          int     `gorm:"column:leverage;default:0" json:"leverage"`
	StopLossPrice     float64 `gorm:"column:stop_loss_price;default:0" json:"stop_loss_price"`
	TPPlanJSON        string  `gorm:"column:tp_plan_json;default:''" json:"tp_plan_json"` // [{price, quantity, order_id}]
	TPHitCount        int     `gorm:"column:tp_hit_count;default:0" json:"tp_hit_count"`
	BreakevenApplied  bool    `gorm:"column:breakeven_applied;default:false" json:"breakeven_applied"`
	// BreakevenAfterTP marks an author-stated conditional rule ("move SL to
	// entry after TP1") on this specific trade; OR-ed with the global config.
	BreakevenAfterTP bool `gorm:"column:breakeven_after_tp;default:false" json:"breakeven_after_tp"`

	EntryOrderID string `gorm:"column:entry_order_id;default:''" json:"entry_order_id"`
	LastAction   string `gorm:"column:last_action;default:''" json:"last_action"`
	LastError    string `gorm:"column:last_error;default:''" json:"last_error"`

	OpenedAt  *time.Time `gorm:"column:opened_at" json:"opened_at,omitempty"`
	ClosedAt  *time.Time `gorm:"column:closed_at" json:"closed_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (CopyTradeContext) TableName() string { return "copytrade_trade_contexts" }

// ---------------------------------------------------------------------------
// AI runs: one row per LLM parsing call (latency / model comparison)
// ---------------------------------------------------------------------------

// CopyTradeAIRun records one LLM interpretation call end to end.
type CopyTradeAIRun struct {
	ID            int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID      string `gorm:"column:trader_id;not null;index" json:"trader_id"`
	ChannelID     string `gorm:"column:channel_id;not null" json:"channel_id"`
	MessageID     string `gorm:"column:message_id;not null;index" json:"message_id"`
	Model         string `gorm:"column:model;default:''" json:"model"`
	Provider      string `gorm:"column:provider;default:''" json:"provider"`
	PromptVersion string `gorm:"column:prompt_version;default:''" json:"prompt_version"`
	SystemPrompt  string `gorm:"column:system_prompt;default:''" json:"system_prompt"`
	InputPrompt   string `gorm:"column:input_prompt;default:''" json:"input_prompt"`
	ImageCount    int    `gorm:"column:image_count;default:0" json:"image_count"`
	RawResponse   string `gorm:"column:raw_response;default:''" json:"raw_response"`
	ParsedJSON    string `gorm:"column:parsed_json;default:''" json:"parsed_json"`
	Error         string `gorm:"column:error;default:''" json:"error"`

	StartedAt  time.Time `gorm:"column:started_at" json:"started_at"`
	FinishedAt time.Time `gorm:"column:finished_at" json:"finished_at"`
	DurationMs int64     `gorm:"column:duration_ms;default:0" json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

func (CopyTradeAIRun) TableName() string { return "copytrade_ai_runs" }

// ---------------------------------------------------------------------------
// Signals: one row per (trader, message revision) interpretation outcome
// ---------------------------------------------------------------------------

// Copy-trading signal statuses.
const (
	SignalStatusReceived  = "received"
	SignalStatusParsed    = "parsed"
	SignalStatusValidated = "validated"
	SignalStatusExecuting = "executing"
	SignalStatusExecuted  = "executed"
	SignalStatusSkipped   = "skipped"
	SignalStatusFailed    = "failed"
)

// CopyTradeSignal is the per-trader record of one interpreted message.
type CopyTradeSignal struct {
	ID              string `gorm:"primaryKey" json:"id"`
	TraderID        string `gorm:"column:trader_id;not null;index:idx_cts_trader_time,priority:1" json:"trader_id"`
	ChannelID       string `gorm:"column:channel_id;not null;index" json:"channel_id"`
	MessageID       string `gorm:"column:message_id;not null;index" json:"message_id"`
	MessageRevision int    `gorm:"column:message_revision;default:0" json:"message_revision"`
	AIRunID         int64  `gorm:"column:ai_run_id;default:0" json:"ai_run_id"`
	TradeContextID  string `gorm:"column:trade_context_id;default:'';index" json:"trade_context_id"`

	Classification     string `gorm:"column:classification;default:''" json:"classification"`
	Action             string `gorm:"column:action;default:''" json:"action"`
	Symbol             string `gorm:"column:symbol;default:''" json:"symbol"`
	Direction          string `gorm:"column:direction;default:''" json:"direction"`
	InterpretationJSON string `gorm:"column:interpretation_json;default:''" json:"interpretation_json"`
	// hasExecutionIntent=false rows are message context only (setups, analysis
	// with parameters): usable to complete future signals, never executed.
	HasExecutionIntent bool `gorm:"column:has_execution_intent;default:false" json:"has_execution_intent"`

	Status       string `gorm:"column:status;default:'received';index" json:"status"`
	SkipReason   string `gorm:"column:skip_reason;default:''" json:"skip_reason"`
	ErrorMessage string `gorm:"column:error_message;default:''" json:"error_message"`

	MessageTimestamp time.Time `gorm:"column:message_timestamp;index:idx_cts_trader_time,priority:2,sort:desc" json:"message_timestamp"`
	ReceivedAt       time.Time `gorm:"column:received_at" json:"received_at"`

	// Latency breakdown (milliseconds)
	ReceiveLatencyMs int64     `gorm:"column:receive_latency_ms;default:0" json:"receive_latency_ms"`
	MediaDownloadMs  int64     `gorm:"column:media_download_ms;default:0" json:"media_download_ms"`
	PromptBuildMs    int64     `gorm:"column:prompt_build_ms;default:0" json:"prompt_build_ms"`
	LLMRequestMs     int64     `gorm:"column:llm_request_ms;default:0" json:"llm_request_ms"`
	RiskCalcMs       int64     `gorm:"column:risk_calc_ms;default:0" json:"risk_calc_ms"`
	ExchangeSubmitMs int64     `gorm:"column:exchange_submit_ms;default:0" json:"exchange_submit_ms"`
	TotalMs          int64     `gorm:"column:total_ms;default:0" json:"total_ms"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (CopyTradeSignal) TableName() string { return "copytrade_signals" }

// ---------------------------------------------------------------------------
// Execution events: full trace waterfall (mirrors the reference project)
// ---------------------------------------------------------------------------

// CopyTradeEvent is one trace event in the copy-trading pipeline.
type CopyTradeEvent struct {
	ID          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TraceID     string    `gorm:"column:trace_id;not null;index" json:"trace_id"` // usually = signal id
	SignalID    string    `gorm:"column:signal_id;default:''" json:"signal_id"`
	TraderID    string    `gorm:"column:trader_id;not null;index:idx_cte_trader_time,priority:1" json:"trader_id"`
	ChannelID   string    `gorm:"column:channel_id;default:''" json:"channel_id"`
	MessageID   string    `gorm:"column:message_id;default:''" json:"message_id"`
	Level       string    `gorm:"column:level;default:'info'" json:"level"` // info / success / warn / error
	Event       string    `gorm:"column:event;not null" json:"event"`       // e.g. copytrade.signal.received
	Message     string    `gorm:"column:message;default:''" json:"message"` // human-readable summary
	ContextJSON string    `gorm:"column:context_json;default:''" json:"context_json"`
	DurationMs  int64     `gorm:"column:duration_ms;default:0" json:"duration_ms"`
	OccurredAt  time.Time `gorm:"column:occurred_at;index:idx_cte_trader_time,priority:2,sort:desc" json:"occurred_at"`
	CreatedAt   time.Time `json:"created_at"`
}

func (CopyTradeEvent) TableName() string { return "copytrade_execution_events" }

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// CopyTradeStore persists copy-trading contexts, AI runs, signals and events.
type CopyTradeStore struct {
	db *gorm.DB
}

// NewCopyTradeStore creates a new CopyTradeStore.
func NewCopyTradeStore(db *gorm.DB) *CopyTradeStore {
	return &CopyTradeStore{db: db}
}

func (s *CopyTradeStore) initTables() error {
	return s.db.AutoMigrate(
		&CopyTradeContext{},
		&CopyTradeAIRun{},
		&CopyTradeSignal{},
		&CopyTradeEvent{},
	)
}

// --- Trade contexts ---

// CreateContext inserts a new trade context.
func (s *CopyTradeStore) CreateContext(ctx *CopyTradeContext) error {
	return s.db.Create(ctx).Error
}

// GetContext fetches one context by id.
func (s *CopyTradeStore) GetContext(id string) (*CopyTradeContext, error) {
	var ctx CopyTradeContext
	if err := s.db.First(&ctx, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ctx, nil
}

// activeStates are trade states that still need management.
var activeStates = []string{"ENTRY_PENDING", "OPEN", "BREAKEVEN", "CLOSE_PENDING", "NEW"}

// GetActiveContexts lists a trader's active trade contexts.
func (s *CopyTradeStore) GetActiveContexts(traderID string) ([]*CopyTradeContext, error) {
	var ctxs []*CopyTradeContext
	err := s.db.Where("trader_id = ? AND state IN ?", traderID, activeStates).
		Order("created_at DESC").Find(&ctxs).Error
	return ctxs, err
}

// GetActiveContextByRootMessage finds the active context anchored to a message.
func (s *CopyTradeStore) GetActiveContextByRootMessage(traderID, rootMessageID string) (*CopyTradeContext, error) {
	var ctx CopyTradeContext
	err := s.db.Where("trader_id = ? AND root_message_id = ? AND state IN ?", traderID, rootMessageID, activeStates).
		First(&ctx).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ctx, nil
}

// GetActiveContextBySymbol finds the active context for a symbol+direction.
func (s *CopyTradeStore) GetActiveContextBySymbol(traderID, symbol, direction string) (*CopyTradeContext, error) {
	q := s.db.Where("trader_id = ? AND symbol = ? AND state IN ?", traderID, symbol, activeStates)
	if direction != "" {
		q = q.Where("direction = ?", direction)
	}
	var ctx CopyTradeContext
	err := q.Order("created_at DESC").First(&ctx).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ctx, nil
}

// UpdateContextVersioned applies updates with optimistic locking: the write
// only succeeds when the stored version matches expectedVersion. This is the
// guard against out-of-order signal processing overwriting newer state.
func (s *CopyTradeStore) UpdateContextVersioned(id string, expectedVersion int, updates map[string]interface{}) error {
	updates["version"] = expectedVersion + 1
	result := s.db.Model(&CopyTradeContext{}).
		Where("id = ? AND version = ?", id, expectedVersion).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("optimistic lock conflict on trade context %s (expected version %d)", id, expectedVersion)
	}
	return nil
}

// CountActiveByTrader counts active trade contexts (max_open_positions guard).
func (s *CopyTradeStore) CountActiveByTrader(traderID string) (int64, error) {
	var n int64
	err := s.db.Model(&CopyTradeContext{}).
		Where("trader_id = ? AND state IN ?", traderID, activeStates).Count(&n).Error
	return n, err
}

// --- AI runs ---

// CreateAIRun inserts an AI run record.
func (s *CopyTradeStore) CreateAIRun(run *CopyTradeAIRun) error {
	return s.db.Create(run).Error
}

// GetAIRun returns one AI run scoped to a trader (ownership).
func (s *CopyTradeStore) GetAIRun(traderID string, id int64) (*CopyTradeAIRun, error) {
	var run CopyTradeAIRun
	err := s.db.Where("id = ? AND trader_id = ?", id, traderID).First(&run).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// AIStat aggregates model latency for comparison.
type AIStat struct {
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Runs     int64   `json:"runs"`
	Errors   int64   `json:"errors"`
	AvgMs    float64 `json:"avg_ms"`
	MinMs    int64   `json:"min_ms"`
	MaxMs    int64   `json:"max_ms"`
}

// GetAIStats aggregates per-model parsing latency over a period.
func (s *CopyTradeStore) GetAIStats(since time.Time) ([]*AIStat, error) {
	var stats []*AIStat
	err := s.db.Model(&CopyTradeAIRun{}).
		Select(`model, provider,
			COUNT(*) as runs,
			SUM(CASE WHEN error != '' THEN 1 ELSE 0 END) as errors,
			AVG(duration_ms) as avg_ms,
			MIN(duration_ms) as min_ms,
			MAX(duration_ms) as max_ms`).
		Where("started_at >= ?", since).
		Group("model, provider").
		Order("avg_ms ASC").
		Scan(&stats).Error
	return stats, err
}

// --- Signals ---

// CreateSignal inserts a signal record.
func (s *CopyTradeStore) CreateSignal(sig *CopyTradeSignal) error {
	return s.db.Create(sig).Error
}

// UpdateSignal applies partial updates to a signal.
func (s *CopyTradeStore) UpdateSignal(id string, updates map[string]interface{}) error {
	return s.db.Model(&CopyTradeSignal{}).Where("id = ?", id).Updates(updates).Error
}

// GetSignal fetches one signal.
func (s *CopyTradeStore) GetSignal(id string) (*CopyTradeSignal, error) {
	var sig CopyTradeSignal
	if err := s.db.First(&sig, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &sig, nil
}

// SignalProcessed reports whether this exact message revision already reached
// a terminal outcome for this trader (duplicate-delivery guard across
// restarts). Non-terminal rows (received/parsed/executing) do NOT count: a
// crash mid-pipeline must not permanently swallow the revision. Re-execution
// safety comes from the duplicate-open protection and idempotent management
// actions, not from half-finished signal rows.
func (s *CopyTradeStore) SignalProcessed(traderID, messageID string, revision int) (bool, error) {
	var n int64
	err := s.db.Model(&CopyTradeSignal{}).
		Where("trader_id = ? AND message_id = ? AND message_revision = ? AND status IN ?",
			traderID, messageID, revision,
			[]string{SignalStatusExecuted, SignalStatusSkipped, SignalStatusFailed}).
		Count(&n).Error
	return n > 0, err
}

// GetRecentSignals lists a trader's recent signals (newest first).
// start/end are optional time bounds (zero value disables the bound).
func (s *CopyTradeStore) GetRecentSignals(traderID string, start, end time.Time, limit int) ([]*CopyTradeSignal, error) {
	var sigs []*CopyTradeSignal
	q := s.db.Where("trader_id = ?", traderID)
	if !start.IsZero() {
		q = q.Where("message_timestamp >= ?", start)
	}
	if !end.IsZero() {
		q = q.Where("message_timestamp <= ?", end)
	}
	err := q.Order("message_timestamp DESC").Limit(limit).Find(&sigs).Error
	return sigs, err
}

// GetContextSignals returns recent interpreted signals of a channel for prompt
// context (both executed trades and hasExecutionIntent=false setups).
func (s *CopyTradeStore) GetContextSignals(traderID, channelID string, since time.Time, limit int) ([]*CopyTradeSignal, error) {
	var sigs []*CopyTradeSignal
	err := s.db.Where("trader_id = ? AND channel_id = ? AND message_timestamp >= ? AND classification != ''",
		traderID, channelID, since).
		Order("message_timestamp DESC").Limit(limit).Find(&sigs).Error
	return sigs, err
}

// --- Events ---

// AppendEvent writes one trace event.
func (s *CopyTradeStore) AppendEvent(ev *CopyTradeEvent) error {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	return s.db.Create(ev).Error
}

// GetEventsByTrader lists a trader's events (newest first, paginated).
// start/end are optional time bounds (zero value disables the bound).
func (s *CopyTradeStore) GetEventsByTrader(traderID string, start, end time.Time, limit, offset int) ([]*CopyTradeEvent, error) {
	var evs []*CopyTradeEvent
	q := s.db.Where("trader_id = ?", traderID)
	if !start.IsZero() {
		q = q.Where("occurred_at >= ?", start)
	}
	if !end.IsZero() {
		q = q.Where("occurred_at <= ?", end)
	}
	err := q.Order("occurred_at DESC").Limit(limit).Offset(offset).Find(&evs).Error
	return evs, err
}

// GetEventsByTrace lists all events of one trace (oldest first: waterfall),
// scoped to a trader so one user cannot read another trader's traces.
func (s *CopyTradeStore) GetEventsByTrace(traderID, traceID string) ([]*CopyTradeEvent, error) {
	var evs []*CopyTradeEvent
	err := s.db.Where("trader_id = ? AND trace_id = ?", traderID, traceID).
		Order("occurred_at ASC").Find(&evs).Error
	return evs, err
}

// CleanOldEvents removes events older than the retention window.
func (s *CopyTradeStore) CleanOldEvents(days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	result := s.db.Where("occurred_at < ?", cutoff).Delete(&CopyTradeEvent{})
	return result.RowsAffected, result.Error
}

// CleanOldSignals removes terminal signals older than the retention window.
// Non-terminal rows are kept regardless of age (still useful for debugging
// stuck pipelines and never bulky).
func (s *CopyTradeStore) CleanOldSignals(days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	result := s.db.Where("created_at < ? AND status IN ?", cutoff,
		[]string{SignalStatusExecuted, SignalStatusSkipped, SignalStatusFailed}).
		Delete(&CopyTradeSignal{})
	return result.RowsAffected, result.Error
}

// CleanOldAIRuns removes AI run records older than the retention window.
// These rows carry full prompts and raw responses and dominate table growth.
func (s *CopyTradeStore) CleanOldAIRuns(days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	result := s.db.Where("created_at < ?", cutoff).Delete(&CopyTradeAIRun{})
	return result.RowsAffected, result.Error
}
