package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// rotatingWriter caps text output independently of structured audit retention.
// Includes the active file in maxFiles; permissions never expose credentials.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	maxFiles int
	maxAge   time.Duration
	opened   time.Time
}

func newRotatingWriter(path string, size int64, files int, age time.Duration) (*rotatingWriter, error) {
	r := &rotatingWriter{path: path, maxBytes: size, maxFiles: files, maxAge: age}
	return r, r.open()
}
func (r *rotatingWriter) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.file = f
	r.size = stat.Size()
	r.opened = stat.ModTime()
	r.prune()
	return nil
}
func (r *rotatingWriter) prune() {
	// Include legacy nofx_YYYY-MM-DD.log files; never touch arbitrary data files.
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(r.path), "nofx_*.log"))
	for i := 1; i < r.maxFiles; i++ {
		paths = append(paths, fmt.Sprintf("%s.%d", r.path, i))
	}
	for _, p := range paths {
		if st, e := os.Stat(p); e == nil && time.Since(st.ModTime()) > r.maxAge {
			_ = os.Remove(p)
		}
	}
}
func (r *rotatingWriter) rotate() error {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			return err
		}
		r.file = nil
	}
	for i := r.maxFiles - 1; i >= 1; i-- {
		dst := fmt.Sprintf("%s.%d", r.path, i)
		src := r.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", r.path, i-1)
		}
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return r.open()
}
func (r *rotatingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > r.maxBytes || time.Since(r.opened) > r.maxAge || r.opened.UTC().Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}
func (r *rotatingWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
