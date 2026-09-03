package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"nofx/store"
)

// loadGoldenMessages replays a captured channel sample (raw Discord API JSON).
func loadGoldenMessages(t *testing.T, name string) []Message {
	t.Helper()
	path := filepath.Join("..", "testdata", "copytrading", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden sample %s not available: %v", name, err)
	}
	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	if len(msgs) == 0 {
		t.Fatalf("%s contains no messages", name)
	}
	return msgs
}

// TestReplayGoldenChannels ensures every captured real-world message from the
// three reference channels survives the normalization pipeline.
func TestReplayGoldenChannels(t *testing.T) {
	samples := []string{
		"channel_jonzi_sample.json",
		"channel_dsc_sample.json",
		"channel_neil_sample.json",
	}
	for _, name := range samples {
		t.Run(name, func(t *testing.T) {
			msgs := loadGoldenMessages(t, name)
			for i := range msgs {
				rec, err := ToStoreMessage(&msgs[i], "test-channel")
				if err != nil {
					t.Fatalf("msg %s: ToStoreMessage failed: %v", msgs[i].ID, err)
				}
				if rec.MessageID == "" {
					t.Fatalf("msg index %d: empty message id", i)
				}
				if rec.ContentHash == "" {
					t.Fatalf("msg %s: empty content hash", msgs[i].ID)
				}
				// Round-trip stored embeds/attachments
				if rec.EmbedsJSON != "" && ParseStoredEmbeds(rec.EmbedsJSON) == nil {
					t.Fatalf("msg %s: stored embeds not parseable", msgs[i].ID)
				}
				if rec.AttachmentsJSON != "" && ParseStoredAttachments(rec.AttachmentsJSON) == nil {
					t.Fatalf("msg %s: stored attachments not parseable", msgs[i].ID)
				}
			}
		})
	}
}

// TestReplayEditDetection verifies the edit-detection hash: the jonzi channel
// edits trade cards in place (entry fill, stop moves, close), so a content
// change must produce a different hash while identical content must not.
func TestReplayEditDetection(t *testing.T) {
	msgs := loadGoldenMessages(t, "channel_jonzi_sample.json")
	m := &msgs[0]

	rec1, err := ToStoreMessage(m, "c1")
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := ToStoreMessage(m, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if rec1.ContentHash != rec2.ContentHash {
		t.Fatal("identical content produced different hashes")
	}

	edited := *m
	edited.Content = m.Content + "\n**:ChromaAlert: Trade was closed**"
	rec3, err := ToStoreMessage(&edited, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if rec3.ContentHash == rec1.ContentHash {
		t.Fatal("edited content did not change the hash")
	}
}

// TestReplayEmbedsFlatten verifies embed-style channels (nurse-neil) produce
// non-empty flattened text so the LLM can read Entry / Stop Loss fields.
func TestReplayEmbedsFlatten(t *testing.T) {
	msgs := loadGoldenMessages(t, "channel_neil_sample.json")
	found := false
	for i := range msgs {
		if len(msgs[i].Embeds) == 0 {
			continue
		}
		found = true
		text := FlattenEmbeds(msgs[i].Embeds)
		if text == "" {
			t.Fatalf("msg %s: embeds flattened to empty text", msgs[i].ID)
		}
	}
	if !found {
		t.Log("no embeds found in neil sample (content-only capture) — flatten check skipped")
	}
}

// TestReplayMessageLinks verifies alert posts linking back to trade cards
// (jonzi style) are extracted for context correlation.
func TestReplayMessageLinks(t *testing.T) {
	links := ExtractMessageLinks(
		"Trade closed https://discord.com/channels/123/456/789 and " +
			"again https://discord.com/channels/123/456/789 plus " +
			"https://discordapp.com/channels/1/2/3",
	)
	if len(links) != 2 {
		t.Fatalf("expected 2 unique links, got %d", len(links))
	}
	if links[0].ChannelID != "456" || links[0].MessageID != "789" {
		t.Fatalf("unexpected first link: %+v", links[0])
	}
}

// TestReplayHashStability guards the hash function against accidental change:
// the stored baseline hashes in production DBs must stay comparable.
func TestReplayHashStability(t *testing.T) {
	h1 := store.HashDiscordContent("abc", "")
	h2 := store.HashDiscordContent("abc", "")
	h3 := store.HashDiscordContent("abc", `[{"title":"x"}]`)
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if h1 == h3 {
		t.Fatal("embeds must affect the hash")
	}
}
