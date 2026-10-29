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

func (b *countingBackend) Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) error {
	b.puts++
	return b.Backend.Put(ctx, sum, data, opts...)
}

func TestStore_PutThenOpenRoundTrips(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
	ctx := context.Background()

	if _, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	r, err := store.Open(ctx, sumOf("hello"))
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestStore_SizeAndExistsReflectPut(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
	ctx := context.Background()

	// 存在しないことはエラーではなく false. 保存先を読み取れない時のエラーとは区別する.
	if ok, err := store.Exists(ctx, sumOf("hello")); err != nil || ok {
		t.Fatalf("Put の前の Exists() = %v, %v, want false, nil", ok, err)
	}
	if _, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}
	if ok, err := store.Exists(ctx, sumOf("hello")); err != nil || !ok {
		t.Fatalf("Put の後の Exists() = %v, %v, want true, nil", ok, err)
	}
	if n, err := store.Size(ctx, sumOf("hello")); err != nil || n != int64(len("hello")) {
		t.Errorf("Size() = %d, %v, want %d, nil", n, err, len("hello"))
	}
}

func TestStore_PutMismatchPlacesNothing(t *testing.T) {
	root := t.TempDir()
	local := NewLocalStore(root)
	store := New(local)
	ctx := context.Background()

	created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("goodbye"))
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Put() error = %v, want *MismatchError", err)
	}
	if created {
		t.Errorf("一致しない中身での Put() created = true, want false")
	}
	if mismatch.Want != sumOf("hello") || mismatch.Got != sumOf("goodbye") {
		t.Errorf("MismatchError = {Want: %s, Got: %s}, want {Want: %s, Got: %s}", mismatch.Want, mismatch.Got, sumOf("hello"), sumOf("goodbye"))
	}

	if _, statErr := os.Stat(local.path(sumOf("hello"))); !os.IsNotExist(statErr) {
		t.Errorf("一致しない中身が保存されている (statErr = %v), want os.ErrNotExist", statErr)
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

	if created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil || !created {
		t.Fatalf("Put() = %v, %v, want true, nil", created, err)
	}

	// 一致する中身なら成功するが, 保存先には書き込まず, 新規に作成したとは返さない.
	if created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil || created {
		t.Fatalf("2回目の Put() = %v, %v, want false, nil", created, err)
	}
	if backend.puts != 1 {
		t.Errorf("Backend.Put の回数 = %d, want 1", backend.puts)
	}

	// 既に存在しても, 一致しない中身は拒否する.
	_, err := store.Put(ctx, sumOf("hello"), strings.NewReader("goodbye"))
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("一致しない中身での Put() error = %v, want *MismatchError", err)
	}
}

func TestStore_PutPropagatesReadError(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))

	_, err := store.Put(context.Background(), sumOf("partial"), &failingReader{remaining: []byte("partial")})
	if err == nil {
		t.Fatalf("Put() error = nil, want error")
	}
	var mismatch *MismatchError
	if errors.As(err, &mismatch) {
		t.Errorf("Put() error = %v, want 読み取りのエラーのまま (MismatchError ではない)", err)
	}
}

// blockingBackend は, block の Sum への Put の途中で, release が閉じられるまで停止する.
type blockingBackend struct {
	countingBackend
	block   Sum
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBackend) Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) error {
	if sum == b.block {
		close(b.entered)
		<-b.release
	}
	return b.countingBackend.Put(ctx, sum, data, opts...)
}

func newBlockingBackend(t *testing.T, block Sum) *blockingBackend {
	return &blockingBackend{
		countingBackend: countingBackend{Backend: NewLocalStore(t.TempDir())},
		block:           block,
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
}

// waitForWaiters は, sum のロックを待機している数 (保持している1つを含む) が n になるまで待つ.
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
	t.Fatalf("%s のロックの待機数が %d にならなかった", sum, n)
}

func TestStore_ConcurrentPutsOfSameSumWriteOnce(t *testing.T) {
	backend := newBlockingBackend(t, sumOf("hello"))
	store := New(backend)
	ctx := context.Background()

	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	put := func() {
		created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello"))
		results <- result{created, err}
	}
	go put()
	<-backend.entered
	go put()
	// 2つ目の Put が, 1つ目の書き込みの途中で待機している状態を作る.
	waitForWaiters(t, store, sumOf("hello"), 2)
	close(backend.release)

	created := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Errorf("Put() error = %v, want nil", r.err)
		}
		if r.created {
			created++
		}
	}
	if backend.puts != 1 {
		t.Errorf("Backend.Put の回数 = %d, want 1", backend.puts)
	}
	// 存在の確認をロックの中で行うので, 新規に作成したと返すのは1つだけになる.
	if created != 1 {
		t.Errorf("created が true の Put の数 = %d, want 1", created)
	}
	if n := len(store.locks.entries); n != 0 {
		t.Errorf("終了後のロックの待機数 = %d, want 0", n)
	}
}

func TestStore_PutsOfDifferentSumsDoNotWait(t *testing.T) {
	backend := newBlockingBackend(t, sumOf("hello"))
	store := New(backend)
	ctx := context.Background()

	errs := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello"))
		errs <- err
	}()
	<-backend.entered

	// "hello" の書き込みが停止している間でも, 異なる名前の Put は完了する.
	if _, err := store.Put(ctx, sumOf("world"), strings.NewReader("world")); err != nil {
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
		t.Errorf("キャンセルした ctx での lock() error = %v, want context.Canceled", err)
	}

	unlock()
	if n := len(locks.entries); n != 0 {
		t.Errorf("終了後のロックの待機数 = %d, want 0", n)
	}
}
