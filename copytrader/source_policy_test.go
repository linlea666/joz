package copytrader

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"nofx/discord"
	"nofx/store"
)

func quotedMessage(body string) *store.DiscordMessage {
	now := time.Now().UTC()
	embeds, _ := json.Marshal([]discord.Embed{{Description: "#FLOCK | 做多\n進場：市價\n止損：0.067\n止盈：0.07507 - 0.07785 - 0.08324", Timestamp: now.Add(-14 * time.Minute).Format(time.RFC3339), Author: &discord.EmbedAuthor{Name: "gks-合約風控聯盟"}}})
	return &store.DiscordMessage{MessageID: "quote", ChannelID: "chan-1", Content: body, MessageTimestamp: now, EmbedsJSON: string(embeds)}
}
func TestTylerScreenshotManagement(t *testing.T) {
	tests := []struct {
		body        string
		actions     []Action
		requiresAdd bool
	}{
		{"沒開玩笑，10分鐘不到2倍", []Action{ActionIgnore}, false},
		{"#FLOCK 起飛，跟不上自己反思", []Action{ActionIgnore}, false},
		{"#FLOCK 2X，止盈或者減倉給套", []Action{ActionReduce}, false},
		{"#FLOCK 提前平倉", []Action{ActionClose}, false},
		{"#FLOCK 減倉50%，止損移至TP1", []Action{ActionReduce, ActionUpdateSL}, false},
		{"#FLOCK 有補倉的自行提前作Tp1", []Action{ActionReduce}, true},
		{"#FLOCK TP1 已到", []Action{ActionIgnore}, false},
		{"#FLOCK 成本損", []Action{ActionUpdateSL}, false},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			msg := quotedMessage(tt.body)
			segments := BuildSourceSegments(msg, "tyler_v1")
			if segments[1].Role != "reference" {
				t.Fatal("quoted card is current")
			}
			got := InterpretKnownSource(msg, segments, "tyler_v1")
			if got == nil {
				t.Fatal("known sample not recognized")
			}
			items := got.Flatten()
			if len(items) != len(tt.actions) {
				t.Fatalf("actions %v", items)
			}
			for i, action := range tt.actions {
				if items[i].Action != action || items[i].RequiresAddFill != tt.requiresAdd {
					t.Fatalf("instruction %+v", items[i])
				}
				if action == ActionReduce && (items[i].CloseRatio == nil || *items[i].CloseRatio != 50) {
					t.Fatal("ambiguous partial profit not 50% remaining")
				}
				if skip, _ := ValidateActionEvidence(items[i], segments); skip != SkipNone {
					t.Fatalf("current evidence rejected %s", skip)
				}
			}
		})
	}
}

func TestTylerNaturalLanguageExitAndCancelIntents(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		action Action
	}{
		{name: "early stop short", body: "#ASTER 提前止損出", action: ActionClose},
		{name: "early stop full", body: "#ASTER 提前止损出局", action: ActionClose},
		{name: "this trade stopped", body: "#ASTER 这单止损出局", action: ActionClose},
		{name: "early exit", body: "#ASTER 提前离场", action: ActionClose},
		{name: "early leave", body: "#ASTER 提前退出", action: ActionClose},
		{name: "cancel pending", body: "#ASTER 撤挂单", action: ActionCancel},
		{name: "cancel pending with 掉", body: "#ASTER 撤掉挂单", action: ActionCancel},
		{name: "cancel limit", body: "#ASTER 取消挂单", action: ActionCancel},
		{name: "cancel order", body: "#ASTER 取消订单", action: ActionCancel},
		{name: "cancel this order", body: "#ASTER 取消这笔订单", action: ActionCancel},
		{name: "cancel entry order", body: "#ASTER 取消入场订单", action: ActionCancel},
		{name: "cancel limit order", body: "#ASTER 取消限价单", action: ActionCancel},
		{name: "cancel traditional order", body: "#ASTER 取消這筆訂單", action: ActionCancel},
		{name: "cancel traditional entry", body: "#ASTER 取消進場訂單", action: ActionCancel},
		{name: "cancel traditional limit", body: "#ASTER 取消限價單", action: ActionCancel},
		{name: "cancel add", body: "#ASTER 取消补仓", action: ActionCancel},
		{name: "cancel position is close", body: "#ASTER 取消仓位", action: ActionClose},
		{name: "partial close is reduce", body: "#ASTER 平仓50%", action: ActionReduce},
		{name: "stop hit status", body: "#ASTER 止损已触发", action: ActionClose},
		{name: "closed status", body: "#ASTER 交易已平仓", action: ActionClose},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &store.DiscordMessage{Content: tt.body}
			segments := BuildSourceSegments(msg, "tyler_v1")
			got := InterpretKnownSource(msg, segments, "tyler_v1")
			if got == nil || got.Action != tt.action {
				t.Fatalf("got %+v, want %s", got, tt.action)
			}
			if skip, detail, _ := ValidateActionEvidenceDetailed(got, segments); skip != SkipNone || detail != "" {
				t.Fatalf("recognized intent failed evidence validation: skip=%s detail=%q", skip, detail)
			}
		})
	}
}

func TestCancelOrderSourceGates(t *testing.T) {
	for _, profile := range []string{"default", "tyler_v1"} {
		for _, tt := range []struct {
			body   string
			action Action
			skip   SkipReason
		}{
			{"#BTC 取消订单", ActionCancel, SkipNone},
			{"#BTC 取消这笔订单", ActionCancel, SkipNone},
			{"#BTC 取消入场订单", ActionCancel, SkipNone},
			{"#BTC 取消限价单", ActionCancel, SkipNone},
			{"#BTC 取消這筆訂單", ActionCancel, SkipNone},
			{"#BTC 取消進場訂單", ActionCancel, SkipNone},
			{"#BTC 取消订单", ActionOpen, SkipSourceEvidence},
			{"#BTC 取消订单", ActionClose, SkipSourceEvidence},
			{"#BTC 取消仓位", ActionCancel, SkipSourceEvidence},
			{"#BTC 取消仓位", ActionClose, SkipNone},
			{"#BTC 不要取消订单", ActionCancel, SkipSourceEvidence},
			{"#BTC 暂不取消入场订单", ActionCancel, SkipSourceEvidence},
			{"#BTC 如果价格跌破90，取消订单", ActionCancel, SkipNeedsContext},
			{"#BTC 继续持有\n#ETH 取消订单", ActionCancel, SkipSourceEvidence},
		} {
			t.Run(profile+"/"+tt.body+"/"+string(tt.action), func(t *testing.T) {
				msg := &store.DiscordMessage{Content: tt.body}
				ins := &SourceInterpretation{Classification: ClassificationSignal, Action: tt.action, Symbol: "BTC", ActionEvidence: &ActionEvidence{SourceID: "body", Text: "AI paraphrase of the current instruction"}}
				if skip, detail, err := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(msg, profile)); err != nil || skip != tt.skip {
					t.Fatalf("skip=%s detail=%q err=%v, want %s", skip, detail, err, tt.skip)
				}
			})
		}
	}
}

func TestTylerMixedCancelAndOpenStillUsesInterpreter(t *testing.T) {
	for _, body := range []string{
		"#BTC 取消入场订单，然后市价做多，止损90",
		"#BTC 取消订单\n進場：100 止損：90",
		"#BTC cancel the entry and open short",
	} {
		msg := &store.DiscordMessage{Content: body}
		if got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1"); got != nil {
			t.Fatalf("mixed entry instruction overwritten: %q -> %+v", body, got)
		}
	}
}

func TestQuotedCancellationCannotAuthorizeCurrentAction(t *testing.T) {
	msg := &store.DiscordMessage{Content: "引用信息：#BTC 取消订单\n💬 回复：\n#BTC 继续等待"}
	segments := BuildSourceSegments(msg, "tyler_v1")
	if got := InterpretKnownSource(msg, segments, "tyler_v1"); got != nil {
		t.Fatalf("historical cancel became a current instruction: %+v", got)
	}
	for _, id := range []string{"body:reference:0", "body:current:1"} {
		ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionCancel, Symbol: "BTC", ActionEvidence: &ActionEvidence{SourceID: id, Text: "取消订单"}}
		if skip, _, _ := ValidateActionEvidenceDetailed(ins, segments); skip != SkipSourceEvidence {
			t.Fatalf("historical cancel authorized through %s: %s", id, skip)
		}
	}
}

func TestCancelOrderMessagePreservesFillsAndRequiresUniqueTarget(t *testing.T) {
	for _, tt := range []struct {
		name        string
		state       TradeState
		orderStatus string
		filled      float64
		ambiguous   bool
		wantState   TradeState
		wantSkip    SkipReason
		wantCancels int
	}{
		{name: "unfilled", state: StateEntryPending, orderStatus: "NEW", wantState: StateCancelled, wantCancels: 1},
		{name: "partial", state: StateEntryPending, orderStatus: "PARTIALLY_FILLED", filled: .2, wantState: StateOpen, wantCancels: 1},
		{name: "filled before cancel", state: StateEntryPending, orderStatus: "FILLED", filled: .5, wantState: StateOpen},
		{name: "already open", state: StateOpen, orderStatus: "FILLED", filled: .5, wantState: StateOpen, wantSkip: SkipAlreadyFlat},
		{name: "missing", wantSkip: SkipNeedsContext},
		{name: "ambiguous", state: StateEntryPending, orderStatus: "NEW", ambiguous: true, wantState: StateEntryPending, wantSkip: SkipNeedsContext},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStore(t)
			venue := &mockExchange{orderStatus: map[string]interface{}{"status": tt.orderStatus, "executedQty": tt.filled, "avgPrice": 100.0}}
			var ctx *store.CopyTradeContext
			if tt.state != "" {
				ctx = newTestContext(t, st, tt.state)
				if tt.ambiguous {
					second := *ctx
					second.ID, second.RootMessageID = "another-trade", "another-root"
					if err := st.CopyTrade().CreateContext(&second); err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg := DefaultCopyTradingConfig()
			cfg.PrimaryChannelID, cfg.InterpretationProfile = "chan-1", "tyler_v1"
			e := NewEngine(EngineParams{TraderID: "trader-1", Store: st, Exchange: venue, Config: &cfg})
			msg := &store.DiscordMessage{MessageID: "cancel-order", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC 取消入场订单"}
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			sig, err := st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if sig.Action != string(ActionCancel) || sig.SkipReason != string(tt.wantSkip) || sig.ErrorMessage != "" {
				t.Fatalf("unexpected cancellation result: %+v", sig)
			}
			if len(venue.cancelOrderLog) != tt.wantCancels || venue.closeCalled != 0 || len(venue.cancelAllLog) != 0 || len(venue.cancelSLLog) != 0 {
				t.Fatalf("wrong cancellation side effects: %+v", venue)
			}
			if ctx != nil {
				saved, err := st.CopyTrade().GetContext(ctx.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.State != string(tt.wantState) {
					t.Fatalf("state=%s, want=%s", saved.State, tt.wantState)
				}
				if tt.filled > 0 && saved.Quantity != tt.filled {
					t.Fatalf("filled quantity changed: %g", saved.Quantity)
				}
				if tt.filled > 0 && tt.state == StateEntryPending && len(venue.setStopLossLog) == 0 {
					t.Fatal("fill discovered during cancellation was not protected")
				}
			}
			// The same cancellation must remain harmless after a repeat delivery.
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			if len(venue.cancelOrderLog) != tt.wantCancels || venue.closeCalled != 0 {
				t.Fatal("repeat cancellation changed orders or positions")
			}
		})
	}
}

func TestTylerUntriggeredStopDoesNotClose(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#ASTER 止损未触发，继续持有"}
	if got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1"); got != nil {
		t.Fatalf("untriggered stop became an action: %+v", got)
	}
}

func TestNegatedManagementActionCannotPassEvidence(t *testing.T) {
	for _, body := range []string{"#ASTER 不要平仓", "#ASTER 暂不止盈", "#ASTER do not close"} {
		ins := &SourceInterpretation{
			Classification: ClassificationSignal,
			Action:         ActionClose,
			Symbol:         "ASTER",
			ActionEvidence: &ActionEvidence{SourceID: "body", Text: body},
		}
		if skip, detail, _ := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(&store.DiscordMessage{Content: body}, "default")); skip != SkipSourceEvidence || detail != "current evidence negates the parsed action" {
			t.Fatalf("negated close passed evidence: body=%q skip=%s detail=%q", body, skip, detail)
		}
	}
}

func TestQuotedOpenRejectedIndependentlyOfPrices(t *testing.T) {
	msg := quotedMessage("#FLOCK 止盈或者減倉")
	segments := BuildSourceSegments(msg, "default")
	ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, ActionEvidence: &ActionEvidence{SourceID: "embed:0", Text: "進場：市價"}}
	if skip, _ := ValidateActionEvidence(ins, segments); skip != SkipSourceEvidence {
		t.Fatal("historical open escaped source gate")
	}
}

func TestEvidenceWhitespaceDifferencesRemainCurrentSource(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#STEEM｜做空 📉\n\n進場：市價\n止損：0.0685\n\n止盈：\n0.06208-0.05898-0.05466"}
	ins := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionOpen,
		Symbol:         "STEEM",
		Direction:      DirectionShort,
		ActionEvidence: &ActionEvidence{SourceID: "body", Text: "#STEEM｜做空 📉\n進場：市價\n止損：0.0685\n止盈：\n0.06208-0.05898-0.05466"},
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(msg, "default")); skip != SkipNone || detail != "" {
		t.Fatalf("whitespace-only evidence mismatch: skip=%s detail=%q", skip, detail)
	}
}

func TestLegacyBodyEvidenceNormalizesToCurrentTylerSegment(t *testing.T) {
	msg := &store.DiscordMessage{Content: "⏰ #ADA｜做空\n進場：市價-0.2469\n止損：0.2511\n止盈：0.2334-0.2197"}
	segments := BuildSourceSegments(msg, "tyler_v1")
	ins := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionOpen,
		Symbol:         "ADA",
		Direction:      DirectionShort,
		ActionEvidence: &ActionEvidence{SourceID: "body", Text: "作者明确要求 ADA 做空并给出市价参考"},
	}
	got := ApplySourcePolicy(ins, msg, segments, "tyler_v1")
	if got == nil || got.ActionEvidence == nil || got.ActionEvidence.SourceID != "body" {
		t.Fatalf("legacy body evidence was not retained for unsplit body: %+v", got)
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(got, segments); skip != SkipNone || detail != "" {
		t.Fatalf("normalized current evidence rejected: skip=%s detail=%q evidence=%+v", skip, detail, got.ActionEvidence)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("normalization warning missing: %+v", got.Warnings)
	}
}

func TestLegacyBodyEvidenceMapsToTylerCurrentSegment(t *testing.T) {
	body := "引用信息：#SOL｜做多\n進場：116.5\n止損：113.5\n\n💬 回复：\n#SOL 提前止損出"
	msg := &store.DiscordMessage{Content: body}
	segments := BuildSourceSegments(msg, "tyler_v1")
	ins := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionClose,
		Symbol:         "SOL",
		Direction:      DirectionLong,
		ActionEvidence: &ActionEvidence{SourceID: "body", Text: "作者要求平仓"},
	}
	got := ApplySourcePolicy(ins, msg, segments, "tyler_v1")
	if got == nil || got.ActionEvidence == nil || got.ActionEvidence.SourceID != "body:current:1" {
		t.Fatalf("legacy body evidence did not map to current reply: %+v", got)
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(got, segments); skip != SkipNone || detail != "" {
		t.Fatalf("current mapped evidence rejected: skip=%s detail=%q evidence=%+v", skip, detail, got.ActionEvidence)
	}
}

func TestClosedStatusCardCannotAuthorizeNewOpen(t *testing.T) {
	for _, body := range []string{
		"## 🟢 BTC/USDT long\nTrade was manually closed\nEntry: 82889.4\nTP: 83825.6",
		"## 🟢 BTC/USDT long\n交易已平仓\n入场价: 82889.4\n止盈: 83825.6",
		"## 🟢 BTC/USDT 做多\n交易已平仓\n进场价: 82889.4\n止盈: 83825.6",
		"## 🟢 BTC/USDT 做多\n交易已平仓\n开仓价: 82889.4\n止盈: 83825.6",
	} {
		msg := &store.DiscordMessage{Content: body}
		ins := &SourceInterpretation{
			Classification: ClassificationSignal,
			Action:         ActionOpen,
			Symbol:         "BTC/USDT",
			Direction:      DirectionLong,
			ActionEvidence: &ActionEvidence{SourceID: "body", Text: "BTC setup with entry and TP"},
		}
		ApplySourcePolicy(ins, msg, BuildSourceSegments(msg, "default"), "default")
		if skip, detail, _ := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(msg, "default")); skip != SkipSourceEvidence || detail != "current status indicates a closed trade; no explicit new entry instruction" {
			t.Fatalf("closed status card authorized OPEN: body=%q skip=%s detail=%q", body, skip, detail)
		}
	}
}

func TestClosedStatusRecapCannotAuthorizeReduce(t *testing.T) {
	for _, body := range []string{"#BTC 获利了", "#BTC 起飞，赚到了"} {
		ins := &SourceInterpretation{
			Classification: ClassificationSignal,
			Action:         ActionReduce,
			Symbol:         "BTC",
			ActionEvidence: &ActionEvidence{SourceID: "body", Text: body},
		}
		if skip, detail, _ := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(&store.DiscordMessage{Content: body}, "default")); skip != SkipSourceEvidence || detail != "current evidence lacks a management instruction" {
			t.Fatalf("recap authorized REDUCE: body=%q skip=%s detail=%q", body, skip, detail)
		}
	}
}

func TestExplicitOpenAfterClosedStatusCanStartNewTrade(t *testing.T) {
	body := "BTC 交易已平仓\n做多\n進場：市價\n止損：95"
	msg := &store.DiscordMessage{Content: body}
	ins := &SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionOpen,
		Symbol:         "BTC",
		Direction:      DirectionLong,
		ActionEvidence: &ActionEvidence{SourceID: "body", Text: body},
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(ins, BuildSourceSegments(msg, "default")); skip != SkipNone || detail != "" {
		t.Fatalf("explicit new entry after terminal status was blocked: skip=%s detail=%q", skip, detail)
	}
}

func TestTylerPlainTextQuoteCannotAuthorizeOpen(t *testing.T) {
	body := "引用信息：#SOL｜做多 🚀🚀🚀\n\n進場：116.5\n止損：113.5\n\n止盈：118 - 120 - 123\n\n2%～3% 倉位｜50x-40x\n💬 回复：\n#SOL 這單下午6點之前有效 <@&1539559615241523230>"
	msg := &store.DiscordMessage{Content: body}

	tyler := BuildSourceSegments(msg, "tyler_v1")
	if len(tyler) != 2 || tyler[0].Role != "reference" || tyler[1].Role != "current" {
		t.Fatalf("unexpected TYLER source split: %+v", tyler)
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(&SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionOpen,
		Symbol:         "SOL",
		Direction:      DirectionLong,
		ActionEvidence: &ActionEvidence{SourceID: tyler[0].ID, Text: "#SOL｜做多 進場：116.5 止損：113.5"},
	}, tyler); skip != SkipSourceEvidence || detail != "action evidence points to a reference source" {
		t.Fatalf("quoted open was not rejected safely: skip=%s detail=%q", skip, detail)
	}
	if skip, detail, _ := ValidateActionEvidenceDetailed(&SourceInterpretation{
		Classification: ClassificationSignal,
		Action:         ActionOpen,
		Symbol:         "SOL",
		Direction:      DirectionLong,
		ActionEvidence: &ActionEvidence{SourceID: tyler[1].ID, Text: "#SOL 這單下午6點之前有效"},
	}, tyler); skip != SkipSourceEvidence || detail != "current evidence lacks an unambiguous open/add instruction" {
		t.Fatalf("validity-only reply authorized open: skip=%s detail=%q", skip, detail)
	}

	generic := BuildSourceSegments(msg, "default")
	if len(generic) != 1 || generic[0].ID != "body" || generic[0].Role != "current" {
		t.Fatalf("generic source behavior changed: %+v", generic)
	}
}

func TestTylerReduceAndMoveStopToCost(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#STEEM TP1✅ 提醒大家記得減倉上成本 <@&1539559615241523230>"}
	segments := BuildSourceSegments(msg, "tyler_v1")
	got := InterpretKnownSource(msg, segments, "tyler_v1")
	if got == nil {
		t.Fatal("TYLER up-cost instruction was not recognized")
	}
	items := got.Flatten()
	if len(items) != 2 || items[0].Action != ActionReduce || items[1].Action != ActionUpdateSL {
		t.Fatalf("unexpected TYLER actions: %+v", items)
	}
	if items[1].StopLossLevels[0].Price.Type != PriceEntry {
		t.Fatalf("up-cost action did not target entry: %+v", items[1].StopLossLevels)
	}
	for _, item := range items {
		if skip, detail, _ := ValidateActionEvidenceDetailed(item, segments); skip != SkipNone || detail != "" {
			t.Fatalf("TYLER action evidence rejected: action=%s skip=%s detail=%q", item.Action, skip, detail)
		}
	}
}

func TestTylerCostPriceMentionAloneIsNotBreakevenInstruction(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#STEEM 成本价 0.0685"}
	if got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1"); got != nil {
		t.Fatalf("cost-price mention became an action: %+v", got)
	}
}

func TestNeilCardAndJonziEditRemainCurrent(t *testing.T) {
	now := time.Now().UTC()
	for _, body := range []string{"", "Trade update"} {
		msg := &store.DiscordMessage{Content: body, MessageTimestamp: now, EditedAt: &now, EmbedsJSON: fmt.Sprintf(`[{"title":"BTC long","description":"Entry 100, SL 90, TP 110","timestamp":%q,"author":{"name":"Neil"}}]`, now.Add(-10*time.Minute).Format(time.RFC3339))}
		if got := BuildSourceSegments(msg, "default"); got[1].Role != "current" {
			t.Fatalf("timestamp alone classified native card as history: %+v", got)
		}
	}
}
func TestProfilesAreExplicitAndUnknownConditionsStayUnmet(t *testing.T) {
	msg := quotedMessage("#FLOCK 止盈或者減倉")
	if got := InterpretKnownSource(msg, BuildSourceSegments(msg, "default"), "default"); got != nil {
		t.Fatal("TYLER policy leaked into generic follower")
	}
	msg.Content = "#FLOCK 倉位重的可以先跑"
	got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1")
	if got.Classification != ClassificationNeedsContext {
		t.Fatal("subjective condition assumed satisfied")
	}
}
func TestConditionalTP2NotImmediateBreakeven(t *testing.T) {
	msg := quotedMessage("#FLOCK TP2後成本損")
	got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1")
	if got == nil || len(got.ConditionalRules) != 1 || got.ConditionalRules[0].ConditionLevel != 2 {
		t.Fatalf("condition lost: %+v", got)
	}
}
func TestMultiActionClassificationIsolation(t *testing.T) {
	p, err := ParseInterpretation(`{"classification":"SIGNAL","instructions":[{"classification":"SIGNAL","action":"REDUCE","symbol":"FLOCK","close_ratio":50},{"classification":"UNSUPPORTED","action":"OPEN","symbol":"UNKNOWN"},{"action":"REDUCE","symbol":"BTC","close_ratio":200}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Instructions[0].IsActionable() || p.Instructions[1].IsActionable() || p.Instructions[2].Classification != ClassificationAmbiguous {
		t.Fatal("one bad child contaminated others")
	}
}
func TestTargetRootMustBeUniqueAndMatchSide(t *testing.T) {
	st := newTestStore(t)
	e := newReconcileEngine(st, &mockExchange{})
	one := newTestContext(t, st, StateOpen)
	two := *one
	two.ID = "second"
	two.Symbol = "ETHUSDT"
	if err := st.CopyTrade().CreateContext(&two); err != nil {
		t.Fatal(err)
	}
	msg := &store.DiscordMessage{MessageID: "manage", ChannelID: one.ChannelID, MessageTimestamp: time.Now()}
	ins := &SourceInterpretation{TradeReference: TradeReference{RootMessageID: one.RootMessageID}}
	if e.correlateContext(ins, msg, "") != nil {
		t.Fatal("ambiguous root selected first trade")
	}
	if got := e.correlateContext(ins, msg, "ETHUSDT"); got == nil || got.ID != two.ID {
		t.Fatal("root+symbol not resolved")
	}
	ins.Direction = DirectionShort
	if e.correlateContext(ins, msg, "ETHUSDT") != nil {
		t.Fatal("opposite direction resolved")
	}
	ins.Direction = ""
	ins.TradeReference.RootMessageID = "missing"
	if e.correlateContext(ins, msg, "ETHUSDT") != nil {
		t.Fatal("bad explicit reference silently fell back")
	}
}
func TestTPLevelUsesOriginalRecipe(t *testing.T) {
	ctx := &store.CopyTradeContext{TPRecipeJSON: planRecipeJSON(&OpenPlan{TPPrices: []float64{110, 120}, TPOriginalPrices: []float64{120, 110, 130}}), TPPlanJSON: `[{"ordinal":1,"price":999,"filled":true}]`}
	if p := resolveContextPrice(PriceSpec{Type: PriceTPLevel, Level: 1}, ctx, 100); p != 120 {
		t.Fatalf("TP1=%v", p)
	}
	if resolveContextPrice(PriceSpec{Type: PriceTPLevel, Level: 7}, ctx, 100) != 0 {
		t.Fatal("missing TP guessed")
	}
}
func TestCurrentEmbedImagesIncludedHistoricalExcluded(t *testing.T) {
	msg := quotedMessage("#FLOCK 起飛")
	embeds := discord.ParseStoredEmbeds(msg.EmbedsJSON)
	embeds[0].Image = &discord.EmbedMedia{URL: "https://cdn.discordapp.com/quote.png"}
	b, _ := json.Marshal(embeds)
	msg.EmbedsJSON = string(b)
	segments := BuildSourceSegments(msg, "tyler_v1")
	media := MediaSources(msg, segments)
	if len(media) != 1 || media[0].Role != "reference" {
		t.Fatalf("media %+v", media)
	}
	msg.Content = ""
	segments = BuildSourceSegments(msg, "default")
	if media = MediaSources(msg, segments); len(media) != 1 || media[0].Role != "current" {
		t.Fatal("current embed image lost")
	}
}

func TestEvidenceCannotDropQualifyingClause(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#BTC 有補倉的減倉50%\n#ETH 成本損"}
	segments := BuildSourceSegments(msg, "default")
	reduce := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "BTC", ActionEvidence: &ActionEvidence{SourceID: "body", Text: "減倉50%"}}
	stop := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: "ETH", ActionEvidence: &ActionEvidence{SourceID: "body", Text: "成本損"}}
	ApplySourcePolicy(&SourceInterpretation{Instructions: []*SourceInterpretation{reduce, stop}}, msg, segments, "default")
	if !reduce.RequiresAddFill || stop.RequiresAddFill {
		t.Fatal("condition was dropped or leaked into another symbol")
	}
	msg.Content = "#BTC 如果盈利超過10%，減倉50%"
	segments = BuildSourceSegments(msg, "default")
	reduce.RequiresAddFill = false
	if skip, _ := ValidateActionEvidence(reduce, segments); skip != SkipNeedsContext {
		t.Fatal("unverified outer condition was ignored")
	}
}

func TestRecapCannotAuthorizeManagement(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#BTC 起飛，賺到了"}
	ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, ActionEvidence: &ActionEvidence{SourceID: "body", Text: msg.Content}}
	if skip, _ := ValidateActionEvidence(ins, BuildSourceSegments(msg, "default")); skip != SkipSourceEvidence {
		t.Fatal("recap manufactured a reduction")
	}
}

func TestConflictingNativeReplyRejectsModelTarget(t *testing.T) {
	st := newTestStore(t)
	e := newReconcileEngine(st, &mockExchange{})
	a := newTestContext(t, st, StateOpen)
	b := *a
	b.ID, b.RootMessageID, b.Symbol = "other", "other-root", "ETHUSDT"
	if err := st.CopyTrade().CreateContext(&b); err != nil {
		t.Fatal(err)
	}
	msg := &store.DiscordMessage{ChannelID: a.ChannelID, ReplyToMessageID: b.RootMessageID}
	ins := &SourceInterpretation{TradeReference: TradeReference{RootMessageID: a.RootMessageID}}
	if e.correlateContext(ins, msg, a.Symbol) != nil {
		t.Fatal("conflicting reply selected an unrelated trade")
	}
}
