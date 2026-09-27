package blobstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sumOf(s string) Sum {
	return sha256.Sum256([]byte(s))
}

func TestLocalStore_PutThenOpenRoundTrips(t *testing.T) {
	store := NewLocalStore(t.TempDir())
	ctx := context.Background()

	if err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	r, err := store.Open(ctx, sumOf("hello"))
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestLocalStore_ExistsReflectsPut(t *testing.T) {
	store := NewLocalStore(t.TempDir())
	ctx := context.Background()

	ok, err := store.Exists(ctx, sumOf("hello"))
	if err != nil {
		t.Fatalf("Exists() error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("Exists() = true before Put, want false")
	}

	if err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	ok, err = store.Exists(ctx, sumOf("hello"))
	if err != nil {
		t.Fatalf("Exists() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Exists() = false after Put, want true")
	}
}

func TestLocalStore_OpenMissingBlobErrors(t *testing.T) {
	store := NewLocalStore(t.TempDir())
	if _, err := store.Open(context.Background(), sumOf("does-not-exist")); err == nil {
		t.Fatalf("Open() error = nil, want error")
	}
}

func TestLocalStore_PutPlacesBlobUnderSHA256Dir(t *testing.T) {
	root := t.TempDir()
	store := NewLocalStore(root)

	if err := store.Put(context.Background(), sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	// "hello" の SHA-256 は 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824.
	want := filepath.Join(root, "sha256", "2c", "f2", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("Stat(%s) error = %v, want nil", want, err)
	}
}

// failingReader は, remaining の分は読めるが, その後は必ずエラーになる io.Reader. 書き込みの途中で失敗した状況を再現する.
type failingReader struct {
	remaining []byte
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, errors.New("simulated write failure")
	}
	n := copy(p, r.remaining)
	r.remaining = r.remaining[n:]
	return n, nil
}

func TestLocalStore_Put_FailureLeavesNoPartialFileAtFinalPath(t *testing.T) {
	root := t.TempDir()
	store := NewLocalStore(root)
	ctx := context.Background()

	sum := sumOf("partial")
	err := store.Put(ctx, sum, &failingReader{remaining: []byte("partial")})
	if err == nil {
		t.Fatalf("Put() error = nil, want error")
	}

	if _, statErr := os.Stat(store.path(sum)); !os.IsNotExist(statErr) {
		t.Errorf("final path exists after failed Put (statErr = %v), want os.ErrNotExist", statErr)
	}

	// 一時的な置き場にゴミが残っても実害は無いが, 後片付けまでされていることも確かめる.
	entries, err := os.ReadDir(filepath.Join(root, ".tmp"))
	if err != nil {
		t.Fatalf("ReadDir(.tmp) error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf(".tmp配下に%d件のファイルが残っている, want 0", len(entries))
	}
}

func TestMkdirAllDurable_CreatesNestedDirsAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")

	for i := range 2 {
		if err := mkdirAllDurable(dir); err != nil {
			t.Fatalf("mkdirAllDurable() (%d回目) error = %v, want nil", i+1, err)
		}
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v, want nil", err)
	}
	if !info.IsDir() {
		t.Errorf("%s がディレクトリではない", dir)
	}
}

func TestMkdirAllDurable_FileInTheWayErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v, want nil", err)
	}

	if err := mkdirAllDurable(filepath.Join(root, "a", "b")); err == nil {
		t.Fatalf("mkdirAllDurable() error = nil, want error")
	}
}

func TestSyncDir_ExistingDirSucceeds(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir() error = %v, want nil", err)
	}
}
