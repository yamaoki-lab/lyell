package blobstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalStore_RemoveStaleTempRemovesOnlyOldPutFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(filepath.Join(dir, "put-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 閾値 (24時間) の前後1時間で, 消すものと残すものを分ける.
	old := time.Now().Add(-25 * time.Hour)
	recent := time.Now().Add(-23 * time.Hour)
	files := map[string]time.Time{
		"put-old":    old,    // 消す
		"put-recent": recent, // 別のプロセスが書いている途中かもしれないので残す
		"other-old":  old,    // Put の一時ファイルではないので残す
	}
	for name, mtime := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 落ちた Put の一時ファイルは読み取り専用になっていることがある.
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "put-dir"), old, old); err != nil {
		t.Fatal(err)
	}

	if err := NewLocalStore(root).RemoveStaleTemp(); err != nil {
		t.Fatalf("RemoveStaleTemp() error = %v, want nil", err)
	}
	for _, name := range []string{"put-old", "put-recent", "other-old", "put-dir"} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if exists, want := err == nil, name != "put-old"; exists != want {
			t.Errorf("%s が残っている = %v, want %v (statErr = %v)", name, exists, want, err)
		}
	}
	for name := range files {
		_ = os.Chmod(filepath.Join(dir, name), 0o644)
	}
}

func TestLocalStore_RemoveStaleTempWithoutTempDirSucceeds(t *testing.T) {
	if err := NewLocalStore(t.TempDir()).RemoveStaleTemp(); err != nil {
		t.Errorf("RemoveStaleTemp() error = %v, want nil", err)
	}
}
