package copytrader

// Presets describe author language, never exchange execution. The same catalog
// supplies validation, prompt policy and the configuration UI's recommendations.
type ProfileRecommendations struct {
	DualPriceMode string  `json:"market_dual_price_mode"`
	ReduceRatio   float64 `json:"default_reduce_ratio"`
	Notes         string  `json:"channel_notes"`
}
type InterpretationPreset struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Recommended      ProfileRecommendations `json:"recommended_config"`
	Prompt           string                 `json:"-"`
	SplitQuotes      bool                   `json:"-"`
	StrictManagement bool                   `json:"-"`
}

var interpretationPresets = []InterpretationPreset{
	{ID: "default", Name: "默认 / Default", Description: "通用解释，保留现有参数；不启用作者专属口令。 General interpretation without author-specific shorthand.", Recommended: ProfileRecommendations{DualPriceLegacy, 50, ""}},
	{ID: "cmm_v1", Name: "CMM", SplitQuotes: true, StrictManagement: true,
		Description: "信号卡与操作通知分别识别；收益播报、旧卡引用不重新开仓；含糊建议不自动减仓。 Distinguishes cards, instructions and recaps; vague suggestions do not authorize exits.",
		Recommended: ProfileRecommendations{DualPriceSplit, 50, "CMM：新信号卡包含币种、方向、进场、止盈和止损。操作通知需与原交易关联。收益播报、TP 到达提醒和引用旧卡不代表重新开仓。可以先跑、自行止盈等含糊建议不等同明确减仓。作者仓位和杠杆说明不覆盖本地配置。"},
		Prompt:      "CMM: distinguish new signal cards, explicit management and performance recaps. Quoted cards never authorize a new entry. Vague suggestions such as 可以先跑/自行止盈 are AMBIGUOUS, not REDUCE. Explicit reduction without a stated fraction uses the effective default_reduce_ratio."},
	{ID: "tyler_v1", Name: "TYLER", SplitQuotes: true,
		Description: "保留已验证的 TYLER 管理口令；补仓、TP 成交和重仓条件不能省略。 Retains verified TYLER phrases and their eligibility conditions.",
		Recommended: ProfileRecommendations{DualPriceLegacy, 50, "TYLER：新信号与旧卡引用分开。起飞、倍数收益、TP 到达属于播报。止盈或者减仓、可以先跑等已确认话术表示部分退出；明确全平表示结束交易。保留有补仓、TP 成交后、仓位重等条件。作者仓位及杠杆描述不作为跟单准入条件。"},
		Prompt:      "TYLER: celebration / TP hit alone is IGNORE. Verified discretionary partial profit phrases (止盈或者减仓, 可以先跑) use default_reduce_ratio unless an explicit ratio is stated. Full exit remains CLOSE. Retain all eligibility conditions."},
	{ID: "jonzi_v1", Name: "jonzi", SplitQuotes: true, StrictManagement: true,
		Description: "原卡编辑与中英重复作为同一交易处理；结束卡只管理原卡对应交易。 Card edits and translations describe one trade; terminal cards require an exact original-message match.",
		Recommended: ProfileRecommendations{DualPriceLegacy, 50, "jonzi：原信号卡会编辑入场、移动止损和结束状态。中英文重复描述同一动作，不执行两次；关键数值冲突时等待核对。原卡已手动平仓或止损触发表示结束该原卡交易。引用旧卡和收益播报不发起交易。仓位示例仅为作者说明，执行数量取本地风控。"},
		Prompt:      "jonzi: native card edits remain current; timestamps alone do not make them historical. English and Chinese translations describe one action, not two; conflicting directions, prices or ratios are AMBIGUOUS. Terminal native cards cannot OPEN and may CLOSE only the trade rooted in this exact message. Trailed stop is the current stop; original Stop/loss is historical when a trailed stop is present. Vague discretionary exit suggestions are AMBIGUOUS."},
}

func LookupInterpretationPreset(id string) (InterpretationPreset, bool) {
	if id == "" {
		id = "default"
	}
	for _, p := range interpretationPresets {
		if p.ID == id {
			return p, true
		}
	}
	return InterpretationPreset{}, false
}
func InterpretationPresets() []InterpretationPreset {
	return append([]InterpretationPreset(nil), interpretationPresets...)
}
func splitQuotedSources(profile string) bool {
	p, ok := LookupInterpretationPreset(profile)
	return ok && p.SplitQuotes
}
