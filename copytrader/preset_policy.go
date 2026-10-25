package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/store"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var discretionaryExit = regexp.MustCompile(`(?i)(可以先跑|自行.*(?:止盈|減倉|减仓|TP)|止盈或者減倉|止盈或者减仓|consider.*(?:clos|exit|profit)|(?:may|could).*\b(?:close|exit|trim)\b)`)
var explicitManagement = regexp.MustCompile(`(?i)(减仓|減倉|减掉|減掉|減半|减半|全平|平仓|平倉|\b(?:reduce|trim|close|exit)\b|(?:止盈|take profit)\s*[0-9一二三四五六七八九十])`)
var cardSymbol = regexp.MustCompile(`(?i)\b([A-Z0-9]+)/(?:USDT|USD)\b`)
var cardDirection = regexp.MustCompile(`(?i)\b(long|short)\b|做多|做空|多头|多頭|空头|空頭`)
var cardField = regexp.MustCompile(`(?i)^\s*[-•]?\s*(entry|入场价?|入場價?|trailed stop|移动止损|移動止損|跟踪止损|追踪止损|stop/loss|止损(?:\s*\(stop/loss\))?|止損|tp|止盈(?:\s*\(tp\))?)\s*[:：]\s*(.*)$`)
var cardNumber = regexp.MustCompile(`[0-9]+(?:,[0-9]{3})*(?:\.[0-9]+)?`)

var unresolvedProtection = regexp.MustCompile(`上套(?:保)?|套保`)
var managementAlternative = regexp.MustCompile(`(?i)或者|或是|\bor\b`)
var exitWording = regexp.MustCompile(`(?i)减仓|減倉|止盈|平仓|平倉|\b(?:reduce|trim|close|exit)\b`)
var holdWording = regexp.MustCompile(`(?i)持有|持倉|持仓|\bhold\b`)

// A recognized fragment is not permission to execute only part of an
// unresolved instruction. This boundary is shared by deterministic and AI paths.
func unresolvedManagement(text string) string {
	if sourceFullProfitExit.MatchString(text) && retainedPosition.MatchString(text) {
		return "full exit conflicts with retained-position wording"
	}
	if unresolvedProtection.MatchString(text) {
		return "management contains undefined protection wording (上套/套保); explicit stop target required"
	}
	if managementAlternative.MatchString(text) && exitWording.MatchString(text) && (sourceBreakeven.MatchString(text) || holdWording.MatchString(text)) {
		return "management offers alternative exit/protection/holding actions; no unique action selected"
	}
	return ""
}

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
	return interpretJonziSource(msg, segments)
}

// currentPresetGateText is currentPresetText with a jonzi native card narrowed
// to its status line(s) and Trade details block (SourceSegment.GateScope).
func currentPresetGateText(segments []SourceSegment) string {
	var parts []string
	for _, s := range segments {
		if s.Role == "current" && !s.Image {
			if s.GateScope != "" {
				parts = append(parts, s.GateScope)
			} else {
				parts = append(parts, s.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// jonziBodyCard returns the message's own native card, if its whole body is one.
func jonziBodyCard(segments []SourceSegment) (*SourceSegment, jonziCard) {
	for i := range segments {
		s := &segments[i]
		if s.ID == "body" && s.Role == "current" && !s.Image {
			if card := parseJonziCard(s.Text); card.IsCard {
				return s, card
			}
			return nil, jonziCard{}
		}
	}
	return nil, jonziCard{}
}

func jonziEmbeddedCard(segments []SourceSegment) bool {
	for _, s := range segments {
		if strings.HasPrefix(s.ID, "embed:") && !s.Image && parseJonziCard(s.Text).IsCard {
			return true
		}
	}
	return false
}

// interpretJonziSource is the deterministic jonzi_v1 route:
//   - revision-0 native card, NEW, ticked entry, complete fields -> OPEN
//   - native card TERMINAL -> full CLOSE; TRAILED -> UPDATE_SL (original card)
//   - any other edit of the card -> IGNORE; commentary with an embedded card -> IGNORE
//
// It returns nil only where the model is still consulted: a revision-0 card
// that is not deterministic (pending entry, missing field, translation price
// conflict), a partial manual close, and non-card messages without an embedded
// card. Every model OPEN/ADD is re-checked by guardJonziOpenAuthority.
func interpretJonziSource(msg *store.DiscordMessage, segments []SourceSegment) *SourceInterpretation {
	seg, card := jonziBodyCard(segments)
	ignore := func(class Classification, reason string) *SourceInterpretation {
		return &SourceInterpretation{Classification: class, Action: ActionIgnore, Symbol: card.Symbol, Direction: card.Direction, Reasoning: "jonzi_v1: " + reason}
	}
	if seg == nil {
		if jonziEmbeddedCard(segments) {
			return ignore(ClassificationIgnore, "commentary or forward carrying an embedded card; exits follow the original card's edits and embedded cards never open")
		}
		return nil
	}
	if card.Err != "" {
		return ignore(ClassificationAmbiguous, card.Err)
	}
	if card.IdentityConflict != "" {
		return ignore(ClassificationAmbiguous, card.IdentityConflict)
	}
	evidence := &ActionEvidence{SourceID: seg.ID, Text: card.StatusLine}
	switch card.Status {
	case jonziCardTerminal:
		// Prices are irrelevant to a full close; only the identity must agree.
		return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, Symbol: card.Symbol, Direction: card.Direction, CloseMode: CloseModeFull,
			TradeReference: TradeReference{RootMessageID: msg.MessageID}, ActionEvidence: evidence, RequiresOriginalCard: true,
			Reasoning: "jonzi native terminal card; exact original-message association required"}
	case jonziCardTrailed:
		if card.PriceConflict != "" {
			return ignore(ClassificationAmbiguous, card.PriceConflict)
		}
		return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: card.Symbol, Direction: card.Direction,
			StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: card.TrailedStop.Value}}},
			TradeReference: TradeReference{RootMessageID: msg.MessageID}, ActionEvidence: evidence, RequiresOriginalCard: true,
			Reasoning: "jonzi native card trailed stop; exact original-message association required"}
	case jonziCardPartial:
		if card.PriceConflict != "" {
			return ignore(ClassificationAmbiguous, card.PriceConflict)
		}
		return nil // model decides the partial exit; it can never open (guardJonziOpenAuthority)
	default:
		if msg.Revision > 0 {
			return ignore(ClassificationIgnore, fmt.Sprintf("edit (revision %d) of the original card without a trailed, terminal or partial-close change", msg.Revision))
		}
		// Revision 0: a complete, ticked card opens deterministically; anything
		// less (pending entry, missing field, translation conflict) goes to the
		// model, whose OPEN is re-checked by guardJonziOpenAuthority.
		return jonziDeterministicOpen(msg, seg, card)
	}
}

// jonziDeterministicOpen builds the OPEN for a revision-0 native card whose
// entry is already ticked (:ChromaCheck2:) — the author is in the trade at the
// card's entry. It is the same shape the model emits for "market with a stated
// reference": MARKET order carrying the English entry as reference price, a
// fixed stop/loss and fixed TPs. It returns nil (model route) whenever a fact
// is missing, inconsistent or already triggered.
func jonziDeterministicOpen(msg *store.DiscordMessage, seg *SourceSegment, card jonziCard) *SourceInterpretation {
	if msg.Revision != 0 || card.Status != jonziCardNew || card.Struck || len(card.StatusLines) > 0 || card.TrailedStop != nil || card.ManualClosePct > 0 {
		return nil
	}
	if card.Err != "" || card.IdentityConflict != "" || card.PriceConflict != "" || card.Direction == "" || card.Symbol == "" {
		return nil
	}
	if card.Entry == nil || !card.Entry.Checked || card.StopLoss == nil || card.StopLoss.Checked || len(card.TakeProfits) == 0 {
		return nil
	}
	entry, sl := card.Entry.Value, card.StopLoss.Value
	if (card.Direction == DirectionLong && sl >= entry) || (card.Direction == DirectionShort && sl <= entry) {
		return nil
	}
	tps := make([]TPLevel, 0, len(card.TakeProfits))
	for _, tp := range card.TakeProfits {
		if tp.Checked || (card.Direction == DirectionLong && tp.Value <= entry) || (card.Direction == DirectionShort && tp.Value >= entry) {
			return nil
		}
		tps = append(tps, TPLevel{Price: PriceSpec{Type: PriceFixed, Price: tp.Value}})
	}
	return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: card.Symbol, Direction: card.Direction,
		EntryOrders:      []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: entry}}},
		StopLossLevels:   []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: sl}}},
		TakeProfitLevels: tps,
		ActionEvidence:   &ActionEvidence{SourceID: seg.ID, Text: card.Entry.Line},
		Reasoning:        "jonzi native card (revision 0) with ticked entry; market entry with the card entry as reference, fixed stop/loss and TP from the English Trade details"}
}

// jonziModelOpenMismatch compares a model OPEN against the English card facts
// that parsed. English values are authoritative; a translated value can never
// move an entry or stop.
func jonziModelOpenMismatch(ins *SourceInterpretation, card jonziCard) string {
	same := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b)) }
	if card.Entry != nil {
		for _, o := range ins.EntryOrders {
			if o.Price.Type == PriceRange {
				return "model entry range differs from the English card entry"
			}
			if o.Price.Price > 0 && !same(o.Price.Price, card.Entry.Value) {
				return "model entry price differs from the English card entry"
			}
		}
	}
	if card.StopLoss != nil && len(ins.StopLossLevels) > 0 {
		p := ins.StopLossLevels[0].Price
		if p.Type != PriceFixed || !same(p.Price, card.StopLoss.Value) {
			return "model stop/loss differs from the English card stop/loss"
		}
	}
	return ""
}

// jonziOpenBlockReason explains why this message cannot authorize OPEN/ADD,
// or returns "" for a revision-0 native card without management state.
func jonziOpenBlockReason(msg *store.DiscordMessage, sources []SourceSegment) (string, jonziCard) {
	seg, card := jonziBodyCard(sources)
	switch {
	case seg == nil && jonziEmbeddedCard(sources):
		return "commentary or forward carrying an embedded card", card
	case seg == nil:
		return "message is not a native jonzi card", card
	case msg.Revision > 0:
		return fmt.Sprintf("edit (revision %d) of the original card", msg.Revision), card
	case card.Status != jonziCardNew:
		return "card status is " + string(card.Status), card
	case card.Err != "":
		return card.Err, card
	case card.IdentityConflict != "":
		return card.IdentityConflict, card
	case card.StopLoss != nil && card.StopLoss.Checked:
		return "card stop/loss already triggered", card
	}
	for _, tp := range card.TakeProfits {
		if tp.Checked {
			return "card take profit already triggered", card
		}
	}
	// A translation price conflict still reaches the model, but only English
	// card values may be executed (jonziModelOpenMismatch).
	return "", card
}

// guardJonziOpenAuthority downgrades every OPEN/ADD that does not come from a
// revision-0 native card's own body. It only ever removes authority.
func guardJonziOpenAuthority(interp *SourceInterpretation, msg *store.DiscordMessage, sources []SourceSegment) {
	reason, card := jonziOpenBlockReason(msg, sources)
	items := interp.Flatten()
	if len(interp.Instructions) > 0 {
		items = append(items, interp)
	}
	for _, ins := range items {
		if ins.Action != ActionOpen && ins.Action != ActionAdd {
			continue
		}
		why := reason
		if why == "" && ins.ActionEvidence != nil {
			src := findSourceSegment(sources, ins.ActionEvidence.SourceID)
			if strings.HasPrefix(ins.ActionEvidence.SourceID, "embed:") || (src != nil && src.Role != "current") {
				why = "action evidence cites an embedded or referenced card"
			}
		}
		if why == "" && ins.Symbol != "" {
			got, gotErr := ResolveInstrument(ins.Symbol)
			want, wantErr := ResolveInstrument(card.Symbol)
			if (gotErr == nil) != (wantErr == nil) || got != want {
				why = "model symbol conflicts with the card header"
			}
		}
		if why == "" && ins.Direction != "" && ins.Direction != card.Direction {
			why = "model direction conflicts with the card header"
		}
		if why == "" {
			why = jonziModelOpenMismatch(ins, card)
		}
		if why == "" {
			continue
		}
		proposed := ins.Action
		ins.Warnings = appendUniqueWarning(ins.Warnings, fmt.Sprintf("jonzi_v1: model %s downgraded to IGNORE (%s)", proposed, why))
		ins.Reasoning = strings.TrimSpace(fmt.Sprintf("jonzi_v1 opens only from a revision-0 native card; %s blocked: %s. %s", proposed, why, ins.Reasoning))
		ins.Classification, ins.Action = ClassificationIgnore, ActionIgnore
	}
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
	jonzi := rules.Profile == "jonzi_v1" && rules.SourceMode != "chroma"
	if jonzi {
		// Native card edits, terminal cards and commentary carrying a card are
		// fully determined by the card itself; generic heuristics applied to the
		// author's free-text reasoning cannot change that outcome.
		if det := interpretJonziSource(msg, sources); det != nil {
			guardJonziOpenAuthority(det, msg, sources)
			return det
		}
	}
	text := currentPresetText(sources)
	gate := currentPresetGateText(sources)
	for _, ins := range interp.Flatten() {
		scope := evidenceScopeForSymbol(text, ins.Symbol)
		gateScope := evidenceScopeForSymbol(gate, ins.Symbol)
		if preset.ID == "tyler_v1" && !sourceOpen.MatchString(sourceCancel.ReplaceAllString(scope, "")) {
			if reason := unresolvedManagement(scope); reason != "" {
				ins.Classification, ins.Reasoning = ClassificationAmbiguous, reason
				continue
			}
		}
		if preset.StrictManagement && (ins.Action == ActionReduce || ins.Action == ActionClose) && discretionaryExit.MatchString(gateScope) {
			ins.Classification = ClassificationAmbiguous
			ins.Reasoning = "preset requires explicit management, not a discretionary suggestion"
		}
		if preset.ID != "default" && (ins.Action == ActionReduce || ins.Action == ActionClose) && recapOnly.MatchString(gateScope) && !explicitManagement.MatchString(gateScope) && !hasFullCloseIntent(gateScope) && !hasCurrentManagementWording(gateScope) && !jonziTerminal.MatchString(gateScope) {
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
	if jonzi {
		guardJonziOpenAuthority(interp, msg, sources)
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
