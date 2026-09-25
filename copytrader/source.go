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
}

type SourceMedia struct{ URL, SourceID, Role string }

var sourceSymbol = regexp.MustCompile(`(?i)[#＃]([\p{L}\p{N}]+(?:/USDT)?)`)
var sourceOpen = regexp.MustCompile(`(?i)(進場|进场|入場|入场|entry|entries|buy now|sell now|open long|open short|做多|做空)`)
var sourceManagement = regexp.MustCompile(`(?i)(止盈|止損|止损|減倉|减仓|平倉|平仓|全平|成本|保本|[無无][風风][險险]持[倉仓]|[這这][單单]就不拿|可以先跑|提前.*TP|close|reduce|exit|stops?|\bSL\b|take profit|breakeven|break even|trim)`)
var sourceDirection = regexp.MustCompile(`(?i)(做多|做空|多單|空單|long|short|buy|sell)`)
var sourceNegation = regexp.MustCompile(`(?i)(不要|暫不|暂不|勿|別|别|尚未|未到|如果|若是|not yet|do not|don't)`)
var sourceProfit = regexp.MustCompile(`(?i)(起飛|起飞|獲利|获利|翻倍|倍|TP\s*\d.*(?:到|完成|✅|hit|booked)|^\s*[#＃]\S+\s+TP\s*\d\s*(?:<.*>)?\s*$)`)
var sourceTPLevel = regexp.MustCompile(`(?i)TP\s*([1-9][0-9]*)`)
var sourceAddCondition = regexp.MustCompile(`(有[補补][倉仓]|[補补][倉仓][後后]|after (?:an? )?add|if (?:you )?added)`)
var sourceCancel = regexp.MustCompile(`(?i)(撤[單单]|取消[掛挂][單单]|cancel)`)
var sourceAdd = regexp.MustCompile(`(?i)([補补][倉仓]|加[倉仓]|\badd\b)`)

func BuildSourceSegments(msg *store.DiscordMessage, profile string) []SourceSegment {
	segments := []SourceSegment{{ID: "body", Role: "current", Text: msg.Content}}
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
		} else if old && (profile == "tyler_v1" || embed.Author != nil) && (sourceProfit.MatchString(msg.Content) || sourceManagement.MatchString(msg.Content)) && !sourceOpen.MatchString(msg.Content) {
			segment.Role, segment.Reason = "reference", "attributed older card accompanying a current message"
		}
		segments = append(segments, segment)
	}
	for _, media := range MediaSources(msg, segments) {
		segments = append(segments, SourceSegment{ID: media.SourceID + ":image", Role: media.Role, Image: true, Text: "Image supplied separately; transcribe current action words for evidence."})
	}
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
func InterpretKnownSource(msg *store.DiscordMessage, segments []SourceSegment, profile string) *SourceInterpretation {
	if profile != "tyler_v1" {
		return nil
	}
	body := strings.TrimSpace(msg.Content)
	if body == "" || sourceOpen.MatchString(body) {
		return nil
	}
	if sourceNegation.MatchString(body) && !strings.Contains(body, "如果還繼續持有") {
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
	var actions []*SourceInterpretation
	makeAction := func(a Action) *SourceInterpretation {
		return &SourceInterpretation{Classification: ClassificationSignal, Action: a, Symbol: sym, ActionEvidence: &ActionEvidence{SourceID: "body", Text: body}, RequiresAddFill: sourceAddCondition.MatchString(body), Reasoning: "TYLER explicit management policy"}
	}
	full := strings.Contains(body, "全平") || strings.Contains(body, "提前平倉") || strings.Contains(body, "提前平仓") || strings.Contains(body, "提前止損離場") || strings.Contains(body, "提前止损离场") || strings.Contains(body, "這單就不拿") || strings.Contains(body, "这单就不拿")
	reduce := strings.Contains(body, "減倉") || strings.Contains(body, "减仓") || strings.Contains(body, "可以先跑") || regexp.MustCompile(`(?i)(提前|可做|可以作|自行.*作)\s*TP\s*\d`).MatchString(body)
	be := regexp.MustCompile(`(成本.*[損损]|[損损].*成本|保本)`).MatchString(body) || strings.Contains(body, "無風險持倉") || strings.Contains(body, "无风险持仓")
	moveTP := (strings.Contains(body, "止損") || strings.Contains(body, "止损")) && sourceTPLevel.MatchString(body) && (strings.Contains(body, "提升") || strings.Contains(body, "移至") || strings.Contains(body, "移到"))
	if full {
		a := makeAction(ActionClose)
		a.CloseMode = CloseModeFull
		actions = append(actions, a)
	} else {
		if reduce {
			a := makeAction(ActionReduce)
			v := 50.0
			if m := regexp.MustCompile(`(?:減倉|减仓|止盈|平倉|平仓)\s*([0-9]+(?:\.[0-9]+)?)\s*[%％]`).FindStringSubmatch(body); len(m) > 1 {
				fmt.Sscanf(m[1], "%f", &v)
			}
			a.CloseRatio = &v
			a.CloseMode = CloseModePartial
			if strings.Contains(body, "倉位重") || strings.Contains(body, "仓位重") {
				a.Classification = ClassificationNeedsContext
				a.Reasoning = "position-heavy condition is not objectively established"
			}
			actions = append(actions, a)
		}
		if strings.Contains(body, "撤單") || strings.Contains(body, "撤单") || strings.Contains(body, "取消掛單") || strings.Contains(body, "取消挂单") {
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
		if sourceProfit.MatchString(body) {
			return &SourceInterpretation{Classification: ClassificationIgnore, Action: ActionIgnore, Symbol: sym, Reasoning: "TYLER performance recap has no current management instruction"}
		}
		return nil
	}
	if len(actions) == 1 {
		return actions[0]
	}
	return &SourceInterpretation{Classification: ClassificationSignal, Action: actions[0].Action, Symbol: sym, Instructions: actions}
}

func ApplySourcePolicy(interp *SourceInterpretation, msg *store.DiscordMessage, segments []SourceSegment, profile string) *SourceInterpretation {
	if known := InterpretKnownSource(msg, segments, profile); known != nil {
		return known
	}
	for _, ins := range interp.Flatten() {
		conditionText := msg.Content
		if ins.ActionEvidence != nil {
			for _, s := range segments {
				if s.ID == ins.ActionEvidence.SourceID && s.Role == "current" && !s.Image {
					conditionText = evidenceScope(s.Text, ins.ActionEvidence.Text)
				}
			}
		}
		if sourceAddCondition.MatchString(conditionText) {
			ins.RequiresAddFill = true
		}
		if strings.Contains(conditionText, "倉位重") || strings.Contains(conditionText, "仓位重") {
			ins.Classification = ClassificationNeedsContext
		}
		if ins.ActionEvidence != nil {
			continue
		}
		// Compatibility with older model output: infer a source only from a
		// current action-bearing segment, never from a reference card.
		for _, s := range segments {
			if s.Role != "current" || s.Image || strings.TrimSpace(s.Text) == "" {
				continue
			}
			if ins.Action == ActionOpen || ins.Action == ActionAdd {
				if !sourceOpen.MatchString(s.Text) && !(ins.Action == ActionAdd && sourceAdd.MatchString(s.Text)) {
					continue
				}
			} else if !sourceManagement.MatchString(s.Text) && !sourceCancel.MatchString(s.Text) {
				continue
			}
			ins.ActionEvidence = &ActionEvidence{SourceID: s.ID, Text: s.Text}
			break
		}
	}
	return interp
}

// Keep the qualifying clause around quoted evidence. In a multi-symbol body,
// restrict it to that symbol's block so another trade's condition does not leak.
func evidenceScope(body, evidence string) string {
	index := strings.Index(body, evidence)
	if index < 0 {
		return body
	}
	matches := sourceSymbol.FindAllStringIndex(body, -1)
	start, end := 0, len(body)
	for _, m := range matches {
		if m[0] <= index {
			start = m[0]
		} else if m[0] >= index+len(evidence) {
			end = m[0]
			break
		}
	}
	if len(matches) <= 1 {
		return body
	}
	return body[start:end]
}

func ValidateActionEvidence(ins *SourceInterpretation, segments []SourceSegment) (SkipReason, error) {
	if !ins.IsActionable() {
		return SkipNone, nil
	}
	if ins.ActionEvidence == nil {
		return SkipSourceEvidence, nil
	}
	for _, s := range segments {
		if s.ID != ins.ActionEvidence.SourceID {
			continue
		}
		text := strings.TrimSpace(ins.ActionEvidence.Text)
		if s.Image && !ins.EvidenceVerified {
			return SkipSourceEvidence, nil
		}
		if s.Role != "current" || text == "" || (!s.Image && !strings.Contains(s.Text, text)) {
			return SkipSourceEvidence, nil
		}
		if len(ins.EligibilityConditions) > 0 {
			return SkipNeedsContext, nil
		}
		scope := text
		if !s.Image {
			scope = evidenceScope(s.Text, text)
		}
		if sourceAddCondition.MatchString(scope) && !ins.RequiresAddFill {
			return SkipNeedsContext, nil
		}
		if regexp.MustCompile(`(?i)(如果|若是|只有|only if|\bif\b)`).MatchString(scope) && !ins.RequiresAddFill && len(ins.ConditionalRules) == 0 {
			return SkipNeedsContext, nil
		}
		if (ins.Action == ActionOpen || ins.Action == ActionAdd) && !s.Image {
			mentions := sourceSymbol.FindAllStringSubmatch(s.Text, -1)
			if len(mentions) > 0 {
				match := false
				want, _ := ResolveInstrument(ins.Symbol)
				for _, m := range mentions {
					symbol, _ := ResolveInstrument(m[1])
					if symbol != "" && symbol == want {
						match = true
					}
				}
				if !match {
					return SkipSourceEvidence, nil
				}
			}
		}
		openingEvidence := sourceOpen.MatchString(text) || (ins.Action == ActionAdd && sourceAdd.MatchString(text))
		if (ins.Action == ActionOpen || ins.Action == ActionAdd) && (!openingEvidence || sourceNegation.MatchString(text)) {
			return SkipSourceEvidence, nil
		}
		if ins.Action != ActionOpen && ins.Action != ActionAdd && !sourceManagement.MatchString(text) && !sourceCancel.MatchString(text) {
			return SkipSourceEvidence, nil
		}
		return SkipNone, nil
	}
	return SkipSourceEvidence, nil
}
