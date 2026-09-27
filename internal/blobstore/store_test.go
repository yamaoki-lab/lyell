package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingBackend は, Backend.Put が呼ばれた回数を数える.
type countingBackend struct {
	Backend
	puts int
}

func (b *countingBackend) Put(ctx context.Context, sum Sum, data io.Reader) error {
	b.puts++
	return b.Backend.Put(ctx, sum, data)
}

func TestStore_PutThenOpenRoundTrips(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
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

func TestStore_PutMismatchPlacesNothing(t *testing.T) {
	root := t.TempDir()
	local := NewLocalStore(root)
	store := New(local)
	ctx := context.Background()

	err := store.Put(ctx, sumOf("hello"), strings.NewReader("goodbye"))
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Put() error = %v, want *MismatchError", err)
	}
	if mismatch.Want != sumOf("hello") || mismatch.Got != sumOf("goodbye") {
		t.Errorf("MismatchError = {Want: %s, Got: %s}, want {Want: %s, Got: %s}", mismatch.Want, mismatch.Got, sumOf("hello"), sumOf("goodbye"))
	}

	if _, statErr := os.Stat(local.path(sumOf("hello"))); !os.IsNotExist(statErr) {
		t.Errorf("合わない中身が置かれている (statErr = %v), want os.ErrNotExist", statErr)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".tmp"))
	if err != nil {
		t.Fatalf("ReadDir(.tmp) error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf(".tmp の下に%d件のファイルが残っている, want 0", len(entries))
	}
}

func TestStore_PutExistingVerifiesWithoutWriting(t *testing.T) {
	backend := &countingBackend{Backend: NewLocalStore(t.TempDir())}
	store := New(backend)
	ctx := context.Background()

	if err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	// 合う中身なら成功するが, 置き場には書かない.
	if err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("2回目の Put() error = %v, want nil", err)
	}
	if backend.puts != 1 {
		t.Errorf("Backend.Put の回数 = %d, want 1", backend.puts)
	}

	// 既にあっても, 合わない中身は断る.
	err := store.Put(ctx, sumOf("hello"), strings.NewReader("goodbye"))
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("合わない中身での Put() error = %v, want *MismatchError", err)
	}
}

func TestStore_PutPropagatesReadError(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))

	err := store.Put(context.Background(), sumOf("partial"), &failingReader{remaining: []byte("partial")})
	if err == nil {
		t.Fatalf("Put() error = nil, want error")
	}
	var mismatch *MismatchError
	if errors.As(err, &mismatch) {
		t.Errorf("Put() error = %v, want 読み取りのエラーのまま (MismatchError ではない)", err)
	}
}
