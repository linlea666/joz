package discord

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// allowedMediaHosts is the download allowlist: attachment URLs come from
// message payloads, so restricting hosts closes the SSRF surface.
var allowedMediaHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

// isAllowedMediaURL validates that the URL is HTTPS on an allowed Discord CDN host.
func isAllowedMediaURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && allowedMediaHosts[strings.ToLower(u.Hostname())]
}

// DownloadedImage is a validated, locally cached image ready for a vision model.
type DownloadedImage struct {
	Path     string // local file path
	MimeType string
	SHA256   string
	Size     int64
	Source   string // original URL
}

// MediaDir is where downloaded images are cached (under the data directory).
const MediaDir = "data/discord_media"

// DownloadImage fetches, validates and caches one image.
// Validation order: HTTP ok -> size cap -> MIME sniff (actual bytes, not the
// server header) -> cache by content hash. The reference project saw invalid
// images and upstream 404s break whole signals; callers must degrade to
// text-only parsing with a warning instead of failing the signal.
func DownloadImage(client *Client, rawURL string) (*DownloadedImage, error) {
	if !isAllowedMediaURL(rawURL) {
		return nil, fmt.Errorf("attachment host not allowed (only Discord CDN): %s", rawURL)
	}
	data, _, err := client.DownloadAttachment(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if len(data) < 100 {
		return nil, fmt.Errorf("image too small (%d bytes), likely invalid", len(data))
	}

	// Sniff the real content type from bytes; never trust upstream headers.
	mime := http.DetectContentType(data)
	var ext string
	switch mime {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	default:
		return nil, fmt.Errorf("unsupported image type %q", mime)
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if err := os.MkdirAll(MediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("create media dir: %w", err)
	}
	path := filepath.Join(MediaDir, hash+ext)
	if _, statErr := os.Stat(path); statErr != nil {
		if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
			return nil, fmt.Errorf("cache image: %w", writeErr)
		}
	}

	return &DownloadedImage{
		Path:     path,
		MimeType: mime,
		SHA256:   hash,
		Size:     int64(len(data)),
		Source:   rawURL,
	}, nil
}

// CleanOldMedia deletes cached media files whose modification time is older
// than the retention window. Returns the number of files removed.
func CleanOldMedia(days int) (int, error) {
	entries, err := os.ReadDir(MediaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := timeNow().AddDate(0, 0, -days)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(filepath.Join(MediaDir, entry.Name())); rmErr == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// timeNow is stubbed in tests.
var timeNow = func() time.Time { return time.Now() }

// ReadImageBytes loads a cached image back for base64 encoding.
func ReadImageBytes(img *DownloadedImage) ([]byte, error) {
	if img == nil || strings.TrimSpace(img.Path) == "" {
		return nil, fmt.Errorf("no image path")
	}
	return os.ReadFile(img.Path)
}
