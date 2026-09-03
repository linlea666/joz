// Discord copy-trading types

export interface DiscordChannelStatus {
  channel_id: string
  subscribers: number
  last_message_at?: string
  last_error?: string
  paused_until?: string
}

export interface DiscordConfig {
  configured: boolean
  token_masked: string
  poll_interval_seconds: number
  enabled: boolean
  channels?: DiscordChannelStatus[]
}

export interface DiscordChannelPreviewMessage {
  message_id: string
  author_id: string
  author_name: string
  content: string
  timestamp: string
  has_embeds: boolean
  attachments: number
  edited: boolean
}

// Per-trader copy trading config (mirrors Go copytrader.CopyTradingConfig)
export interface CopyTradingConfig {
  primary_channel_id: string
  source_author_ids?: string[]
  channel_notes?: string

  parse_images: boolean
  send_position_snapshot: boolean
  signal_context_enabled: boolean
  context_lookback_days: number

  risk_mode: 'by_loss' | 'percent' | 'fixed'
  risk_amount_usd: number
  max_position_notional_usd: number
  max_open_positions: number
  major_leverage: number
  altcoin_leverage: number

  major_price_offset_pct: number
  altcoin_price_offset_pct: number
  limit_to_market_within_threshold: boolean
  open_signal_ttl_seconds: number
  management_signal_ttl_seconds: number
  entry_timeout_minutes: number

  default_tp_ratios: string
  auto_breakeven_after_tp: boolean

  duplicate_open_protection: boolean
  paused: boolean
}

export const DEFAULT_COPY_TRADING_CONFIG: CopyTradingConfig = {
  primary_channel_id: '',
  channel_notes: '',
  parse_images: true,
  send_position_snapshot: true,
  signal_context_enabled: true,
  context_lookback_days: 5,
  risk_mode: 'by_loss',
  risk_amount_usd: 50,
  max_position_notional_usd: 30000,
  max_open_positions: 5,
  major_leverage: 20,
  altcoin_leverage: 10,
  major_price_offset_pct: 0.3,
  altcoin_price_offset_pct: 1.0,
  limit_to_market_within_threshold: true,
  open_signal_ttl_seconds: 300,
  management_signal_ttl_seconds: 1800,
  entry_timeout_minutes: 240,
  default_tp_ratios: '50,30,20',
  auto_breakeven_after_tp: false,
  duplicate_open_protection: true,
  paused: false,
}

// Observability records (mirror Go store/copytrade.go JSON tags)

export interface CopyTradeEvent {
  id: number
  trace_id: string
  signal_id: string
  trader_id: string
  channel_id: string
  message_id: string
  level: 'info' | 'success' | 'warn' | 'error'
  event: string
  message: string
  context_json?: string
  duration_ms: number
  occurred_at: string
  created_at: string
}

export interface CopyTradeSignal {
  id: string
  trader_id: string
  channel_id: string
  message_id: string
  message_revision: number
  ai_run_id: number
  trade_context_id: string
  classification: string
  action: string
  symbol: string
  direction: string
  interpretation_json: string
  has_execution_intent: boolean
  status: string
  skip_reason: string
  error_message: string
  message_timestamp: string
  received_at: string
  receive_latency_ms: number
  media_download_ms: number
  prompt_build_ms: number
  llm_request_ms: number
  risk_calc_ms: number
  exchange_submit_ms: number
  total_ms: number
  created_at: string
}

export interface CopyTradeContext {
  id: string
  trader_id: string
  channel_id: string
  root_message_id: string
  symbol: string
  raw_symbol: string
  direction: string
  state: string
  planned_entry_price: number
  avg_fill_price: number
  quantity: number
  leverage: number
  stop_loss_price: number
  tp_plan_json: string
  tp_hit_count: number
  breakeven_applied: boolean
  entry_order_id: string
  last_action: string
  last_error: string
  opened_at?: string
  closed_at?: string
  created_at: string
  updated_at: string
}

export interface CopyTradeAIStat {
  model: string
  provider: string
  runs: number
  errors: number
  avg_ms: number
  min_ms: number
  max_ms: number
}
