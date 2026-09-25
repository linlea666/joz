package copytrader

import (
	"encoding/json"
	"os"
	"testing"

	"nofx/discord"
	"nofx/store"
)

// Responses are protocol fixtures, not claims about historical model output or
// venue fills. The dry-run interpreter is checked for complete write isolation.
func TestSanitizedTylerFixtures(t *testing.T) {
	b, err := os.ReadFile("../testdata/copytrading/tyler_regressions.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples []struct {
		marketReferenceFixture
		ExpectedAction    Action        `json:"expected_action"`
		ExpectedEntryType EntryPlanType `json:"expected_entry_type"`
		RequiresAddFill   bool          `json:"requires_add_fill"`
	}
	if err = json.Unmarshal(b, &samples); err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		t.Run(sample.Name, func(t *testing.T) {
			e, venue, _, msg := fixtureEngine(t, sample.marketReferenceFixture, .2)
			e.cfg.InterpretationProfile = "tyler_v1"
			item := e.replayOne(msg)
			if item.Error != "" || item.Classification != string(sample.ExpectedClassification) || item.Action != string(sample.ExpectedAction) {
				t.Fatalf("fixture interpretation: %+v", item)
			}
			var parsed SourceInterpretation
			if err := json.Unmarshal([]byte(item.ParsedJSON), &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed.RequiresAddFill != sample.RequiresAddFill {
				t.Fatal("conditional eligibility lost")
			}
			if sample.ExpectedAction == ActionOpen {
				for _, threshold := range []float64{.2, 5} {
					decision, err := decideEntry(parsed.Direction, parsed.EntryOrders[0].Price, sample.MarketPrice, threshold, true)
					if err != nil || decision.OrderType != sample.ExpectedEntryType {
						t.Fatalf("price rules changed at threshold %g: %+v %v", threshold, decision, err)
					}
				}
			}
			if sample.ExpectedAction == ActionReduce && (parsed.CloseRatio == nil || *parsed.CloseRatio != 50) {
				t.Fatal("TYLER reduction not half remaining")
			}
			if venue.marketEntries != 0 || len(venue.limitEntries) > 0 {
				t.Fatal("replay placed orders")
			}
			for _, model := range []interface{}{&store.CopyTradeSignal{}, &store.CopyTradeAIRun{}, &store.CopyTradeContext{}, &store.CopyTradeEvent{}, &store.CopyTradeAction{}, &store.CopyTradeOrder{}, &store.DiscordMessage{}} {
				var n int64
				if err := e.st.GormDB().Model(model).Count(&n).Error; err != nil || n != 0 {
					t.Fatalf("replay wrote %T: %d %v", model, n, err)
				}
			}
		})
	}
}

func TestExistingChannelCardsRemainCurrent(t *testing.T) {
	for _, sample := range []struct {
		channel string
		index   int
	}{{"neil", 1}, {"jonzi", 0}, {"dsc", 3}} {
		t.Run(sample.channel, func(t *testing.T) {
			b, err := os.ReadFile("../testdata/copytrading/channel_" + sample.channel + "_sample.json")
			if err != nil {
				t.Fatal(err)
			}
			var messages []discord.Message
			if err = json.Unmarshal(b, &messages); err != nil {
				t.Fatal(err)
			}
			msg, err := discord.ToStoreMessage(&messages[sample.index], "test-channel")
			if err != nil {
				t.Fatal(err)
			}
			segments := BuildSourceSegments(msg, "default")
			for _, s := range segments {
				if s.Role != "current" {
					t.Fatalf("native %s card classified as quote: %+v", sample.channel, s)
				}
			}
			if InterpretKnownSource(msg, segments, "default") != nil {
				t.Fatal("TYLER policy leaked into another channel")
			}
		})
	}
}
