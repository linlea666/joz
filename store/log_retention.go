package store

import "gorm.io/gorm"

// Protect evidence by persistent lifecycle and unresolved intents, never by
// whichever trader configuration happens to be loaded now.
func protectedContexts(db *gorm.DB) *gorm.DB {
	return db.Model(&CopyTradeContext{}).Select("id").Where(`state IN ? OR entry_working = ? OR stop_intent_json <> '' OR tp_update_intent_json <> '' OR close_pending_json <> '' OR id IN (SELECT context_id FROM copytrade_actions WHERE status IN ?) OR id IN (SELECT context_id FROM copytrade_orders WHERE status IN ?)`, activeStates, true, []string{"executing", "uncertain"}, []string{"PLANNED", "SUBMITTING", "UNKNOWN", "NEW", "PARTIALLY_FILLED"})
}
func protectedSignals(db *gorm.DB) *gorm.DB {
	contexts := protectedContexts(db)
	return db.Model(&CopyTradeSignal{}).Select("id").Where(`status NOT IN ? OR next_retry_at IS NOT NULL OR trade_context_id IN (?) OR id IN (SELECT entry_signal_id FROM copytrade_trade_contexts WHERE id IN (?)) OR id IN (SELECT last_tp_signal_id FROM copytrade_trade_contexts WHERE id IN (?)) OR id IN (SELECT signal_id FROM copytrade_actions WHERE status IN ? OR context_id IN (?)) OR id IN (SELECT signal_id FROM copytrade_orders WHERE context_id IN (?))`, []string{SignalStatusExecuted, SignalStatusSkipped, SignalStatusFailed}, contexts, contexts, contexts, []string{"executing", "uncertain"}, contexts, contexts)
}
func protectedMessageIDs(db *gorm.DB) *gorm.DB {
	return db.Model(&CopyTradeSignal{}).Select("message_id").Where("id IN (?)", protectedSignals(db))
}

// HasProtectedMedia conservatively retains cached media while any lifecycle
// requires evidence; files are content addressed and do not carry a unique owner.
func (s *CopyTradeStore) HasProtectedMedia() (bool, error) {
	var n int64
	err := s.db.Table("copytrade_signals").Where("id IN (?)", protectedSignals(s.db)).Count(&n).Error
	if err != nil || n > 0 {
		return n > 0, err
	}
	err = s.db.Model(&CopyTradeContext{}).Where("id IN (?)", protectedContexts(s.db)).Count(&n).Error
	return n > 0, err
}
