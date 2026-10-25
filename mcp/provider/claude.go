// Package provider — ClaudeClient implements the Anthropic Messages API.
//
// Wire-format differences from the OpenAI-compatible base Client:
//
//	┌─────────────────────┬───────────────────────────┬─────────────────────────────────┐
//	│ Concept             │ OpenAI format              │ Anthropic format                │
//	├─────────────────────┼───────────────────────────┼─────────────────────────────────┤
//	│ Endpoint            │ /v1/chat/completions       │ /v1/messages                    │
//	│ Auth header         │ Authorization: Bearer xxx  │ x-api-key: xxx                  │
//	│ System prompt       │ messages[0] role=system    │ top-level "system" field        │
//	│ Tool definition     │ type=function + parameters │ name + description + input_schema│
//	│ Tool choice         │ "auto" (string)            │ {"type":"auto"} (object)        │
//	│ Assistant tool call │ tool_calls array           │ content[{type:tool_use,...}]    │
//	│ Tool result         │ role=tool + tool_call_id   │ role=user content[tool_result]  │
//	│ Max tokens          │ max_tokens                 │ max_tokens (same)               │
//	└─────────────────────┴───────────────────────────┴─────────────────────────────────┘
package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"nofx/mcp"
)

const (
	DefaultClaudeBaseURL = "https://api.anthropic.com/v1"
	DefaultClaudeModel   = "claude-opus-4-6"
)

func init() {
	mcp.RegisterProvider(mcp.ProviderClaude, func(opts ...mcp.ClientOption) mcp.AIClient {
		return NewClaudeClientWithOptions(opts...)
	})
}

// ClaudeClient wraps the base Client and overrides the methods that differ
// for the Anthropic Messages API.  All other behaviour (retry, timeout,
// logging) is inherited unchanged.
type ClaudeClient struct {
	*mcp.Client
}

func (c *ClaudeClient) BaseClient() *mcp.Client { return c.Client }

// NewClaudeClient creates a ClaudeClient with default settings.
func NewClaudeClient() mcp.AIClient {
	return NewClaudeClientWithOptions()
}

// NewClaudeClientWithOptions creates a ClaudeClient with optional overrides.
func NewClaudeClientWithOptions(opts ...mcp.ClientOption) mcp.AIClient {
	baseClient := mcp.NewClient(append([]mcp.ClientOption{
		mcp.WithProvider(mcp.ProviderClaude),
		mcp.WithModel(DefaultClaudeModel),
		mcp.WithBaseURL(DefaultClaudeBaseURL),
	}, opts...)...).(*mcp.Client)

	c := &ClaudeClient{Client: baseClient}
	baseClient.Hooks = c // wire dynamic dispatch to ClaudeClient
	return c
}

// ── Hook overrides ────────────────────────────────────────────────────────────

// SetAPIKey stores credentials and optional custom endpoint / model.
func (c *ClaudeClient) SetAPIKey(apiKey, customURL, customModel string) {
	c.APIKey = apiKey
	if len(apiKey) > 8 {
		c.Log.Infof("🔧 [MCP] Claude API Key: %s...%s", apiKey[:4], apiKey[len(apiKey)-4:])
	}
	if customURL != "" {
		c.BaseURL = customURL
		c.Log.Infof("🔧 [MCP] Claude BaseURL: %s", customURL)
	}
	if customModel != "" {
		c.Model = customModel
		c.Log.Infof("🔧 [MCP] Claude Model: %s", customModel)
	}
}

// SetAuthHeader uses x-api-key instead of Authorization: Bearer.
func (c *ClaudeClient) SetAuthHeader(h http.Header) {
	h.Set("x-api-key", c.APIKey)
	h.Set("anthropic-version", "2023-06-01")
}

// BuildUrl targets /messages instead of /chat/completions.
func (c *ClaudeClient) BuildUrl() string {
	return fmt.Sprintf("%s/messages", c.BaseURL)
}

// BuildMCPRequestBody builds the Anthropic wire format for the simple
// CallWithMessages path (no tool support).
func (c *ClaudeClient) BuildMCPRequestBody(systemPrompt, userPrompt string) map[string]any {
	return map[string]any{
		"model":      c.Model,
		"max_tokens": c.MaxTokens,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}
}

// BuildRequestBodyFromRequest converts a *Request into the Anthropic Messages
// API wire format.
func (c *ClaudeClient) BuildRequestBodyFromRequest(req *mcp.Request) map[string]any {
	// ── 1. Separate system prompt from conversation messages ──────────────────
	var systemPrompt string
	var convMsgs []mcp.Message
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemPrompt = m.Content
		} else {
			convMsgs = append(convMsgs, m)
		}
	}

	// ── 2. Convert messages to Anthropic format ───────────────────────────────
	anthropicMsgs, warnings := convertMessagesToAnthropic(convMsgs)
	for _, w := range warnings {
		if c.Log != nil {
			c.Log.Warnf("⚠️  [MCP] Claude request: %s", w)
		}
	}

	// ── 3. Convert tool definitions (parameters → input_schema) ──────────────
	var anthropicTools []map[string]any
	for _, t := range req.Tools {
		anthropicTools = append(anthropicTools, map[string]any{
			"name":         t.Function.Name,
			"description":  t.Function.Description,
			"input_schema": t.Function.Parameters,
		})
	}

	// ── 4. Assemble request body ──────────────────────────────────────────────
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": c.MaxTokens,
		"system":     systemPrompt,
		"messages":   anthropicMsgs,
	}

	if len(anthropicTools) > 0 {
		body["tools"] = anthropicTools
	}

	// tool_choice: Anthropic uses an object, not a string.
	switch req.ToolChoice {
	case "auto":
		body["tool_choice"] = map[string]any{"type": "auto"}
	case "any":
		body["tool_choice"] = map[string]any{"type": "any"}
	case "none", "":
		// omit — no tool_choice sent
	}

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	return body
}

// ConvertMessagesToAnthropic translates from the OpenAI-shaped mcp.Message
// slice to Anthropic's messages array.
func ConvertMessagesToAnthropic(msgs []mcp.Message) []map[string]any {
	out, _ := convertMessagesToAnthropic(msgs)
	return out
}

// convertMessagesToAnthropic is ConvertMessagesToAnthropic plus the list of
// content parts that could not be represented (for logging by the caller).
func convertMessagesToAnthropic(msgs []mcp.Message) ([]map[string]any, []string) {
	var out []map[string]any
	var warnings []string

	for i := 0; i < len(msgs); {
		msg := msgs[i]

		switch {
		// ── Assistant message carrying tool calls ─────────────────────────────
		case msg.Role == "assistant" && len(msg.ToolCalls) > 0:
			var blocks []map[string]any
			for _, tc := range msg.ToolCalls {
				// Arguments are a JSON string; Claude wants a parsed object.
				var input map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					input = map[string]any{"_raw": tc.Function.Arguments}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			out = append(out, map[string]any{
				"role":    "assistant",
				"content": blocks,
			})
			i++

		// ── Tool result message(s) → single user turn ─────────────────────────
		case msg.Role == "tool":
			// Collect all consecutive tool-result messages.
			var blocks []map[string]any
			for i < len(msgs) && msgs[i].Role == "tool" {
				blocks = append(blocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": msgs[i].ToolCallID,
					"content":     msgs[i].Content,
				})
				i++
			}
			out = append(out, map[string]any{
				"role":    "user",
				"content": blocks,
			})

		// ── Multimodal message (text + images) → content block array ─────────
		// ContentParts takes priority over Content (see mcp.Message). Sending
		// only msg.Content here dropped both the prompt text and the images
		// and Anthropic rejected the request ("user messages must have
		// non-empty content").
		case len(msg.ContentParts) > 0:
			blocks, partWarnings := convertContentPartsToAnthropic(msg.ContentParts, msg.Content)
			warnings = append(warnings, partWarnings...)
			if len(blocks) > 0 {
				out = append(out, map[string]any{
					"role":    msg.Role,
					"content": blocks,
				})
			} else {
				// Every part was empty text: behave exactly like a plain message.
				out = append(out, map[string]any{
					"role":    msg.Role,
					"content": msg.Content,
				})
			}
			i++

		// ── Regular user / assistant text message ─────────────────────────────
		default:
			out = append(out, map[string]any{
				"role":    msg.Role,
				"content": msg.Content,
			})
			i++
		}
	}

	return out, warnings
}

// anthropicImageMediaTypes are the base64 image media types the Messages API
// accepts.
var anthropicImageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// convertContentPartsToAnthropic maps OpenAI-style content parts to Anthropic
// content blocks. Empty text parts are dropped (the API rejects empty text
// blocks). An image that cannot be represented is replaced by an explicit text
// note, so the model is told an image existed but was not visible and the
// message never silently loses its image or becomes empty. When the parts carry
// no text at all, a non-empty fallback Content is used as the leading text.
func convertContentPartsToAnthropic(parts []mcp.ContentPart, fallback string) ([]map[string]any, []string) {
	var blocks []map[string]any
	var warnings []string
	hasText := false
	for idx, p := range parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			hasText = true
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			url := ""
			if p.ImageURL != nil {
				url = strings.TrimSpace(p.ImageURL.URL)
			}
			source, err := anthropicImageSource(url)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("content part %d: image omitted: %v", idx, err))
				hasText = true
				blocks = append(blocks, map[string]any{"type": "text", "text": "[image omitted: " + err.Error() + "]"})
				continue
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": source})
		default:
			if strings.TrimSpace(p.Text) != "" {
				hasText = true
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				continue
			}
			warnings = append(warnings, fmt.Sprintf("content part %d: unsupported type %q omitted", idx, p.Type))
			hasText = true
			blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprintf("[content omitted: unsupported part type %q]", p.Type)})
		}
	}
	if !hasText && strings.TrimSpace(fallback) != "" {
		blocks = append([]map[string]any{{"type": "text", "text": fallback}}, blocks...)
	}
	return blocks, warnings
}

// anthropicImageSource converts a data URL or http(s) URL into an Anthropic
// image source object.
func anthropicImageSource(url string) (map[string]any, error) {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "data:"):
		comma := strings.IndexByte(url, ',')
		if comma < 0 {
			return nil, fmt.Errorf("malformed data URL")
		}
		header, data := url[len("data:"):comma], strings.TrimSpace(url[comma+1:])
		params := strings.Split(header, ";")
		mediaType := strings.ToLower(strings.TrimSpace(params[0]))
		isBase64 := false
		for _, param := range params[1:] {
			if strings.EqualFold(strings.TrimSpace(param), "base64") {
				isBase64 = true
			}
		}
		if !isBase64 {
			return nil, fmt.Errorf("data URL is not base64 encoded")
		}
		if !anthropicImageMediaTypes[mediaType] {
			return nil, fmt.Errorf("unsupported image media type %q", mediaType)
		}
		if data == "" {
			return nil, fmt.Errorf("empty image data")
		}
		return map[string]any{"type": "base64", "media_type": mediaType, "data": data}, nil
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return map[string]any{"type": "url", "url": url}, nil
	case url == "":
		return nil, fmt.Errorf("missing image URL")
	default:
		return nil, fmt.Errorf("unsupported image URL scheme")
	}
}

// ── Response parsers ──────────────────────────────────────────────────────────

// ParseMCPResponse extracts the plain-text reply from an Anthropic response.
func (c *ClaudeClient) ParseMCPResponse(body []byte) (string, error) {
	r, err := c.ParseMCPResponseFull(body)
	if err != nil {
		return "", err
	}
	return r.Content, nil
}

// ParseMCPResponseFull extracts both text and tool calls from an Anthropic
// response envelope.
func (c *ClaudeClient) ParseMCPResponseFull(body []byte) (*mcp.LLMResponse, error) {
	var raw struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			ID    string          `json:"id,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse Anthropic response: %w — body: %s", err, body)
	}
	if raw.Error != nil {
		return nil, fmt.Errorf("Anthropic API error: %s — %s", raw.Error.Type, raw.Error.Message)
	}

	total := raw.Usage.InputTokens + raw.Usage.OutputTokens
	if mcp.TokenUsageCallback != nil && total > 0 {
		mcp.TokenUsageCallback(mcp.TokenUsage{
			Provider:         c.Provider,
			Model:            c.Model,
			PromptTokens:     raw.Usage.InputTokens,
			CompletionTokens: raw.Usage.OutputTokens,
			TotalTokens:      total,
		})
	}

	result := &mcp.LLMResponse{}
	for _, block := range raw.Content {
		switch block.Type {
		case "text":
			result.Content = block.Text

		case "tool_use":
			// Input is a JSON object; serialise back to a JSON string so it
			// matches the ToolCallFunction.Arguments field (always a string).
			argsJSON, err := json.Marshal(block.Input)
			if err != nil {
				argsJSON = []byte("{}")
			}
			result.ToolCalls = append(result.ToolCalls, mcp.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: mcp.ToolCallFunction{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}
	return result, nil
}
