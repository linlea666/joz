package store

import "time"

// RecognitionStats counts retained live interpretation records, including
// failed calls. Replay records are separate. This is not a lifetime counter.
type RecognitionStats struct {
	ModelCalls        int64      `json:"model_calls"`
	DeterministicRuns int64      `json:"deterministic_runs"`
	RecognitionRuns   int64      `json:"recognition_runs"`
	EarliestRun       *time.Time `json:"earliest_run,omitempty"`
}

func (s *CopyTradeStore) RecognitionStats(traderID string) (*RecognitionStats, error) {
	var stats RecognitionStats
	err := s.db.Model(&CopyTradeAIRun{}).Where("trader_id = ?", traderID).
		Select("COUNT(*) AS recognition_runs, COALESCE(SUM(CASE WHEN model LIKE 'deterministic:%' THEN 1 ELSE 0 END),0) AS deterministic_runs, COALESCE(SUM(CASE WHEN model LIKE 'deterministic:%' THEN 0 ELSE 1 END),0) AS model_calls").Scan(&stats).Error
	if err != nil {
		return nil, err
	}
	var first []CopyTradeAIRun
	if err := s.db.Select("started_at").Where("trader_id = ?", traderID).Order("started_at ASC").Limit(1).Find(&first).Error; err != nil {
		return nil, err
	}
	if len(first) > 0 {
		stats.EarliestRun = &first[0].StartedAt
	}
	return &stats, nil
}
