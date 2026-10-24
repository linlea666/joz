package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"nofx/mcp"
)

func claudeBodyJSON(t *testing.T, msgs ...mcp.Message) map[string]any {
	t.Helper()
	client := NewClaudeClientWithOptions(mcp.WithLogger(mcp.NewNoopLogger())).(*ClaudeClient)
	body := client.BuildRequestBodyFromRequest(&mcp.Request{Model: "claude-test", Messages: msgs})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestClaudeMultimodalMessageKeepsTextAndBase64Image(t *testing.T) {
	prompt := "Interpret this trade card"
	body := claudeBodyJSON(t,
		mcp.NewSystemMessage("system rules"),
		mcp.NewMultimodalUserMessage(
			mcp.NewTextPart(prompt),
			mcp.NewTextPart("Current image source_id=1:image"),
			mcp.NewImagePart("data:image/png;base64,iVBORw0KGgo="),
		),
	)
	if body["system"] != "system rules" {
		t.Fatalf("system prompt lost: %v", body["system"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role=%v", msg["role"])
	}
	blocks, ok := msg["content"].([]any)
	if !ok || len(blocks) != 3 {
		t.Fatalf("content must be a 3-block array, got %#v", msg["content"])
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != prompt {
		t.Fatalf("prompt text lost: %#v", first)
	}
	second := blocks[1].(map[string]any)
	if second["type"] != "text" || second["text"] != "Current image source_id=1:image" {
		t.Fatalf("image label lost: %#v", second)
	}
	img := blocks[2].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("image block type=%v", img["type"])
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "iVBORw0KGgo=" {
		t.Fatalf("bad image source: %#v", src)
	}
}

func TestClaudeMultimodalHTTPImageUsesURLSource(t *testing.T) {
	body := claudeBodyJSON(t, mcp.NewMultimodalUserMessage(
		mcp.NewTextPart("look"),
		mcp.NewImagePart("https://cdn.discordapp.com/a.png"),
	))
	blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	src := blocks[1].(map[string]any)["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "https://cdn.discordapp.com/a.png" {
		t.Fatalf("bad url source: %#v", src)
	}
}

func TestClaudeMultimodalUnsupportedImageNeverYieldsEmptyContent(t *testing.T) {
	for _, url := range []string{"", "data:image/bmp;base64,AAAA", "data:image/png,notbase64", "ftp://x/y.png", "data:image/png;base64,"} {
		body := claudeBodyJSON(t, mcp.NewMultimodalUserMessage(mcp.NewImagePart(url)))
		blocks, ok := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if !ok || len(blocks) != 1 {
			t.Fatalf("%q: content=%#v", url, body["messages"])
		}
		b := blocks[0].(map[string]any)
		text, _ := b["text"].(string)
		if b["type"] != "text" || !strings.HasPrefix(text, "[image omitted") {
			t.Fatalf("%q: unsupported image must become an explicit note, got %#v", url, b)
		}
	}
}

func TestClaudeMultimodalFallsBackToContentWhenPartsHaveNoText(t *testing.T) {
	msg := mcp.NewMultimodalUserMessage(mcp.NewTextPart("  "), mcp.NewImagePart("data:image/jpeg;base64,/9j/4AAQ"))
	msg.Content = "fallback prompt"
	body := claudeBodyJSON(t, msg)
	blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["text"] != "fallback prompt" || blocks[1].(map[string]any)["type"] != "image" {
		t.Fatalf("blocks=%#v", blocks)
	}
}

func TestClaudePlainTextMessagesUnchanged(t *testing.T) {
	body := claudeBodyJSON(t,
		mcp.NewSystemMessage("sys"),
		mcp.NewUserMessage("hello"),
		mcp.NewAssistantMessage("hi"),
	)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("user=%#v", m)
	}
	if m := msgs[1].(map[string]any); m["role"] != "assistant" || m["content"] != "hi" {
		t.Fatalf("assistant=%#v", m)
	}
}
