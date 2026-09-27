package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// blockingBackend は, block の Sum への Put の途中で, release が閉じられるまで止まる.
type blockingBackend struct {
	countingBackend
	block   Sum
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBackend) Put(ctx context.Context, sum Sum, data io.Reader) error {
	if sum == b.block {
		close(b.entered)
		<-b.release
	}
	return b.countingBackend.Put(ctx, sum, data)
}

func newBlockingBackend(t *testing.T, block Sum) *blockingBackend {
	return &blockingBackend{
		countingBackend: countingBackend{Backend: NewLocalStore(t.TempDir())},
		block:           block,
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
}

// waitForWaiters は, sum の順番待ちに並んでいる数 (使っている1つを含む) が n になるまで待つ.
func waitForWaiters(t *testing.T, store *Store, sum Sum, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		store.locks.mu.Lock()
		e := store.locks.entries[sum]
		refs := 0
		if e != nil {
			refs = e.refs
		}
		store.locks.mu.Unlock()
		if refs == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s の順番待ちが %d にならなかった", sum, n)
}

func TestStore_ConcurrentPutsOfSameSumWriteOnce(t *testing.T) {
	backend := newBlockingBackend(t, sumOf("hello"))
	store := New(backend)
	ctx := context.Background()

	errs := make(chan error, 2)
	go func() { errs <- store.Put(ctx, sumOf("hello"), strings.NewReader("hello")) }()
	<-backend.entered
	go func() { errs <- store.Put(ctx, sumOf("hello"), strings.NewReader("hello")) }()
	// 2つ目の Put が, 1つ目の書き込みの途中で待っている状態を作る.
	waitForWaiters(t, store, sumOf("hello"), 2)
	close(backend.release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("Put() error = %v, want nil", err)
		}
	}
	if backend.puts != 1 {
		t.Errorf("Backend.Put の回数 = %d, want 1", backend.puts)
	}
	if n := len(store.locks.entries); n != 0 {
		t.Errorf("終わった後の順番待ちの数 = %d, want 0", n)
	}
}

func TestStore_PutsOfDifferentSumsDoNotWait(t *testing.T) {
	backend := newBlockingBackend(t, sumOf("hello"))
	store := New(backend)
	ctx := context.Background()

	errs := make(chan error, 1)
	go func() { errs <- store.Put(ctx, sumOf("hello"), strings.NewReader("hello")) }()
	<-backend.entered

	// "hello" の書き込みが止まっている間でも, 違う名前の Put は終わる.
	if err := store.Put(ctx, sumOf("world"), strings.NewReader("world")); err != nil {
		t.Errorf("Put(world) error = %v, want nil", err)
	}

	close(backend.release)
	if err := <-errs; err != nil {
		t.Errorf("Put(hello) error = %v, want nil", err)
	}
}

func TestSumLocks_WaitingCanBeCanceled(t *testing.T) {
	var locks sumLocks
	unlock, err := locks.lock(context.Background(), sumOf("hello"))
	if err != nil {
		t.Fatalf("lock() error = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.lock(ctx, sumOf("hello")); !errors.Is(err, context.Canceled) {
		t.Errorf("取り消した ctx での lock() error = %v, want context.Canceled", err)
	}

	unlock()
	if n := len(locks.entries); n != 0 {
		t.Errorf("終わった後の順番待ちの数 = %d, want 0", n)
	}
}
