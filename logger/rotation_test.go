package logger

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotationSizeAgeAndPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nofx.log")
	r, err := newRotatingWriter(p, 10, 3, time.Hour)
	require.NoError(t, err)
	defer r.Close()
	for i := 0; i < 5; i++ {
		_, err = r.Write([]byte("12345678\n"))
		require.NoError(t, err)
	}
	files, _ := filepath.Glob(p + "*")
	require.Len(t, files, 3)
	for _, file := range files {
		st, err := os.Stat(file)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), st.Mode().Perm())
	}
	r.opened = time.Now().Add(-2 * time.Hour)
	_, err = r.Write([]byte("age\n"))
	require.NoError(t, err)
}
