package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"nofx/logger"
	"strconv"
	"sync"
	"time"
)

const apiBase = "https://discord.com/api/v10"

// maxAttachmentBytes caps image downloads (charts are ~200KB; 8MB is generous).
const maxAttachmentBytes = 8 << 20

// StatusError carries the HTTP status of a failed Discord API call so the
// poller can distinguish auth failures (401/403) from transient errors.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("discord API status %d: %s", e.StatusCode, e.Body)
}

// IsAuthError reports token-invalid / no-permission failures.
func IsAuthError(err error) bool {
	if se, ok := err.(*StatusError); ok {
		return se.StatusCode == 401 || se.StatusCode == 403
	}
	return false
}

// IsNotFound reports 404 (channel deleted / no access).
func IsNotFound(err error) bool {
	if se, ok := err.(*StatusError); ok {
		return se.StatusCode == 404
	}
	return false
}

// Client is a conservative Discord REST client.
// ALL requests across all channels go through one serial queue (mu) and honor
// rate-limit headers, so the process can never burst against Discord.
type Client struct {
	token      string
	httpClient *http.Client

	mu          sync.Mutex
	rateResetAt time.Time // do not send before this time
}

// NewClient creates a client for a personal account token
// (Authorization header carries the raw token, no "Bot " prefix).
func NewClient(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// do executes one API request through the serial queue with rate-limit
// compliance and a single retry on 429.
func (c *Client) do(method, path string, out interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < 3; attempt++ {
		if wait := time.Until(c.rateResetAt); wait > 0 {
			time.Sleep(wait)
		}

		req, err := http.NewRequest(method, apiBase+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", c.token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}

		c.updateRateLimit(resp)

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			retryAfter := parseRetryAfter(resp, body)
			logger.Warnf("[Discord] rate limited, backing off %.1fs (attempt %d)", retryAfter.Seconds(), attempt+1)
			c.rateResetAt = time.Now().Add(retryAfter)
			continue
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out == nil {
				return nil
			}
			return json.Unmarshal(body, out)
		default:
			return &StatusError{StatusCode: resp.StatusCode, Body: truncate(string(body), 300)}
		}
	}
	return fmt.Errorf("discord API rate limited after retries")
}

// updateRateLimit records the per-route bucket state; when the bucket is
// exhausted we wait for its reset before the next request.
func (c *Client) updateRateLimit(resp *http.Response) {
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	resetAfter := resp.Header.Get("X-RateLimit-Reset-After")
	if remaining == "" || resetAfter == "" {
		return
	}
	rem, err1 := strconv.Atoi(remaining)
	after, err2 := strconv.ParseFloat(resetAfter, 64)
	if err1 != nil || err2 != nil {
		return
	}
	if rem <= 0 {
		c.rateResetAt = time.Now().Add(time.Duration(after*1000) * time.Millisecond)
	}
}

func parseRetryAfter(resp *http.Response, body []byte) time.Duration {
	// Prefer the JSON body's retry_after (seconds, may be fractional).
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter > 0 {
		return time.Duration(payload.RetryAfter*1000)*time.Millisecond + 500*time.Millisecond
	}
	if h := resp.Header.Get("Retry-After"); h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil {
			return time.Duration(secs*1000)*time.Millisecond + 500*time.Millisecond
		}
	}
	return 5 * time.Second
}

// GetCurrentUser validates the token (`GET /users/@me`).
func (c *Client) GetCurrentUser() (*User, error) {
	var u User
	if err := c.do(http.MethodGet, "/users/@me", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetMessages fetches the most recent messages of a channel (newest first).
func (c *Client) GetMessages(channelID string, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var msgs []*Message
	path := fmt.Sprintf("/channels/%s/messages?limit=%d", url.PathEscape(channelID), limit)
	if err := c.do(http.MethodGet, path, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// GetMessage fetches a single message (reply / linked-message context).
func (c *Client) GetMessage(channelID, messageID string) (*Message, error) {
	var msg Message
	path := fmt.Sprintf("/channels/%s/messages/%s", url.PathEscape(channelID), url.PathEscape(messageID))
	if err := c.do(http.MethodGet, path, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// DownloadAttachment fetches an attachment/embed image. Attachment URLs are
// signed with expiry parameters, so downloads must happen promptly after the
// message is received. Returns the raw bytes and the response content type.
func (c *Client) DownloadAttachment(rawURL string) ([]byte, string, error) {
	// CDN downloads don't count against the API rate limits, but user-token
	// signed attachment URLs often 401 without the same Authorization header
	// used for the REST API. Reuse the client token.
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &StatusError{StatusCode: resp.StatusCode, Body: "attachment download failed"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxAttachmentBytes {
		return nil, "", fmt.Errorf("attachment exceeds %d bytes", maxAttachmentBytes)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
