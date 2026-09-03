package discord

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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

// ReadImageBytes loads a cached image back for base64 encoding.
func ReadImageBytes(img *DownloadedImage) ([]byte, error) {
	if img == nil || strings.TrimSpace(img.Path) == "" {
		return nil, fmt.Errorf("no image path")
	}
	return os.ReadFile(img.Path)
}
