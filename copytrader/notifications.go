package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"nofx/discord"
	"nofx/store"
	"regexp"
	"strconv"
	"strings"
)

// Deliberately narrow: a quoted header or a mention later in a recap does not
// grant opening authority. Unknown formats continue through AI and the guard.
var errNotificationEnded = errors.New("opening superseded by received terminal notification")
var errNotificationPending = errors.New("notification outcome requires reconciliation")
var notificationDirection = regexp.MustCompile(`(?i)\b(long|short)\b|做多|做空`)

func notificationRoots(msg *store.DiscordMessage) map[string]bool {
	roots := map[string]bool{}
	if msg.ReplyToMessageID != "" {
		roots[msg.ReplyToMessageID] = true
	}
	for _, link := range discord.ExtractMessageLinks(msg.Content + "\n" + msg.EmbedsJSON) {
		roots[link.MessageID] = true
	}
	return roots
}

func notificationReferences(root *store.DiscordMessage, refs map[string]bool) bool {
	if refs[root.MessageID] {
		return true
	}
	for id := range notificationRoots(root) {
		if refs[id] {
			return true
		}
	}
	return false
}

func notificationConflicts(msg *store.DiscordMessage, symbol string, direction Direction) bool {
	text := discord.FlattenEmbeds(discord.ParseStoredEmbeds(msg.EmbedsJSON))
	if conflictingCardPrices(text) {
		return true
	}
	for _, value := range notificationDirection.FindAllString(text, -1) {
		long := strings.EqualFold(value, "long") || value == "做多"
		if long != (direction == DirectionLong) {
			return true
		}
	}
	// A second, contradictory language/header cannot silently change the first.
	for _, line := range strings.Split(msg.Content, "\n")[1:] {
		s, d, k := notificationIdentity(line)
		if k != "" && (s != symbol || d != direction) {
			return true
		}
	}
	for _, m := range cardSymbol.FindAllStringSubmatch(text, -1) {
		if strings.ToUpper(m[1])+"USDT" != symbol {
			return true
		}
	}
	return false
}

var notificationScalar = regexp.MustCompile(`(?i)^\s*[0-9]+(?:,[0-9]{3})*(?:\.[0-9]+)?\s*(?:\(LIMIT\))?\s*$`)
var notificationHeader = regexp.MustCompile(`(?i)^\s*(?:[🔴🟢]\s*)?([A-Z0-9]+)/USDT\s+(long|short)\s+was\s+(added|closed|updated)\s*$`)

func notificationIdentity(text string) (string, Direction, string) {
	line := strings.Split(strings.TrimSpace(text), "\n")[0]
	line = strings.NewReplacer("*", "", "`", "").Replace(line)
	m := notificationHeader.FindStringSubmatch(line)
	if m == nil {
		return "", "", ""
	}
	return strings.ToUpper(m[1]) + "USDT", Direction(strings.ToUpper(m[2])), strings.ToLower(m[3])
}

// Only a complete, scalar, internally consistent card gets the deterministic
// fast path. Complex ranges, ratios and conditional prices remain AI work.
func notificationParameters(msg *store.DiscordMessage) ([]EntryOrder, []TPLevel, []SLLevel, bool) {
	text := discord.FlattenEmbeds(discord.ParseStoredEmbeds(msg.EmbedsJSON))
	if conflictingCardPrices(text) {
		return nil, nil, nil, false
	}
	values := map[string]float64{}
	limit := false
	clean := strings.NewReplacer("*", "", "`", "", "$", "").Replace(text)
	for _, line := range strings.Split(clean, "\n") {
		m := cardField.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := strings.ToLower(m[1])
		body := strings.Split(strings.Split(m[2], "<")[0], ":Chroma")[0]
		if !notificationScalar.MatchString(body) {
			return nil, nil, nil, false
		}
		nums := cardNumber.FindAllString(body, -1)
		if len(nums) != 1 || strings.ContainsAny(body, "%～~—") {
			return nil, nil, nil, false
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(nums[0], ",", ""), 64)
		if err != nil || value <= 0 || !finite(value) {
			return nil, nil, nil, false
		}
		switch {
		case key == "entry" || strings.HasPrefix(key, "入"):
			key = "entry"
			limit = strings.Contains(strings.ToLower(body), "limit")
		case key == "tp" || strings.HasPrefix(key, "止盈"):
			key = "tp"
		case key == "stop/loss" || strings.HasPrefix(key, "止"):
			key = "sl"
		default:
			continue
		}
		values[key] = value
	}
	if !limit || values["entry"] == 0 || values["tp"] == 0 || values["sl"] == 0 {
		return nil, nil, nil, false
	}
	return []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: values["entry"]}}}, []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: values["tp"]}}}, []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: values["sl"]}}}, true
}
func InterpretChromaNotification(msg *store.DiscordMessage, rules MessageRules) *SourceInterpretation {
	if rules.SourceMode != "chroma" {
		return nil
	}
	sym, dir, kind := notificationIdentity(msg.Content)
	if kind == "" {
		return nil
	}
	ins := &SourceInterpretation{Classification: ClassificationSignal, Symbol: sym, Direction: dir, ActionEvidence: &ActionEvidence{SourceID: "body", Text: msg.Content}, Reasoning: "current Chroma notification; attached card supplies parameters, target state checked separately"}
	switch kind {
	case "closed":
		ins.Action = ActionClose
		ins.CloseMode = CloseModeFull
	case "added":
		entries, tp, sl, ok := notificationParameters(msg)
		if !ok {
			return nil
		}
		ins.Action = ActionOpen
		ins.EntryOrders = entries
		ins.TakeProfitLevels = tp
		ins.StopLossLevels = sl
	default:
		return nil
	}
	return ins
}

// Runs for BOTH deterministic and AI output. Untrusted model fields never
// create notification authority; the current header is rechecked here.
func ApplyNotificationPolicy(interp *SourceInterpretation, msg *store.DiscordMessage, rules MessageRules) *SourceInterpretation {
	if rules.SourceMode != "chroma" {
		return interp
	}
	sym, dir, kind := notificationIdentity(msg.Content)
	for _, ins := range interp.Flatten() {
		ins.NotificationKind = ""
		if !ins.IsActionable() {
			continue
		}
		canonical, _ := ResolveInstrument(ins.Symbol)
		allowed := kind == "added" && ins.Action == ActionOpen || kind == "closed" && ins.Action == ActionClose || kind == "updated" && (ins.Action == ActionUpdateSL || ins.Action == ActionUpdateTP)
		if !allowed || canonical != sym || ins.Direction != dir || notificationConflicts(msg, sym, dir) {
			ins.Classification = ClassificationAmbiguous
			ins.Reasoning = "notification action, direction or parameters conflict with current source"
			continue
		}
		ins.NotificationKind = kind
		if kind == "closed" {
			ins.CloseMode = CloseModeFull
			ins.CloseRatio = nil
		}
		ins.ActionEvidence = &ActionEvidence{SourceID: "body", Text: msg.Content}
	}
	return interp
}

// Return a unique historical open, including failed and completed attempts.
// Never filter to active positions first: a newer failed open is evidence that
// a later "closed" notification must not close an older same-symbol trade.
func (e *Engine) notificationTarget(msg *store.DiscordMessage, ins *SourceInterpretation, rules MessageRules) (*store.CopyTradeContext, string, error) {
	days := rules.ContextDays
	if days <= 0 {
		days = 5
	}
	rows, err := e.st.CopyTrade().GetContextSignalsForSources(e.traderID, rules.SourceChannels, msg.MessageTimestamp.AddDate(0, 0, -days), 1001)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
	}
	if len(rows) > 1000 {
		return nil, "", fmt.Errorf("candidate history truncated; cannot associate safely")
	}
	roots := notificationRoots(msg)
	if len(roots) == 0 && e.source != nil && !e.source.HistoryComplete(e.traderID) {
		return nil, "", fmt.Errorf("source history incomplete; explicit original-card association required")
	}
	candidates := map[string]*store.CopyTradeSignal{}
	cardEntry, cardTP, cardSL, complete := notificationParameters(msg)
	for _, sig := range rows {
		if sig.MessageID == msg.MessageID || sig.Action != string(ActionOpen) || sig.MessageTimestamp.After(msg.MessageTimestamp) {
			continue
		}
		canonical, _ := ResolveInstrument(sig.Symbol)
		target, _ := ResolveInstrument(ins.Symbol)
		if canonical != target || sig.Direction != string(ins.Direction) {
			continue
		}
		root, err := e.st.DiscordMessage().DeliveryMessage(e.traderID, sig.MessageID, sig.MessageRevision)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
		}
		if root == nil {
			root, err = e.st.DiscordMessage().GetByMessageID(sig.ChannelID, sig.MessageID)
			if err != nil {
				return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
			}
		}
		if root == nil || root.AuthorID != msg.AuthorID || !rules.AllowsAuthor(root) {
			continue
		}
		_, _, kind := notificationIdentity(root.Content)
		if kind != "added" {
			continue
		}
		if len(roots) > 0 && !notificationReferences(root, roots) {
			continue
		}
		// Explicit original-card evidence has priority. An UPDATE card contains
		// the new TP/SL, so comparing that field with the original would reject
		// every valid change. Without a reference these updates require a unique
		// historical candidate instead of matching on the newly changed prices.
		if complete && len(roots) == 0 && ins.Action != ActionUpdateSL && ins.Action != ActionUpdateTP {
			entry, tp, sl, ok := notificationParameters(root)
			if !ok || entry[0].Price.Price != cardEntry[0].Price.Price || tp[0].Price.Price != cardTP[0].Price.Price || sl[0].Price.Price != cardSL[0].Price.Price {
				continue
			}
		}
		if previous := candidates[root.MessageID]; previous == nil || previous.MessageRevision < sig.MessageRevision {
			candidates[root.MessageID] = sig
		}
	}
	if len(candidates) != 1 {
		return nil, "", fmt.Errorf("notification target missing or ambiguous (%d candidates)", len(candidates))
	}
	var sig *store.CopyTradeSignal
	for _, v := range candidates {
		sig = v
	}
	if sig.TradeContextID == "" {
		actions, err := e.st.CopyTrade().GetActionsForMessage(e.traderID, sig.MessageID)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
		}
		for _, a := range actions {
			if a.Status == "uncertain" || a.Status == "executing" {
				return nil, "", fmt.Errorf("%w: opening result unknown", errNotificationPending)
			}
		}
		if sig.Status == store.SignalStatusFailed || sig.Status == store.SignalStatusSkipped {
			return nil, "identified ended notification; opening failed or was not executed", nil
		}
		return nil, "", fmt.Errorf("%w: opening has no confirmed outcome", errNotificationPending)
	}
	ctx, err := e.st.CopyTrade().GetContext(sig.TradeContextID)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
	}
	if ctx == nil {
		return nil, "", fmt.Errorf("tracked context missing")
	}
	switch ctx.State {
	case "CLOSED", "CANCELLED", "FAILED":
		orders, err := e.st.CopyTrade().GetOrdersForContext(e.traderID, ctx.ID)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errNotificationPending, err)
		}
		for _, o := range orders {
			switch o.Status {
			case "FILLED", "CANCELED", "CANCELLED", "REJECTED", "ABANDONED", "EXPIRED":
			default:
				return nil, "", fmt.Errorf("%w: ended context has unresolved orders", errNotificationPending)
			}
		}

		return nil, "identified ended notification; tracked trade already ended", nil
	}
	return ctx, "", nil
}

// An ended notice can arrive while OPEN is waiting on AI. Scan the durable
// inbox before any order, rather than waiting for that notice's queue turn.
func (e *Engine) notificationOpenGate(msg *store.DiscordMessage, ins *SourceInterpretation, rules MessageRules) error {
	rows, err := e.st.DiscordMessage().RecentInbound(rules.SourceChannels, msg.ReceivedAt)
	if err != nil {
		return err
	}
	if len(rows) >= 1000 {
		return fmt.Errorf("pending notification history incomplete")
	}
	target, _ := ResolveInstrument(ins.Symbol)
	for _, row := range rows {
		var later store.DiscordMessage
		if json.Unmarshal([]byte(row.MessageJSON), &later) != nil {
			continue
		}
		sym, dir, kind := notificationIdentity(later.Content)
		if (later.MessageID != msg.MessageID || later.Revision > msg.Revision) && sym == target && dir == ins.Direction && kind == "closed" && later.AuthorID == msg.AuthorID && rules.AllowsAuthor(&later) && !later.MessageTimestamp.Before(msg.MessageTimestamp) {
			refs := notificationRoots(&later)
			if len(refs) > 0 && !notificationReferences(msg, refs) {
				continue // An explicit ending for another card does not revoke this open.
			}
			if notificationConflicts(&later, sym, dir) {
				return fmt.Errorf("%w: terminal notification has conflicting evidence", errNotificationPending)
			}
			if len(refs) == 0 {
				entry, tp, sl, ok := notificationParameters(msg)
				endEntry, endTP, endSL, endOK := notificationParameters(&later)
				if !ok || !endOK || entry[0].Price.Price != endEntry[0].Price.Price || tp[0].Price.Price != endTP[0].Price.Price || sl[0].Price.Price != endSL[0].Price.Price {
					return fmt.Errorf("%w: terminal notification cannot be uniquely tied to pending open", errNotificationPending)
				}
			}
			return errNotificationEnded
		}
	}
	return nil
}
