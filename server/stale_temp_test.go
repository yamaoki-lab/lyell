package server

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpen_RemovesStaleBlobTemp(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "blobs", ".tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmp, "put-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	startTestServerIn(t, dir)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("Open の後も古い一時ファイルが残っている (statErr = %v)", err)
	}
}

func TestOpen_FailingToRemoveStaleBlobTempIsLoggedNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ではパーミッションでディレクトリを読めなくできない")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "blobs", ".tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })
	if _, err := os.ReadDir(tmp); err == nil {
		t.Skip("読めなくしたディレクトリを読めてしまう (root で実行している)")
	}

	var logs bytes.Buffer
	s, err := Open(context.Background(), Config{DBPath: filepath.Join(dir, "lyell.db"), BlobDir: filepath.Join(dir, "blobs"), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if !strings.Contains(logs.String(), "一時ファイルを消せなかった") {
		t.Errorf("ログ %q に一時ファイルを消せなかったことが記録されていない", logs.String())
	}
}
