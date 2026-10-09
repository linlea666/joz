package discord

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxAttachmentBytes = 8 << 20

type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("discord status %d: %s", e.StatusCode, e.Body)
}
func IsAuthError(err error) bool { e, ok := err.(*StatusError); return ok && e.StatusCode == 401 }
func IsNotFound(err error) bool  { e, ok := err.(*StatusError); return ok && e.StatusCode == 404 }

// Client is an RPC facade. The collector is the sole owner of authenticated
// Discord requests. Media downloads never carry the personal credential.
type Client struct {
	rpc func(string, interface{}, interface{}) error
}

func (c *Client) GetCurrentUser() (*User, error) { return c.TestToken("") }
func (c *Client) TestToken(token string) (*User, error) {
	var u User
	err := c.rpc("test", map[string]string{"token": token}, &u)
	return &u, err
}
func (c *Client) GetMessages(channelID string, limit int) ([]*Message, error) {
	var out []*Message
	err := c.rpc("preview", map[string]interface{}{"channel_id": channelID, "limit": limit}, &out)
	return out, err
}
func (c *Client) GetMessage(channelID, messageID string) (*Message, error) {
	var out Message
	err := c.rpc("message", map[string]string{"channel_id": channelID, "message_id": messageID}, &out)
	return &out, err
}
func (c *Client) DownloadAttachment(rawURL string) ([]byte, string, error) {
	if !isAllowedMediaURL(rawURL) {
		return nil, "", fmt.Errorf("media host not allowed")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || !isAllowedMediaURL(req.URL.String()) {
			return fmt.Errorf("media redirect denied")
		}
		return nil
	}}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("media HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxAttachmentBytes {
		return nil, "", fmt.Errorf("media exceeds size limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}
