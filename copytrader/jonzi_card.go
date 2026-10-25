package copytrader

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// jonzi native cards are posted by the "JONZi" webhook and later EDITED in
// place: entry ticks, trailed stops and terminal status all arrive as edits of
// the original message. The English section (before the first dashed
// separator) is authoritative; the machine-translated Chinese section only
// serves to detect conflicting translations.

type jonziCardStatus string

const (
	jonziCardNew      jonziCardStatus = "NEW"      // no trailed/terminal/partial state
	jonziCardTrailed  jonziCardStatus = "TRAILED"  // stop trailed, trade still open
	jonziCardTerminal jonziCardStatus = "TERMINAL" // trade finished (SL/TP/trailed hit, manual 100%)
	jonziCardPartial  jonziCardStatus = "PARTIAL"  // manually closed N% < 100
)

type jonziCardPrice struct {
	Value   float64
	Checked bool   // :ChromaCheck2: — this level has already triggered
	Line    string // raw source line
}

type jonziCard struct {
	IsCard    bool // card header + Trade details block found
	Symbol    string
	RawSymbol string
	Quote     string // USDT / USD / USDC; empty for "SYMBOL long" and Tradfi titles
	Tradfi    bool
	Direction Direction
	Struck    bool // ~~title~~: the author's card is finished
	Status    jonziCardStatus

	StatusLines []string // English status (ChromaAlert) lines, raw
	StatusLine  string   // raw line that determined Status (action evidence)

	Entry            *jonziCardPrice
	TakeProfits      []jonziCardPrice
	StopLoss         *jonziCardPrice
	TrailedStop      *jonziCardPrice // effective trailed stop (alert or field)
	TrailedLine      string          // raw line carrying the trailed stop
	ManualClosePct   float64
	ManualClosePrice *jonziCardPrice

	DetailsText string // raw English Trade details block
	GateText    string // English status lines + Trade details block

	Err              string // malformed English card
	IdentityConflict string // symbol/direction differ between headers/translations
	PriceConflict    string // a price differs between the English and translated fields
}

var jonziSeparator = regexp.MustCompile(`(?m)^[ \t]*-{10,}[ \t]*$`)
var jonziHeader = regexp.MustCompile(`(?i)^([A-Z0-9][A-Z0-9.]*)(?:/(USDT|USDC|USD))?\s+(long|short)(?:\s*\(\s*([^()]*?)\s*\))?$`)
var jonziDetailsHeader = regexp.MustCompile(`(?i)^(?:#{1,6}\s*)?trade details\s*:?$`)
var jonziSectionHeader = regexp.MustCompile(`^(?:#{1,6}\s+\S|\*\*[^*\n]+\*\*\s*$)`)
var jonziTimestamp = regexp.MustCompile(`<t:[0-9]+(?::[A-Za-z])?>`)
var jonziCheck = regexp.MustCompile(`:ChromaCheck[0-9]*:`)
var jonziEmote = regexp.MustCompile(`:Chroma[A-Za-z0-9_]*:`)
var jonziFieldLine = regexp.MustCompile(`(?i)^[-•]?\s*(entry|trailed stop|stop/loss|tp\s*[0-9]*|manually closed\s*\(\s*([0-9]+(?:\.[0-9]+)?)\s*%\s*\))\s*[:：]\s*(.*)$`)
var jonziTrailedTo = regexp.MustCompile(`(?i)\bstops?\s+(?:(?:are|is|were|was|have\s+been|has\s+been)\s+)?trailed\s+to\s+([0-9][0-9,]*(?:\.[0-9]+)?)`)

// jonziCleanLine removes Discord/markdown decoration so field values parse:
// backticks, bold, dollar signs, strike-through, relative timestamps and
// Chroma emotes. Empty "()" left behind by a removed timestamp is dropped.
func jonziCleanLine(line string) string {
	s := jonziTimestamp.ReplaceAllString(line, "")
	s = jonziEmote.ReplaceAllString(s, "")
	s = strings.NewReplacer("`", "", "*", "", "$", "", "~~", "", "()", "").Replace(s)
	return strings.TrimSpace(s)
}

func jonziHeaderLine(line string) (sym, quote string, dir Direction, tradfi, struck, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return
	}
	struck = strings.Contains(t, "~~")
	t = strings.NewReplacer("~~", "", "*", "").Replace(strings.TrimLeft(t, "#"))
	t = strings.TrimLeftFunc(t, func(r rune) bool { return !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))) })
	m := jonziHeader.FindStringSubmatch(strings.TrimSpace(t))
	if m == nil {
		return
	}
	dir = DirectionLong
	if strings.EqualFold(m[3], "short") {
		dir = DirectionShort
	}
	return strings.ToUpper(m[1]), strings.ToUpper(m[2]), dir, strings.EqualFold(m[4], "tradfi"), struck, true
}

func jonziParseNumber(s string) (float64, bool) {
	n := cardNumber.FindString(s)
	if n == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(n, ",", ""), 64)
	if err != nil || !finite(v) || v <= 0 {
		return 0, false
	}
	return v, true
}

// parseJonziCard parses a jonzi native card. Values come only from the English
// section; the translated section is compared for conflicts and never supplies
// a value. Tradeability of the symbol is decided later by ResolveInstrument.
func parseJonziCard(text string) jonziCard {
	var c jonziCard
	en, zh := text, ""
	if loc := jonziSeparator.FindStringIndex(text); loc != nil {
		en, zh = text[:loc[0]], text[loc[1]:]
	}
	lines := strings.Split(en, "\n")
	header := -1
	for i, line := range lines {
		if sym, quote, dir, tradfi, struck, ok := jonziHeaderLine(line); ok {
			c.RawSymbol, c.Quote, c.Direction, c.Tradfi, c.Struck = sym, quote, dir, tradfi, struck
			c.Symbol = sym
			if quote != "" {
				c.Symbol = sym + "USDT"
			}
			header = i
			break
		}
	}
	if header < 0 {
		return c
	}
	detailsStart, detailsEnd := -1, -1
	for i := header + 1; i < len(lines); i++ {
		if detailsStart < 0 {
			if jonziDetailsHeader.MatchString(jonziCleanLine(lines[i])) {
				detailsStart = i
			}
			continue
		}
		t := strings.TrimSpace(lines[i])
		if t == "" || jonziSectionHeader.MatchString(t) {
			detailsEnd = i
			break
		}
	}
	if detailsStart < 0 {
		return c
	}
	if detailsEnd < 0 {
		detailsEnd = len(lines)
	}
	c.IsCard = true
	c.DetailsText = strings.TrimSpace(strings.Join(lines[detailsStart:detailsEnd], "\n"))

	// Status lines: Chroma alerts in the English section. Older or synthetic
	// cards without the emote fall back to a bare status sentence outside the
	// author's free-text sections (reason/closing reason/sizing).
	for _, line := range lines[header+1:] {
		if strings.Contains(line, ":ChromaAlert") {
			c.StatusLines = append(c.StatusLines, strings.TrimSpace(line))
		}
	}
	if len(c.StatusLines) == 0 {
		section := ""
		for i := header + 1; i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			clean := strings.ToLower(jonziCleanLine(t))
			if jonziSectionHeader.MatchString(t) || strings.HasPrefix(t, "#") {
				section = strings.Trim(strings.TrimLeft(clean, "# "), ": ")
				continue
			}
			if strings.HasPrefix(section, "reason") || strings.HasPrefix(section, "closing") || strings.HasPrefix(section, "position") || strings.HasPrefix(section, "risk") || strings.HasPrefix(section, "chart") {
				continue
			}
			if i > detailsStart && i < detailsEnd {
				continue
			}
			if jonziTerminal.MatchString(t) || jonziTrailedTo.MatchString(jonziCleanLine(t)) {
				c.StatusLines = append(c.StatusLines, t)
			}
		}
	}

	for _, raw := range lines[detailsStart+1 : detailsEnd] {
		line := strings.TrimSpace(raw)
		checked := jonziCheck.MatchString(line)
		m := jonziFieldLine.FindStringSubmatch(jonziCleanLine(line))
		if m == nil {
			continue
		}
		key := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
		value, ok := jonziParseNumber(m[3])
		price := jonziCardPrice{Value: value, Checked: checked, Line: line}
		switch {
		case key == "entry":
			if ok && c.Entry == nil {
				c.Entry = &price
			}
		case key == "trailed stop":
			if !ok {
				c.Err = "trailed stop has no valid price"
				continue
			}
			c.TrailedStop, c.TrailedLine = &price, line
		case key == "stop/loss":
			if ok && c.StopLoss == nil {
				c.StopLoss = &price
			}
		case strings.HasPrefix(key, "tp"):
			for _, n := range cardNumber.FindAllString(m[3], -1) {
				if v, ok := jonziParseNumber(n); ok {
					c.TakeProfits = append(c.TakeProfits, jonziCardPrice{Value: v, Checked: checked, Line: line})
				}
			}
		case strings.HasPrefix(key, "manually closed"):
			pct, err := strconv.ParseFloat(m[2], 64)
			if err != nil || !finite(pct) || pct <= 0 || pct > 100 {
				c.Err = "manual close percentage is invalid"
				continue
			}
			c.ManualClosePct = pct
			if ok {
				c.ManualClosePrice = &price
			}
		}
	}

	terminalLine, trailedAlert := "", ""
	var trailedAlertPrice float64
	for _, line := range c.StatusLines {
		if terminalLine == "" && jonziTerminal.MatchString(line) {
			terminalLine = line
		}
		if m := jonziTrailedTo.FindStringSubmatch(jonziCleanLine(line)); m != nil && trailedAlert == "" {
			if v, ok := jonziParseNumber(m[1]); ok {
				trailedAlert, trailedAlertPrice = line, v
			} else {
				c.Err = "trailed stop alert has no valid price"
			}
		}
	}
	if trailedAlert != "" {
		if c.TrailedStop != nil && c.TrailedStop.Value != trailedAlertPrice {
			c.Err = "trailed stop alert and field disagree"
		}
		if c.TrailedStop == nil {
			c.TrailedStop = &jonziCardPrice{Value: trailedAlertPrice, Line: trailedAlert}
		}
		c.TrailedLine = trailedAlert
	}
	manualLine := ""
	if c.ManualClosePrice != nil {
		manualLine = c.ManualClosePrice.Line
	}
	switch {
	case terminalLine != "":
		c.Status, c.StatusLine = jonziCardTerminal, terminalLine
	case c.ManualClosePct >= 100:
		c.Status, c.StatusLine = jonziCardTerminal, manualLine
	case c.ManualClosePct > 0:
		c.Status, c.StatusLine = jonziCardPartial, manualLine
	case c.TrailedStop != nil:
		c.Status, c.StatusLine = jonziCardTrailed, c.TrailedLine
	default:
		c.Status = jonziCardNew
		if len(c.StatusLines) > 0 {
			c.StatusLine = c.StatusLines[0]
		}
	}
	if c.Struck && c.Status != jonziCardTerminal && c.Err == "" {
		c.Err = "struck-through card title without a terminal status"
	}
	gate := append([]string(nil), c.StatusLines...)
	if c.StatusLine != "" && !containsString(gate, c.StatusLine) {
		gate = append(gate, c.StatusLine)
	}
	c.GateText = strings.TrimSpace(strings.Join(append(gate, c.DetailsText), "\n"))

	// Translation consistency. Without a separator (synthetic or truncated
	// cards), every text outside the English details block is the comparison side.
	other := zh
	if other == "" {
		other = strings.Join(append(append([]string(nil), lines[:detailsStart]...), lines[detailsEnd:]...), "\n")
	}
	c.IdentityConflict = jonziIdentityConflict(text, c)
	enFields := jonziTranslatedFields(c.DetailsText)
	for key, values := range jonziTranslatedFields(other) {
		if prior, ok := enFields[key]; ok && strings.Join(prior, ",") != strings.Join(values, ",") {
			c.PriceConflict = "card " + key + " differs between English and translated fields"
			break
		}
	}
	return c
}

// Headers in either language must agree on symbol and direction; mentions in
// free text (reasoning, sizing) are not identity facts.
func jonziIdentityConflict(text string, c jonziCard) string {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		for _, m := range cardDirection.FindAllString(t, -1) {
			d := DirectionLong
			if strings.EqualFold(m, "short") || strings.Contains(m, "空") {
				d = DirectionShort
			}
			if d != c.Direction {
				return "card direction conflicts across headers or translations"
			}
		}
		for _, m := range cardSymbol.FindAllStringSubmatch(t, -1) {
			if strings.ToUpper(m[1]) != c.RawSymbol {
				return "card symbol conflicts across headers or translations"
			}
		}
	}
	return ""
}

// jonziTranslatedFields collects field values with the same label mapping as
// conflictingCardPrices, keeping the per-key order so multi-level TPs compare
// as a ladder instead of conflicting with themselves.
func jonziTranslatedFields(text string) map[string][]string {
	fields := map[string][]string{}
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
		for _, n := range cardNumber.FindAllString(body, -1) {
			v, err := strconv.ParseFloat(strings.ReplaceAll(n, ",", ""), 64)
			if err != nil || !finite(v) || v <= 0 {
				fields[key] = append(fields[key], "invalid:"+n)
				continue
			}
			fields[key] = append(fields[key], strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	return fields
}
