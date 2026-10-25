package copytrader

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"nofx/discord"
	"nofx/store"
)

type SourceSegment struct {
	ID        string `json:"id"`
	Role      string `json:"role"` // current, reference, unknown
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
	Author    string `json:"author,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Image     bool   `json:"image,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// GateScope narrows the condition/negation gates for a jonzi native card to
	// its English status line(s) and Trade details block. The author's free-text
	// reasoning and the translated position-sizing text are not instructions.
	GateScope string `json:"-"`
}

type SourceMedia struct{ URL, SourceID, Role string }

var sourceSymbol = regexp.MustCompile(`(?i)[#＃]([\p{L}\p{N}]+(?:/USDT)?)`)
var sourceOpen = regexp.MustCompile(`(?i)(進場|进场|入場|入场|開倉|开仓|entry|entries|buy now|sell now|open long|open short|做多|做空)`)
var sourceManagement = regexp.MustCompile(`(?i)(止盈|止損|止损|減倉|减仓|平倉|平仓|全平|成本|保本|[無无][風风][險险]持[倉仓]|[這这][單单]就不拿|可以先跑|提前.*TP|close|reduce|exit|stops?|\bSL\b|take profit|breakeven|break even|trim)`)
var sourceDirection = regexp.MustCompile(`(?i)(做多|做空|多單|空單|long|short|buy|sell)`)
var sourceNegation = regexp.MustCompile(`(?i)(不要|暫不|暂不|勿|別|别|尚未|未到|如果|若是|not yet|do not|don't)`)
var sourceActionNegation = regexp.MustCompile(`(?i)(不要|暫不|暂不|勿|別|别|尚未|未到|not yet|do not|don't)`)
var sourceTPLevel = regexp.MustCompile(`(?i)TP\s*([1-9][0-9]*)`)
var sourceAddCondition = regexp.MustCompile(`(有[補补][倉仓]|[補补][倉仓][後后]|after (?:an? )?add|if (?:you )?added)`)
var sourceCancel = regexp.MustCompile(`(?i)(撤(?:掉(?:[掛挂])?|[掛挂])?[單单]|取消[掛挂][單单]|取消[補补][倉仓]|取消(?:[這这](?:[筆笔])?)?(?:[訂订][單单]|(?:入[場场]|[進进][場场]|限[價价])(?:[訂订])?[單单])|\bcancel(?:\s+(?:the\s+)?(?:bid|order|entry|position))?\b)`)
var sourceAdd = regexp.MustCompile(`(?i)([補补][倉仓]|加[倉仓]|\badd\b)`)

func hasCurrentManagementWording(text string) bool {
	return sourceManagement.MatchString(sourceTPRecap.ReplaceAllString(text, ""))
}

func hasLegacyFullCloseIntent(text string) bool {
	return (sourceFullProfitExit.MatchString(text) && !retainedPosition.MatchString(text)) || sourceCloseIntent.MatchString(text)
}

// Only explicit entry verbs can override a terminal status card. In
// particular, a bare "Entry:" field or a historical long/short label is not
// an instruction to open a new trade.
var sourceExplicitOpen = regexp.MustCompile(`(?i)(進場\s*(?:[:：]|市價|市价|限價|限价)|进场\s*(?:[:：]|市價|市价|限價|限价)|\bopen(?:\s+(?:long|short))?\b|buy\s+now|sell\s+now|做多|做空|開多|开多|開空|开空|開倉(?:\s+|[:：]|$)|开仓(?:\s+|[:：]|$)|\benter(?:ing)?\b)`)
var sourceReduce = regexp.MustCompile(`(?i)(止盈|減倉|减仓|減半|减半|減掉|减掉|(?:平倉|平仓)\s*[0-9]+(?:\.[0-9]+)?\s*[%％]|可以先跑|提前.*TP|\breduce\b|\btrim\b|take profit|(?:獲利|获利).*(?:了結|了结|出場|出场|平倉|平仓|take|book|realiz))`)
var sourceConditional = regexp.MustCompile(`(?i)(如果|若是|只有|only if|\bif\b)`)

// Common compounds that merely contain a negation character ("日线级别",
// "区别", "特别"). Go's RE2 has no look-behind, so these are neutralized before
// the negation/condition cues are matched. The first character is kept and the
// second replaced by a space so no new adjacency can form a different cue.
var cueCompounds = strings.NewReplacer(
	"级别", "级 ", "級別", "級 ", "区别", "区 ", "區別", "區 ", "分别", "分 ", "分別", "分 ",
	"特别", "特 ", "特別", "特 ", "类别", "类 ", "類別", "類 ", "个别", "个 ", "個別", "個 ",
	"识别", "识 ", "識別", "識 ", "告别", "告 ", "差别", "差 ", "性别", "性 ",
)

func matchesSourceCue(cue *regexp.Regexp, text string) bool {
	return cue.MatchString(cueCompounds.Replace(text))
}

var sourceQuoteStart = regexp.MustCompile(`(?im)^[ \t]*(?:引用信息|引用訊息|引用消息|quoted message|quote)[ \t]*[:：][ \t]*`)
var sourceReplyStart = regexp.MustCompile(`(?im)^[ \t]*(?:💬[ \t]*)?(?:回复|回覆|reply)[ \t]*[:：][ \t]*`)
var sourceBreakeven = regexp.MustCompile(`(?i)(成本.*[損损]|[損损].*成本|保本|上成本|移[至到]成本|拉[到至]成本)`)

func BuildSourceSegments(msg *store.DiscordMessage, profile string) []SourceSegment {
	segments := buildBodySourceSegments(msg.Content, profile)
	if profile == "jonzi_v1" {
		for i := range segments {
			if segments[i].ID == "body" {
				if card := parseJonziCard(segments[i].Text); card.IsCard {
					segments[i].GateScope = card.GateText
				}
			}
		}
	}
	for i, embed := range discord.ParseStoredEmbeds(msg.EmbedsJSON) {
		segment := SourceSegment{ID: fmt.Sprintf("embed:%d", i), Role: "current", Text: discord.FlattenEmbeds([]discord.Embed{embed}), Timestamp: embed.Timestamp}
		if embed.Author != nil {
			segment.Author = embed.Author.Name
		}
		// Native cards often predate their message by seconds. Compare against
		// original post time (never EditedAt), and require corroborating context.
		ts, err := time.Parse(time.RFC3339, embed.Timestamp)
		old := err == nil && msg.MessageTimestamp.Sub(ts) > time.Minute
		if msg.ReplyToMessageID != "" && old {
			segment.Role, segment.Reason = "reference", "older card in a reply"
		} else if old && (splitQuotedSources(profile) || embed.Author != nil) && (sourceProfit.MatchString(msg.Content) || sourceManagement.MatchString(msg.Content)) && !sourceOpen.MatchString(msg.Content) {
			segment.Role, segment.Reason = "reference", "attributed older card accompanying a current message"
		} else if err == nil && splitQuotedSources(profile) && ts.Before(msg.MessageTimestamp) && parseJonziCard(segment.Text).IsCard {
			// A jonzi card is native only as message content; an embedded copy
			// that predates the message is a forward/quote, whatever the
			// accompanying commentary says (e.g. "not the place for longs").
			segment.Role, segment.Reason = "reference", "earlier jonzi card embedded in a later message"
		}
		segments = append(segments, segment)
	}
	for _, media := range MediaSources(msg, segments) {
		segments = append(segments, SourceSegment{ID: media.SourceID + ":image", Role: media.Role, Image: true, Text: "Image supplied separately; transcribe current action words for evidence."})
	}
	return segments
}

// buildBodySourceSegments keeps the legacy single-body representation for
// generic interpretation profiles. Author presets may explicitly quote an old
// card in plain text; split that quote from the current reply so quoted entry
// parameters cannot authorize a new OPEN/ADD action.
func buildBodySourceSegments(content, profile string) []SourceSegment {
	if !splitQuotedSources(profile) {
		return []SourceSegment{{ID: "body", Role: "current", Text: content}}
	}
	quote := sourceQuoteStart.FindStringIndex(content)
	if quote == nil {
		return []SourceSegment{{ID: "body", Role: "current", Text: content}}
	}

	segments := make([]SourceSegment, 0, 3)
	appendSegment := func(id, role, text, reason string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		segments = append(segments, SourceSegment{ID: id, Role: role, Text: text, Reason: reason})
	}
	appendSegment("body:current:0", "current", content[:quote[0]], "text before quoted card")

	rest := content[quote[1]:]
	replyRel := sourceReplyStart.FindStringIndex(rest)
	if replyRel == nil {
		appendSegment("body:reference:0", "reference", content[quote[0]:], "explicit quoted card")
		return segments
	}
	replyStart := quote[1] + replyRel[0]
	appendSegment("body:reference:0", "reference", content[quote[0]:replyStart], "explicit quoted card")
	appendSegment("body:current:1", "current", content[replyStart:], "current reply after quoted card")
	return segments
}

// MatchHistoricalSegments uses exact normalized card text, not guessed price
// similarity, to label quotes even when older stored records lost the author.
func MatchHistoricalSegments(segments []SourceSegment, msg *store.DiscordMessage, history []*store.DiscordMessage) {
	for i := range segments {
		if !strings.HasPrefix(segments[i].ID, "embed:") {
			continue
		}
		for _, old := range history {
			if old.MessageID == msg.MessageID || !old.MessageTimestamp.Before(msg.MessageTimestamp) {
				continue
			}
			for _, embed := range discord.ParseStoredEmbeds(msg.EmbedsJSON) {
				if normalizedSignalText(embed.Description) != "" && normalizedSignalText(embed.Description) == normalizedSignalText(old.Content) && strings.Contains(segments[i].Text, embed.Description) {
					segments[i].Role, segments[i].Reason, segments[i].MessageID = "reference", "exact card match to historical message "+old.MessageID, old.MessageID
				}
			}
		}
	}
}

var sourceMention = regexp.MustCompile(`<@[^>]*>`)

func normalizedSignalText(s string) string {
	return strings.Join(strings.Fields(sourceMention.ReplaceAllString(s, "")), "")
}

// findEvidenceSpan matches the model's evidence while tolerating only
// whitespace differences. Semantic paraphrases are handled separately by
// normalizeActionEvidence after the current segment's action intent is
// verified; this helper remains strict so it cannot cross source boundaries.
func findEvidenceSpan(source, evidence string) (int, int, bool) {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return 0, 0, false
	}
	if index := strings.Index(source, evidence); index >= 0 {
		return index, index + len(evidence), true
	}
	fields := strings.Fields(evidence)
	if len(fields) == 0 {
		return 0, 0, false
	}
	pattern := regexp.QuoteMeta(fields[0])
	for _, field := range fields[1:] {
		pattern += `\s+` + regexp.QuoteMeta(field)
	}
	loc := regexp.MustCompile(`(?s)` + pattern).FindStringIndex(source)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

func MediaSources(msg *store.DiscordMessage, segments []SourceSegment) []SourceMedia {
	var media []SourceMedia
	for i, a := range discord.ParseStoredAttachments(msg.AttachmentsJSON) {
		if a.IsImage() && a.URL != "" {
			media = append(media, SourceMedia{a.URL, fmt.Sprintf("attachment:%d", i), "current"})
		}
	}
	for i, e := range discord.ParseStoredEmbeds(msg.EmbedsJSON) {
		if e.Image == nil || e.Image.URL == "" {
			continue
		}
		role := "current"
		id := fmt.Sprintf("embed:%d", i)
		for _, s := range segments {
			if s.ID == id {
				role = s.Role
			}
		}
		media = append(media, SourceMedia{e.Image.URL, id, role})
	}
	return media
}

// InterpretKnownSource fixes only opted-in TYLER management phrases. Entry
// parameters remain the interpreter's job; all resulting actions still pass
// the normal correlation, TTL and risk gates.
func interpretKnownSourceLegacy(msg *store.DiscordMessage, segments []SourceSegment, profile string) *SourceInterpretation {
	if profile != "tyler_v1" {
		return interpretPresetSource(msg, segments, profile)
	}
	body, actionSourceID, actionEvidence := tylerCurrentSource(segments, msg.Content)
	body = strings.TrimSpace(body)
	// "Cancel entry order" contains an entry word but is not an open.
	// Leave mixed cancel/new-entry instructions to the interpreter as before.
	if body == "" || sourceOpen.MatchString(sourceCancel.ReplaceAllString(body, "")) {
		return nil
	}
	if matchesSourceCue(sourceNegation, body) && !strings.Contains(body, "如果還繼續持有") {
		return nil
	}
	if matches := sourceSymbol.FindAllStringSubmatch(body, -1); len(matches) > 1 {
		unique := map[string]bool{}
		for _, m := range matches {
			unique[m[1]] = true
		}
		if len(unique) > 1 {
			return nil
		}
	}
	sym := ""
	if m := sourceSymbol.FindStringSubmatch(body); len(m) > 1 {
		sym = m[1]
	}
	if sym == "" {
		candidates := map[string]bool{}
		for _, s := range segments {
			if s.Role == "reference" {
				if m := sourceSymbol.FindStringSubmatch(s.Text); len(m) > 1 {
					candidates[m[1]] = true
				}
			}
		}
		if len(candidates) == 1 {
			for v := range candidates {
				sym = v
			}
		}
	}
	if reason := unresolvedManagement(body); reason != "" {
		return &SourceInterpretation{Classification: ClassificationAmbiguous, Action: ActionIgnore, Symbol: sym,
			ActionEvidence: &ActionEvidence{SourceID: actionSourceID, Text: actionEvidence}, Reasoning: reason}
	}
	var actions []*SourceInterpretation
	makeAction := func(a Action) *SourceInterpretation {
		return &SourceInterpretation{Classification: ClassificationSignal, Action: a, Symbol: sym, ActionEvidence: &ActionEvidence{SourceID: actionSourceID, Text: actionEvidence}, RequiresAddFill: sourceAddCondition.MatchString(body), Reasoning: "TYLER explicit management policy"}
	}
	full := hasLegacyFullCloseIntent(body) || strings.Contains(body, "提前平倉") || strings.Contains(body, "提前平仓") || strings.Contains(body, "提前止損離場") || strings.Contains(body, "提前止损离场") || strings.Contains(body, "這單就不拿") || strings.Contains(body, "这单就不拿")
	reduce := strings.Contains(body, "減倉") || strings.Contains(body, "减仓") || strings.Contains(body, "可以先跑") || earlyTPWording.MatchString(body) || partialCloseRatio.MatchString(body)
	// A fraction after "close / 平仓" is a partial exit, never a full close.
	if full && !sourceClosedStatus.MatchString(body) {
		_, explicit, ratioErr := explicitReductionRatio(body)
		if explicit || ratioErr != nil {
			reduce = true
		}
	}
	be := sourceBreakeven.MatchString(body) || strings.Contains(body, "無風險持倉") || strings.Contains(body, "无风险持仓")
	moveTP := (strings.Contains(body, "止損") || strings.Contains(body, "止损")) && sourceTPLevel.MatchString(body) && (strings.Contains(body, "提升") || strings.Contains(body, "移至") || strings.Contains(body, "移到"))
	if full && !reduce {
		a := makeAction(ActionClose)
		a.CloseMode = CloseModeFull
		actions = append(actions, a)
	} else {
		if reduce {
			a := makeAction(ActionReduce)
			v := 50.0 // Compatibility for direct callers; effective default is applied in ApplyMessageRules.
			if explicit, found, err := explicitReductionRatio(body); err != nil {
				a.Classification = ClassificationAmbiguous
				a.Reasoning = err.Error()
			} else if found {
				v = explicit
			}
			a.CloseRatio = &v
			a.CloseMode = CloseModePartial
			if strings.Contains(body, "倉位重") || strings.Contains(body, "仓位重") {
				a.Classification = ClassificationNeedsContext
				a.Reasoning = "position-heavy condition is not objectively established"
			}
			actions = append(actions, a)
		}
		if sourceCancel.MatchString(body) && !full {
			actions = append(actions, makeAction(ActionCancel))
		}
		if be || moveTP {
			a := makeAction(ActionUpdateSL)
			p := PriceSpec{Type: PriceEntry}
			if moveTP {
				p.Type = PriceTPLevel
				fmt.Sscanf(strings.ToUpper(strings.ReplaceAll(sourceTPLevel.FindString(body), " ", "")), "TP%d", &p.Level)
			}
			a.StopLossLevels = []SLLevel{{Price: p}}
			if (strings.Contains(body, "後") || strings.Contains(body, "后")) && sourceTPLevel.MatchString(body) && !moveTP {
				var level int
				fmt.Sscanf(strings.ToUpper(strings.ReplaceAll(sourceTPLevel.FindString(body), " ", "")), "TP%d", &level)
				a.ConditionalRules = []ConditionalRule{{Condition: ConditionTPFilled, ConditionLevel: level, Action: ActionUpdateSL, Price: PriceSpec{Type: PriceEntry}}}
			}
			actions = append(actions, a)
		}
	}
	if len(actions) == 0 {
		if sourceProfit.MatchString(body) && !hasCurrentManagementWording(body) {
			return &SourceInterpretation{Classification: ClassificationIgnore, Action: ActionIgnore, Symbol: sym, Reasoning: "TYLER performance recap has no current management instruction"}
		}
		return nil
	}
	if len(actions) == 1 {
		return actions[0]
	}
	return &SourceInterpretation{Classification: ClassificationSignal, Action: actions[0].Action, Symbol: sym, Instructions: actions}
}

// tylerCurrentSource returns the current text used by the deterministic TYLER
// policy and the exact source segment that contains its action wording.
func tylerCurrentSource(segments []SourceSegment, fallback string) (string, string, string) {
	current := make([]SourceSegment, 0, len(segments))
	for _, segment := range segments {
		if segment.Role == "current" && !segment.Image && strings.TrimSpace(segment.Text) != "" {
			current = append(current, segment)
		}
	}
	if len(current) == 0 {
		if len(segments) == 0 {
			return strings.TrimSpace(fallback), "body", strings.TrimSpace(fallback)
		}
		return "", "", ""
	}

	action := current[0]
	for _, segment := range current {
		if sourceManagement.MatchString(segment.Text) || sourceCancel.MatchString(segment.Text) || sourceProfit.MatchString(segment.Text) {
			action = segment
			break
		}
	}
	parts := make([]string, 0, len(current))
	for _, segment := range current {
		parts = append(parts, segment.Text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), action.ID, strings.TrimSpace(action.Text)
}

func ApplySourcePolicy(interp *SourceInterpretation, msg *store.DiscordMessage, segments []SourceSegment, profile string) *SourceInterpretation {
	return applySourcePolicyVersion(interp, msg, segments, profile, MessageRulesVersion)
}
func applySourcePolicyVersion(interp *SourceInterpretation, msg *store.DiscordMessage, segments []SourceSegment, profile string, version int) *SourceInterpretation {
	if known := interpretKnownVersion(msg, segments, profile, version); known != nil {
		return known
	}
	for _, ins := range interp.Flatten() {
		normalizeActionEvidence(ins, segments)
		if ins.ActionEvidence == nil {
			continue
		}
		segment := findSourceSegment(segments, ins.ActionEvidence.SourceID)
		if segment == nil || segment.Role != "current" || segment.Image {
			continue
		}
		conditionText := actionEvidenceScope(ins, segment.Text)
		if sourceAddCondition.MatchString(conditionText) {
			ins.RequiresAddFill = true
		}
		if strings.Contains(conditionText, "倉位重") || strings.Contains(conditionText, "仓位重") {
			ins.Classification = ClassificationNeedsContext
		}
	}
	return interp
}

// Normalize only legacy body IDs, absent evidence, or paraphrases within the
// named current segment. Explicit references, images and unknown IDs are never
// silently rebound to a different source. Ambiguous candidate segments remain
// unnormalized and are rejected by the source gate.
func normalizeActionEvidence(ins *SourceInterpretation, segments []SourceSegment) {
	if ins == nil || !ins.IsActionable() {
		return
	}
	var candidate *SourceSegment
	if ins.ActionEvidence != nil && ins.ActionEvidence.SourceID != "" {
		candidate = findSourceSegment(segments, ins.ActionEvidence.SourceID)
		if candidate != nil {
			if candidate.Role != "current" || candidate.Image {
				return
			}
			if _, _, ok := findEvidenceSpan(candidate.Text, ins.ActionEvidence.Text); ok {
				return // exact evidence still passes symbol, condition and intent gates below
			}
		} else if ins.ActionEvidence.SourceID != "body" {
			return
		}
	}
	if candidate == nil {
		for i := range segments {
			s := &segments[i]
			if s.Role != "current" || s.Image {
				continue
			}
			// A legacy body alias can only address current body fragments.
			if ins.ActionEvidence != nil && ins.ActionEvidence.SourceID == "body" && !strings.HasPrefix(s.ID, "body:") {
				continue
			}
			scope := evidenceScopeForSymbol(s.Text, ins.Symbol)
			if !segmentCarriesAction(ins, scope) {
				continue
			}
			if candidate != nil {
				return
			}
			candidate = s
		}
	}
	if candidate == nil {
		return
	}
	text := evidenceScopeForSymbol(candidate.Text, ins.Symbol)
	if !segmentCarriesAction(ins, text) {
		return
	}
	ins.ActionEvidence = &ActionEvidence{SourceID: candidate.ID, Text: text}
	ins.Warnings = appendUniqueWarning(ins.Warnings, "action evidence normalized to current source segment")
}

func findSourceSegment(segments []SourceSegment, id string) *SourceSegment {
	for i := range segments {
		if segments[i].ID == id {
			return &segments[i]
		}
	}
	return nil
}

// A terminal status card may repeat the original direction or entry fields.
// Only an explicit entry verb that appears after the latest terminal status
// can override that status; earlier direction labels remain historical facts.
func terminalStatusBlocksOpen(text string) bool {
	statuses := sourceClosedStatus.FindAllStringIndex(text, -1)
	if len(statuses) == 0 {
		return false
	}
	opens := sourceExplicitOpen.FindAllStringIndex(text, -1)
	if len(opens) == 0 {
		return true
	}
	lastStatusEnd := statuses[len(statuses)-1][1]
	lastOpenEnd := opens[len(opens)-1][1]
	return lastOpenEnd <= lastStatusEnd
}

func segmentCarriesAction(ins *SourceInterpretation, text string) bool {
	if ins == nil || strings.TrimSpace(text) == "" {
		return false
	}
	if ins.NotificationKind != "" {
		sym, dir, kind := notificationIdentity(text)
		canonical, _ := ResolveInstrument(ins.Symbol)
		if sym == canonical && dir == ins.Direction {
			if kind == "added" && ins.NotificationKind == kind && ins.Action == ActionOpen {
				return true
			}
			if kind == "closed" && ins.NotificationKind == kind && ins.Action == ActionClose {
				return true
			}
		}
	}
	if ins.ReduceTPLevel != 0 {
		m := replaceTPV4.FindStringSubmatch(text)
		if len(m) != 2 || m[1] != fmt.Sprint(ins.ReduceTPLevel) || ins.Action != ActionReduce || ins.CloseRatio != nil {
			return false
		}
		if ins.CancelPendingEntry && !cancelAddV4.MatchString(text) {
			return false
		}
	} else if ins.CancelPendingEntry {
		return false
	}
	switch ins.Action {
	case ActionOpen, ActionAdd:
		if terminalStatusBlocksOpen(text) {
			return false
		}
		return !sourceCancel.MatchString(text) && (sourceOpen.MatchString(text) || (ins.Action == ActionAdd && sourceAdd.MatchString(text)))
	case ActionCancel:
		return (sourceCancel.MatchString(text) || cancelAddV4.MatchString(text)) && !hasFullCloseIntent(text)
	case ActionClose:
		if profitExitV4.MatchString(text) {
			ratio, explicit, err := explicitReductionRatio(text)
			if err != nil || retainedPosition.MatchString(text) || (explicit && ratio < 100) {
				return false
			}
		}
		return hasFullCloseIntent(text) || bareCloseWording.MatchString(text)
	case ActionReduce:
		if profitExitV4.MatchString(text) && !retainedPosition.MatchString(text) {
			_, explicit, err := explicitReductionRatio(text)
			if !explicit || err != nil {
				return false
			}
		}
		if sourceReduce.MatchString(text) {
			return true
		}
		_, explicit, err := explicitReductionRatio(text)
		return hasFullCloseIntent(text) && explicit && err == nil
	default:
		return sourceManagement.MatchString(text) && !sourceCancel.MatchString(text)
	}
}

// Scope a source to the target symbol before testing intent or conditions.
// Multiple blocks for the same symbol need exact evidence to disambiguate;
// a paraphrase must never select the first matching block by accident.
func evidenceScopeForSymbol(body, rawSymbol string) string {
	matches := sourceSymbol.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(body)
	}
	want, _ := ResolveInstrument(rawSymbol)
	unique := map[string]bool{}
	for _, m := range matches {
		got, _ := ResolveInstrument(strings.TrimLeft(body[m[0]:m[1]], "#＃"))
		if got != "" {
			unique[got] = true
		}
	}
	if len(unique) == 1 && (want == "" || unique[want]) {
		return strings.TrimSpace(body)
	}
	if want == "" || !unique[want] {
		return ""
	}
	var block string
	for i, m := range matches {
		got, _ := ResolveInstrument(strings.TrimLeft(body[m[0]:m[1]], "#＃"))
		if got != want {
			continue
		}
		if block != "" {
			return ""
		}
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		start := m[0]
		if i == 0 {
			start = 0 // retain an outer qualifier before the first ticker
		}
		block = strings.TrimSpace(body[start:end])
	}
	return block
}

// For literal evidence preserve its surrounding block and reject a symbol
// mismatch; for a paraphrase use the unique target block from actual source.
func actionEvidenceScope(ins *SourceInterpretation, body string) string {
	if ins.ActionEvidence != nil {
		if _, _, ok := findEvidenceSpan(body, ins.ActionEvidence.Text); ok {
			scope := evidenceScope(body, ins.ActionEvidence.Text)
			if scoped := evidenceScopeForSymbol(scope, ins.Symbol); scoped != "" {
				return scoped
			}
			return ""
		}
	}
	return evidenceScopeForSymbol(body, ins.Symbol)
}

func appendUniqueWarning(warnings []string, warning string) []string {
	for _, existing := range warnings {
		if existing == warning {
			return warnings
		}
	}
	return append(warnings, warning)
}

// Keep the qualifying clause around quoted evidence. In a multi-symbol body,
// restrict it to that symbol's block so another trade's condition does not leak.
func evidenceScope(body, evidence string) string {
	evidenceStart, evidenceEnd, ok := findEvidenceSpan(body, evidence)
	if !ok {
		return body
	}
	matches := sourceSymbol.FindAllStringIndex(body, -1)
	scopeStart, scopeEnd := 0, len(body)
	for _, m := range matches {
		if m[0] <= evidenceStart {
			scopeStart = m[0]
		} else if m[0] >= evidenceEnd {
			scopeEnd = m[0]
			break
		}
	}
	if len(matches) <= 1 {
		return body
	}
	return body[scopeStart:scopeEnd]
}

func ValidateActionEvidence(ins *SourceInterpretation, segments []SourceSegment) (SkipReason, error) {
	skip, _, err := ValidateActionEvidenceDetailed(ins, segments)
	return skip, err
}

// ValidateActionEvidenceDetailed preserves the existing skip semantics while
// returning a stable diagnostic for the execution log/UI.
func ValidateActionEvidenceDetailed(ins *SourceInterpretation, segments []SourceSegment) (SkipReason, string, error) {
	if !ins.IsActionable() {
		return SkipNone, "", nil
	}
	if ins.ActionEvidence == nil {
		return SkipSourceEvidence, "missing current action evidence", nil
	}
	s := findSourceSegment(segments, ins.ActionEvidence.SourceID)
	if s == nil {
		return SkipSourceEvidence, "action evidence source was not found", nil
	}
	if s.Role != "current" {
		return SkipSourceEvidence, "action evidence points to a reference source", nil
	}
	if s.Image && !ins.EvidenceVerified {
		return SkipSourceEvidence, "current image evidence was not verified", nil
	}
	if strings.TrimSpace(ins.ActionEvidence.Text) == "" {
		return SkipSourceEvidence, "current action evidence is empty", nil
	}
	scope := ins.ActionEvidence.Text
	if !s.Image {
		scope = actionEvidenceScope(ins, s.Text)
		if scope == "" {
			return SkipSourceEvidence, "current evidence symbol or block does not match the parsed symbol", nil
		}
		if s.GateScope != "" {
			scope = s.GateScope
		}
	}
	opening := ins.Action == ActionOpen || ins.Action == ActionAdd
	if opening && terminalStatusBlocksOpen(scope) {
		return SkipSourceEvidence, "current status indicates a closed trade; no explicit new entry instruction", nil
	}
	// A current terminal card also takes precedence over an attached entry
	// chart. Images do not override an explicit current status for this symbol.
	if opening && s.Image {
		for _, segment := range segments {
			if segment.Role == "current" && !segment.Image {
				text := evidenceScopeForSymbol(segment.Text, ins.Symbol)
				if segment.GateScope != "" && text != "" {
					text = segment.GateScope
				}
				if terminalStatusBlocksOpen(text) {
					return SkipSourceEvidence, "current status indicates a closed trade; no explicit new entry instruction", nil
				}
			}
		}
	}
	if len(ins.EligibilityConditions) > 0 {
		return SkipNeedsContext, "eligibility condition is not verified", nil
	}
	gateScope := managementScopeV4(scope, ins)
	if sourceAddCondition.MatchString(gateScope) && !ins.RequiresAddFill {
		return SkipNeedsContext, "requires a confirmed add fill for this trade", nil
	}
	if matchesSourceCue(sourceConditional, gateScope) && !ins.RequiresAddFill && len(ins.ConditionalRules) == 0 && ins.RequiresTPFill == 0 {
		return SkipNeedsContext, "conditional eligibility is not verified", nil
	}
	if matchesSourceCue(sourceActionNegation, scope) {
		return SkipSourceEvidence, "current evidence negates the parsed action", nil
	}
	if !segmentCarriesAction(ins, scope) || (opening && matchesSourceCue(sourceNegation, scope)) {
		if opening {
			return SkipSourceEvidence, "current evidence lacks an unambiguous open/add instruction", nil
		}
		return SkipSourceEvidence, "current evidence lacks a management instruction", nil
	}
	return SkipNone, "", nil
}
