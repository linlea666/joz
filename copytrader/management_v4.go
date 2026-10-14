package copytrader

import (
	"nofx/store"
	"regexp"
	"strconv"
	"strings"
)

var profitExitV4 = regexp.MustCompile(`止盈\s*(?:出[場场局]?|[離离][場场]|退出)`)
var cancelAddV4 = regexp.MustCompile(`[補补][倉仓](?:[單单])?\s*(?:可以|可|先|都)?\s*撤(?:掉|了)?`)
var replaceTPV4 = regexp.MustCompile(`(?i)TP\s*([1-9][0-9]*)\s*(?:修改|改|改成|改[為为])(?:[為为成])?\s*(?:[這这][裡里裏]|此[處处]|[現现]在)\s*(?:減倉|减仓|止盈)`)
var tpAfterV4 = regexp.MustCompile(`(?i)TP\s*([1-9][0-9]*)\s*(?:(?:已)?(?:成交|到[達达]|完成|觸發|触发|止盈)\s*)?[後后]`)
var retainedV4 = regexp.MustCompile(`(?:留|保留|剩)[下著着餘余]?\s*([0-9]+(?:\.[0-9]+)?)\s*[%％]`)
var discretionaryV4 = regexp.MustCompile(`(?i)考[慮虑]|或[許许]|也[許许]|maybe|consider`)

func hasFullCloseIntent(text string) bool {
	return hasLegacyFullCloseIntent(text) || profitExitV4.MatchString(text)
}

func InterpretKnownSource(msg *store.DiscordMessage, sources []SourceSegment, profile string) *SourceInterpretation {
	return interpretKnownVersion(msg, sources, profile, MessageRulesVersion)
}

func interpretKnownVersion(msg *store.DiscordMessage, sources []SourceSegment, profile string, version int) *SourceInterpretation {
	if version < 4 || profile != "tyler_v1" {
		return interpretKnownSourceLegacy(msg, sources, profile)
	}
	body, sourceID, evidence := tylerCurrentSource(sources, msg.Content)
	special := profitExitV4.MatchString(body) || cancelAddV4.MatchString(body) || replaceTPV4.MatchString(body)
	if !special {
		return interpretKnownSourceLegacy(msg, sources, profile)
	}
	// Only a complete, single-target instruction is a fast path. Unknown
	// clauses stay with the model and still undergo the common source gates.
	if sourceNegation.MatchString(body) || sourceConditional.MatchString(body) || sourceOpen.MatchString(body) || discretionaryV4.MatchString(body) {
		return nil
	}
	symbols := map[string]bool{}
	for _, m := range sourceSymbol.FindAllStringSubmatch(body, -1) {
		symbols[strings.ToUpper(m[1])] = true
	}
	if len(symbols) != 1 {
		return nil
	}
	sym := ""
	for s := range symbols {
		sym = s
	}
	ins := &SourceInterpretation{Classification: ClassificationSignal, Symbol: sym, ActionEvidence: &ActionEvidence{SourceID: sourceID, Text: evidence}, Reasoning: "TYLER management rules v4"}
	if reason := unresolvedManagement(body); reason != "" {
		ins.Classification = ClassificationAmbiguous
		ins.Action = ActionIgnore
		ins.Reasoning = reason
		return ins
	}
	rest := cancelAddV4.ReplaceAllString(body, "")
	if m := replaceTPV4.FindStringSubmatch(body); len(m) > 1 {
		if len(replaceTPV4.FindAllString(body, -1)) != 1 {
			return nil
		}
		rest = replaceTPV4.ReplaceAllString(rest, "")
		if sourceManagement.MatchString(rest) || sourceCancel.MatchString(rest) || sourceAdd.MatchString(rest) {
			return nil
		}
		level, _ := strconv.Atoi(m[1])
		ins.Action, ins.CloseMode, ins.ReduceTPLevel = ActionReduce, CloseModePartial, level
		ins.CancelPendingEntry = cancelAddV4.MatchString(body)
		if _, explicit, err := explicitReductionRatio(body); explicit || err != nil || retainedPosition.MatchString(body) {
			ins.Classification = ClassificationAmbiguous
			ins.Reasoning = "numbered TP replacement conflicts with a separate portion"
		}
		return ins
	}
	if profitExitV4.MatchString(body) {
		rest = profitExitV4.ReplaceAllString(rest, "")
		if sourceManagement.MatchString(rest) || sourceCancel.MatchString(rest) || sourceAdd.MatchString(rest) {
			return nil
		}
		ins.Action, ins.CloseMode = ActionClose, CloseModeFull
		ratio, explicit, err := explicitReductionRatio(body)
		if err != nil {
			ins.Classification = ClassificationAmbiguous
			ins.Reasoning = err.Error()
			return ins
		}
		if explicit && ratio < 100 {
			ins.Action, ins.CloseMode, ins.CloseRatio = ActionReduce, CloseModePartial, &ratio
		}
		applyRetainedRatioV4(ins, body)
		return ins
	}
	if sourceManagement.MatchString(rest) || sourceCancel.MatchString(rest) || sourceAdd.MatchString(rest) {
		return nil
	}
	ins.Action = ActionCancel
	return ins
}

func applyRetainedRatioV4(ins *SourceInterpretation, scope string) {
	matches := retainedV4.FindAllStringSubmatch(scope, -1)
	if len(matches) == 0 {
		return
	}
	v, _ := strconv.ParseFloat(matches[0][1], 64)
	explicit, has, err := explicitReductionRatio(scope)
	if len(matches) != 1 || v <= 0 || v >= 100 || err != nil || has && explicit != 100-v {
		ins.Classification = ClassificationAmbiguous
		ins.Reasoning = "conflicting retained-position portion"
		return
	}
	ratio := 100 - v
	ins.Action, ins.CloseMode, ins.CloseRatio = ActionReduce, CloseModePartial, &ratio
}

func requiredTPFillV4(scope string) (int, bool) {
	ids := map[int]bool{}
	for _, pattern := range []*regexp.Regexp{tpAfterEnglish, tpAfterV4} {
		for _, m := range pattern.FindAllStringSubmatch(scope, -1) {
			n, _ := strconv.Atoi(m[1])
			ids[n] = true
		}
	}
	if len(ids) > 1 {
		return 0, true
	}
	for n := range ids {
		return n, false
	}
	// Preserve the old explicit "止盈后" ambiguity handling, without the
	// permissive TP...后 expression that consumed "修改为这里减仓后再拿".
	if strings.Contains(scope, "止盈後") || strings.Contains(scope, "止盈后") {
		for _, m := range sourceTPLevel.FindAllStringSubmatch(scope, -1) {
			n, _ := strconv.Atoi(m[1])
			ids[n] = true
		}
		if len(ids) == 1 {
			for n := range ids {
				return n, false
			}
		}
		return 0, true
	}
	return 0, false
}

var managementSeparatorsV4 = regexp.MustCompile(`[\n，,；;。]+`)
var stopClauseV4 = regexp.MustCompile(`(?i)止[損损]|\bSL\b|保本`)

// Retain prerequisites attached to the selected action, not to a different
// action later in the same symbol block. Unsupported/ambiguous scopes remain
// conservative and are rejected by the common evidence validator.
func managementScopeV4(text string, ins *SourceInterpretation) string {
	if ins.Action != ActionReduce && ins.Action != ActionClose && ins.Action != ActionUpdateSL {
		return text
	}
	parts := managementSeparatorsV4.Split(text, -1)
	var selected []string
	pending := ""
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		isStop := stopClauseV4.MatchString(p)
		isExit := sourceReduce.MatchString(p) || hasFullCloseIntent(p) || replaceTPV4.MatchString(p)
		matches := isExit && ins.Action != ActionUpdateSL || isStop && ins.Action == ActionUpdateSL
		if matches {
			selected = append(selected, pending+p)
			pending = ""
			continue
		}
		if sourceConditional.MatchString(p) || tpAfterV4.MatchString(p) || tpAfterEnglish.MatchString(p) || sourceAddCondition.MatchString(p) {
			pending += p + "，"
			continue
		}
		if retainedV4.MatchString(p) && len(selected) > 0 {
			selected[len(selected)-1] += "，" + p
		}
		if isStop || isExit || sourceCancel.MatchString(p) {
			pending = ""
		}
	}
	if len(selected) != 1 {
		return text
	}
	return selected[0]
}
