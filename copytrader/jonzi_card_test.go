package copytrader

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"nofx/discord"
	"nofx/store"
)

const jonziSampleChannel = "1452344484103848026"

func jonziSampleMessages(t *testing.T) []*store.DiscordMessage {
	t.Helper()
	b, err := os.ReadFile("../testdata/copytrading/channel_jonzi_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw []discord.Message
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]*store.DiscordMessage, 0, len(raw))
	for i := range raw {
		msg, err := discord.ToStoreMessage(&raw[i], jonziSampleChannel)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
	}
	return out
}

func jonziSample(t *testing.T, id string) *store.DiscordMessage {
	t.Helper()
	for _, msg := range jonziSampleMessages(t) {
		if msg.MessageID == id {
			return msg
		}
	}
	t.Fatalf("sample %s missing", id)
	return nil
}

// jonziFreshCard rebuilds a revision-0 card from a finished sample card: no
// strike-through, no Chroma alert, no trailed/manual-close lines, entry not ticked.
func jonziFreshCard(content string) string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		lower := strings.ToLower(t)
		if strings.Contains(t, ":ChromaAlert") || strings.HasPrefix(lower, "- trailed stop") || strings.HasPrefix(lower, "- manually closed") ||
			strings.HasPrefix(t, "- 跟踪止损") || strings.HasPrefix(t, "- 移动止损") || strings.HasPrefix(t, "- 手动平仓") {
			continue
		}
		line = strings.ReplaceAll(line, "~~", "")
		line = strings.ReplaceAll(line, " :ChromaCheck2:", "")
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestParseJonziCardTerminalVariants(t *testing.T) {
	for _, tc := range []struct {
		id, symbol, statusWord string
		dir                    Direction
	}{
		{"1544845105448353833", "BTCUSDT", "trailed stop was hit", DirectionLong},
		{"1544665288837042236", "BTCUSDT", "stop/loss was hit", DirectionLong},
		{"1529140187165229138", "BTCUSDT", "take profit was hit", DirectionShort},
		{"1544671784320766014", "BTCUSDT", "Trade was manually closed", DirectionLong},
		{"1526639480730288229", "VIRTUALUSDT", "trailed stop was hit", DirectionLong},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c := parseJonziCard(jonziSample(t, tc.id).Content)
			if !c.IsCard || c.Status != jonziCardTerminal || c.Symbol != tc.symbol || c.Direction != tc.dir || !c.Struck {
				t.Fatalf("card %+v", c)
			}
			if !strings.Contains(c.StatusLine, tc.statusWord) || !strings.Contains(c.StatusLine, ":ChromaAlert:") || strings.Contains(c.StatusLine, "交易") {
				t.Fatalf("status evidence is not the English status line: %q", c.StatusLine)
			}
			if c.Err != "" || c.IdentityConflict != "" || c.PriceConflict != "" {
				t.Fatalf("native card reported conflict: %+v", c)
			}
			if !strings.HasPrefix(c.GateText, c.StatusLine) || !strings.Contains(c.GateText, "Trade details") || strings.Contains(c.GateText, "Reason for setup") || strings.Contains(c.GateText, "Position sizing") || strings.Contains(c.GateText, "如果") {
				t.Fatalf("gate text must be status + Trade details only: %q", c.GateText)
			}
		})
	}
}

func TestParseJonziCardFieldsAndChecks(t *testing.T) {
	c := parseJonziCard(jonziSample(t, "1544845105448353833").Content)
	if c.Entry == nil || c.Entry.Value != 77005.1 || !c.Entry.Checked {
		t.Fatalf("entry %+v", c.Entry)
	}
	if c.StopLoss == nil || c.StopLoss.Value != 76851.3 || c.StopLoss.Checked {
		t.Fatalf("stop %+v", c.StopLoss)
	}
	if len(c.TakeProfits) != 1 || c.TakeProfits[0].Value != 77774 || c.TakeProfits[0].Checked {
		t.Fatalf("tp %+v", c.TakeProfits)
	}
	if c.TrailedStop == nil || c.TrailedStop.Value != 77005.1 || !c.TrailedStop.Checked {
		t.Fatalf("trailed %+v", c.TrailedStop)
	}

	fresh := parseJonziCard(jonziFreshCard(jonziSample(t, "1544845105448353833").Content))
	if !fresh.IsCard || fresh.Status != jonziCardNew || fresh.Struck || fresh.Entry == nil || fresh.Entry.Checked || fresh.TrailedStop != nil || fresh.StatusLine != "" {
		t.Fatalf("fresh card %+v", fresh)
	}
	if fresh.Err != "" || fresh.IdentityConflict != "" || fresh.PriceConflict != "" {
		t.Fatalf("fresh card conflict %+v", fresh)
	}
}

func TestParseJonziCardTrailedAlert(t *testing.T) {
	c := parseJonziCard(jonziSample(t, "1534640151538569348").Content)
	if !c.IsCard || c.Status != jonziCardTrailed || c.Symbol != "CRCL" || !c.Tradfi || c.Quote != "" || c.Direction != DirectionLong || c.Struck {
		t.Fatalf("card %+v", c)
	}
	if c.TrailedStop == nil || c.TrailedStop.Value != 63.4 || !strings.Contains(c.StatusLine, "Stops are trailed to") {
		t.Fatalf("trailed %+v / %q", c.TrailedStop, c.StatusLine)
	}
	if c.Entry == nil || c.Entry.Value != 63.4 || !c.Entry.Checked || c.StopLoss.Value != 55.43 || len(c.TakeProfits) != 1 || c.TakeProfits[0].Value != 190.95 {
		t.Fatalf("fields %+v", c)
	}
	if c.Err != "" || c.IdentityConflict != "" || c.PriceConflict != "" {
		t.Fatalf("conflict %+v", c)
	}
	// Alert-only trailed stop (no field) still yields the alert price.
	alertOnly := strings.Replace(jonziSample(t, "1534640151538569348").Content, "- Trailed stop: $`63.4`\n", "", 1)
	if c := parseJonziCard(alertOnly); c.Status != jonziCardTrailed || c.TrailedStop == nil || c.TrailedStop.Value != 63.4 {
		t.Fatalf("alert-only %+v", c)
	}
	// Alert and field disagreeing is a malformed card, never a guess.
	disagree := strings.Replace(jonziSample(t, "1534640151538569348").Content, "- Trailed stop: $`63.4`", "- Trailed stop: $`64.1`", 1)
	if c := parseJonziCard(disagree); c.Err == "" {
		t.Fatalf("trailed disagreement accepted %+v", c)
	}
}

func TestParseJonziCardManualCloseAndTitles(t *testing.T) {
	c := parseJonziCard(jonziSample(t, "1532457435313471620").Content)
	if c.Status != jonziCardTerminal || c.Symbol != "NQ" || c.Tradfi || c.Direction != DirectionShort || c.ManualClosePct != 100 || c.ManualClosePrice == nil || c.ManualClosePrice.Value != 28090.1 || !c.ManualClosePrice.Checked {
		t.Fatalf("manual close %+v", c)
	}
	nq := parseJonziCard(jonziSample(t, "1544748682732113940").Content)
	if !nq.IsCard || nq.Symbol != "NQ" || !nq.Tradfi || nq.Direction != DirectionShort || nq.Status != jonziCardTerminal {
		t.Fatalf("tradfi title %+v", nq)
	}
	// Partial manual close without a terminal alert stays partial.
	partial := strings.Replace(jonziFreshCard(jonziSample(t, "1532457435313471620").Content), "- TP: $`27081`", "- TP: $`27081`\n- Manually closed (50%): $`28000` :ChromaCheck2: (<t:1785438537:R>)", 1)
	if p := parseJonziCard(partial); p.Status != jonziCardPartial || p.ManualClosePct != 50 || p.ManualClosePrice.Value != 28000 {
		t.Fatalf("partial %+v", p)
	}
	// A struck title without any terminal status is malformed.
	struck := strings.Replace(jonziFreshCard(jonziSample(t, "1544845105448353833").Content), "## 🟢 BTC/USDT long", "## ~~🟢 BTC/USDT long~~", 1)
	if s := parseJonziCard(struck); s.Err == "" {
		t.Fatalf("struck without status accepted %+v", s)
	}
	// "**Reason for setup:**" bold-label layout with a multi-level TP ladder.
	ladder := strings.Replace(jonziFreshCard(jonziSample(t, "1534640151538569348").Content), "- TP: $`190.95`", "- TP: $`120`\n- TP: $`190.95`", 1)
	ladder = strings.Replace(ladder, "- 止盈(TP)：`190.95`美元", "- 止盈(TP)：`120`美元\n- 止盈(TP)：`190.95`美元", 1)
	if l := parseJonziCard(ladder); len(l.TakeProfits) != 2 || l.TakeProfits[0].Value != 120 || l.PriceConflict != "" {
		t.Fatalf("tp ladder %+v", l)
	}
}

func TestParseJonziCardTranslationConflicts(t *testing.T) {
	base := jonziSample(t, "1544845105448353833").Content
	price := strings.Replace(base, "- 入场：$`77005.1`", "- 入场：$`77015.1`", 1)
	if c := parseJonziCard(price); c.PriceConflict == "" || c.Entry.Value != 77005.1 {
		t.Fatalf("translated price conflict missed / value not English: %+v", c)
	}
	dir := strings.Replace(base, "## ~~🟢 BTC/USDT 多(long)~~", "## ~~🔴 BTC/USDT 空(short)~~", 1)
	if c := parseJonziCard(dir); c.IdentityConflict == "" || c.Direction != DirectionLong {
		t.Fatalf("translated direction conflict missed: %+v", c)
	}
	sym := strings.Replace(base, "## ~~🟢 BTC/USDT 多(long)~~", "## ~~🟢 ETH/USDT 多(long)~~", 1)
	if c := parseJonziCard(sym); c.IdentityConflict == "" {
		t.Fatalf("translated symbol conflict missed: %+v", c)
	}
	// Reasoning text that mentions another market or direction is not identity.
	narrative := strings.Replace(base, "LTF MMBM", "LTF MMBM, ETH/USDT short looks weak", 1)
	if c := parseJonziCard(narrative); c.IdentityConflict != "" || c.Symbol != "BTCUSDT" {
		t.Fatalf("free text treated as identity: %+v", c)
	}
}

func TestParseJonziCardRejectsNonCards(t *testing.T) {
	for _, body := range []string{
		jonziSample(t, "1544685110681669735").Content, // "+3R booked" commentary
		jonziSample(t, "1543644202569830570").Content, // "ZEC | 4R setup"
		"> " + strings.ReplaceAll(jonziSample(t, "1544845105448353833").Content, "\n", "\n> "),
		"## BTC/USDT long\nno details here",
	} {
		if c := parseJonziCard(body); c.IsCard {
			t.Fatalf("non-card parsed as card: %q", body[:40])
		}
	}
}

func TestSourceCueCompoundsAreNotNegations(t *testing.T) {
	for _, s := range []string{"日线级别VI", "日線級別", "没有区别", "分别在", "特别注意", "类别", "个别", "识别", "告别", "差别", "性别"} {
		if matchesSourceCue(sourceActionNegation, s) || matchesSourceCue(sourceNegation, s) {
			t.Fatalf("compound %q matched as negation", s)
		}
	}
	for _, s := range []string{"别平仓", "別進場", "级别 别开", "不要减仓", "如果到了"} {
		if !matchesSourceCue(sourceNegation, s) {
			t.Fatalf("real cue %q lost", s)
		}
	}
	if !matchesSourceCue(sourceConditional, "nice raid if internal low") {
		t.Fatal("english conditional lost")
	}
}
