package copytrader

import (
	"encoding/json"
	"fmt"
	"nofx/store"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var discretionaryExit = regexp.MustCompile(`(?i)(可以先跑|自行.*(?:止盈|減倉|减仓|TP)|止盈或者減倉|止盈或者减仓|consider.*(?:clos|exit|profit)|(?:may|could).*\b(?:close|exit|trim)\b)`)
var explicitManagement = regexp.MustCompile(`(?i)(减仓|減倉|减掉|減掉|減半|减半|全平|平仓|平倉|\b(?:reduce|trim|close|exit)\b|(?:止盈|take profit)\s*[0-9一二三四五六七八九十])`)
var recapOnly = regexp.MustCompile(`(?i)(收益|获利|獲利|起飞|起飛|(?:TP\s*\d|止盈).*(?:到达|到達|已到|hit|完成|✅))`)
var jonziTerminal = regexp.MustCompile(`(?i)(trade was (?:manually )?closed|stop/loss was hit|trailed stop was hit|交易已(?:手动|手動)?(?:平仓|平倉|关闭|關閉)|(?:跟踪|追踪|跟蹤|追蹤)?止[損损].*(?:被觸發|被触发|已觸發|已触发))`)
var cardSymbol = regexp.MustCompile(`(?i)\b([A-Z0-9]+)/(?:USDT|USD)\b`)
var cardDirection = regexp.MustCompile(`(?i)\b(long|short)\b|做多|做空|多头|多頭|空头|空頭`)
var cardField = regexp.MustCompile(`(?i)^\s*[-•]?\s*(entry|入场价?|入場價?|trailed stop|移动止损|移動止損|跟踪止损|追踪止损|stop/loss|止损(?:\s*\(stop/loss\))?|止損|tp|止盈(?:\s*\(tp\))?)\s*[:：]\s*(.*)$`)
var cardNumber = regexp.MustCompile(`[0-9]+(?:,[0-9]{3})*(?:\.[0-9]+)?`)

func currentPresetText(segments []SourceSegment) string {
	var parts []string
	for _, s := range segments {
		if s.Role == "current" && !s.Image {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n")
}
func isJonziCard(text string) bool {
	return cardSymbol.MatchString(text) && (strings.Contains(text, "Trade details") || strings.Contains(text, "交易详情") || strings.Contains(text, "交易詳情"))
}

func interpretPresetSource(msg *store.DiscordMessage, segments []SourceSegment, profile string) *SourceInterpretation {
	if profile != "jonzi_v1" {
		return nil
	}
	for _, s := range segments {
		if s.Role != "current" || s.Image || !isJonziCard(s.Text) || !jonziTerminal.MatchString(s.Text) {
			continue
		}
		symbol, direction, err := jonziCardIdentity(s.Text)
		if err != nil {
			return &SourceInterpretation{Classification: ClassificationAmbiguous, Action: ActionIgnore, Reasoning: err.Error()}
		}
		return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, Symbol: symbol, Direction: direction, CloseMode: CloseModeFull,
			TradeReference: TradeReference{RootMessageID: msg.MessageID}, ActionEvidence: &ActionEvidence{SourceID: s.ID, Text: s.Text}, RequiresOriginalCard: true,
			Reasoning: "jonzi native terminal card; exact original-message association required"}
	}
	return nil
}

func jonziCardIdentity(text string) (string, Direction, error) {
	symbols := map[string]bool{}
	dirs := map[Direction]bool{}
	for _, m := range cardSymbol.FindAllStringSubmatch(text, -1) {
		symbols[strings.ToUpper(m[1])+"USDT"] = true
	}
	// Direction is a header fact, not a mention in the reasoning paragraph.
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range cardDirection.FindAllString(line, -1) {
			d := DirectionLong
			if strings.EqualFold(m, "short") || strings.Contains(m, "空") {
				d = DirectionShort
			}
			dirs[d] = true
		}
	}
	if len(symbols) != 1 || len(dirs) != 1 {
		return "", "", fmt.Errorf("card symbol/direction is missing or conflicts across translations")
	}
	var sym string
	var direction Direction
	for s := range symbols {
		sym = s
	}
	for d := range dirs {
		direction = d
	}
	return sym, direction, nil
}

func conflictingCardPrices(text string) bool {
	fields := map[string]string{}
	clean := strings.NewReplacer("`", "", "*", "", "$", "").Replace(text)
	for _, line := range strings.Split(clean, "\n") {
		m := cardField.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		key := strings.ToLower(m[1])
		switch {
		case strings.HasPrefix(key, "entry") || strings.HasPrefix(key, "入"):
			key = "entry"
		case key == "tp" || strings.HasPrefix(key, "止盈"):
			key = "tp"
		case strings.Contains(key, "trailed") || strings.Contains(key, "移动") || strings.Contains(key, "移動") || strings.Contains(key, "跟踪") || strings.Contains(key, "追踪"):
			key = "trailed"
		default:
			key = "stop"
		}
		body := strings.Split(strings.Split(m[2], "<")[0], ":Chroma")[0]
		numbers := cardNumber.FindAllString(body, -1)
		if len(numbers) == 0 {
			continue
		}
		for i, n := range numbers {
			v, err := strconv.ParseFloat(strings.ReplaceAll(n, ",", ""), 64)
			if err != nil || !finite(v) || v <= 0 {
				return true
			}
			numbers[i] = strconv.FormatFloat(v, 'g', -1, 64)
		}
		value := strings.Join(numbers, ",")
		if old, ok := fields[key]; ok && old != value {
			return true
		}
		fields[key] = value
	}
	return false
}

func applyPresetPolicy(interp *SourceInterpretation, msg *store.DiscordMessage, sources []SourceSegment, rules MessageRules) *SourceInterpretation {
	preset, ok := LookupInterpretationPreset(rules.Profile)
	if !ok {
		return &SourceInterpretation{Classification: ClassificationUnsupported, Action: ActionIgnore, Reasoning: "unknown author preset"}
	}
	if interp == nil {
		return &SourceInterpretation{Classification: ClassificationIgnore, Action: ActionIgnore}
	}
	text := currentPresetText(sources)
	for _, ins := range interp.Flatten() {
		scope := evidenceScopeForSymbol(text, ins.Symbol)
		if preset.StrictManagement && (ins.Action == ActionReduce || ins.Action == ActionClose) && discretionaryExit.MatchString(scope) {
			ins.Classification = ClassificationAmbiguous
			ins.Reasoning = "preset requires explicit management, not a discretionary suggestion"
		}
		if preset.ID != "default" && (ins.Action == ActionReduce || ins.Action == ActionClose) && recapOnly.MatchString(scope) && !explicitManagement.MatchString(scope) && !jonziTerminal.MatchString(scope) {
			ins.Classification = ClassificationIgnore
			ins.Reasoning = "performance recap is not a management instruction"
		}
		if rules.Profile == "jonzi_v1" && isJonziCard(scope) {
			symbol, direction, err := jonziCardIdentity(scope)
			canonical, _ := ResolveInstrument(ins.Symbol)
			if err != nil || conflictingCardPrices(scope) || (canonical != "" && canonical != symbol) || (ins.Direction != "" && ins.Direction != direction) {
				ins.Classification = ClassificationAmbiguous
				ins.Reasoning = "conflicting native card facts"
				continue
			}
			if jonziTerminal.MatchString(scope) {
				if ins.Action == ActionClose {
					ins.RequiresOriginalCard = true
					ins.TradeReference = TradeReference{RootMessageID: msg.MessageID}
				} else {
					ins.Classification = ClassificationIgnore
					ins.Reasoning = "terminal card cannot authorize a new entry or partial exit"
				}
			}
		}
	}
	if rules.Profile == "jonzi_v1" && isJonziCard(text) && len(interp.Instructions) > 1 {
		seen := map[string]*SourceInterpretation{}
		var merged []*SourceInterpretation
		for _, ins := range interp.Instructions {
			canonical, _ := ResolveInstrument(ins.Symbol)
			key := canonical + "/" + string(ins.Action) + "/" + ins.TradeReference.RootMessageID
			if prior := seen[key]; prior != nil {
				if duplicateInstructionFacts(prior) != duplicateInstructionFacts(ins) {
					prior.Classification = ClassificationAmbiguous
					prior.Reasoning = "translation instructions conflict"
				}
				continue
			}
			seen[key] = ins
			merged = append(merged, ins)
		}
		interp.Instructions = merged
		if len(merged) == 1 {
			return merged[0]
		}
	}
	return interp
}

// Compare execution facts only, after shared ratio normalization. Missing and
// empty optional arrays are equivalent; prose and evidence locations are not facts.
func duplicateInstructionFacts(ins *SourceInterpretation) string {
	v := *ins
	v.Symbol = ""
	v.ActionEvidence = nil
	v.Reasoning = ""
	v.Warnings = nil
	v.Confidence = nil
	v.SourceInfo = SourceInfo{}
	v.TradeReference.Confidence = 0
	if len(v.EntryOrders) == 0 {
		v.EntryOrders = nil
	}
	if len(v.StopLossLevels) == 0 {
		v.StopLossLevels = nil
	}
	if len(v.TakeProfitLevels) == 0 {
		v.TakeProfitLevels = nil
	} else {
		v.TakeProfitLevels = append([]TPLevel(nil), v.TakeProfitLevels...)
		for i := range v.TakeProfitLevels {
			v.TakeProfitLevels[i].RatioSource = ""
		}
	}
	if len(v.ConditionalRules) == 0 {
		v.ConditionalRules = nil
	}
	v.EligibilityConditions = append([]string(nil), v.EligibilityConditions...)
	sort.Strings(v.EligibilityConditions)
	b, _ := json.Marshal(v)
	return string(b)
}
