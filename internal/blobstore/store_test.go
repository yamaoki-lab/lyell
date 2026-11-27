package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingBackend は, Backend.Put が呼ばれた回数を数える.
type countingBackend struct {
	Backend
	puts atomic.Int32
}

func (b *countingBackend) Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) (bool, error) {
	b.puts.Add(1)
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
	if backend.puts.Load() != 1 {
		t.Errorf("Backend.Put の回数 = %d, want 1", backend.puts.Load())
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

func (b *blockingBackend) Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) (bool, error) {
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

// gatedReader は, 中身を全て返した後, io.EOF を返す前に arrived へ知らせ, gate が閉じられるまで停止する. 転送が止まった接続や, 確定の直前で揃えた Put を再現する.
type gatedReader struct {
	data    []byte
	arrived chan<- struct{}
	gate    <-chan struct{}
	waited  bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.waited {
		r.waited = true
		r.arrived <- struct{}{}
		<-r.gate
	}
	return 0, io.EOF
}

type putResult struct {
	created bool
	err     error
}

func TestStore_StalledPutDoesNotBlockPutOfSameSum(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
	ctx := context.Background()

	arrived := make(chan struct{}, 1)
	gate := make(chan struct{})
	stalled := make(chan putResult, 1)
	go func() {
		created, err := store.Put(ctx, sumOf("hello"), &gatedReader{data: []byte("hello"), arrived: arrived, gate: gate})
		stalled <- putResult{created, err}
	}()
	<-arrived

	// 先の Put の転送が止まっている間に, 送り直しの Put が完了する.
	done := make(chan putResult, 1)
	go func() {
		created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello"))
		done <- putResult{created, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.created {
			t.Errorf("送り直しの Put() = %v, %v, want true, nil", r.created, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("送り直しの Put が, 止まった Put を待っている")
	}

	// 止まっていた Put は, 後から確定しようとして既にあると分かるので, 新規に作成したとは返さない.
	close(gate)
	if r := <-stalled; r.err != nil || r.created {
		t.Errorf("止まっていた Put() = %v, %v, want false, nil", r.created, r.err)
	}
}

// putTogether は, stores のそれぞれで同じ中身を Put し, 全ての Put が確定の直前に揃ってから同時に確定させる.
func putTogether(t *testing.T, stores []*Store, content string) []putResult {
	t.Helper()
	ctx := context.Background()
	arrived := make(chan struct{}, len(stores))
	gate := make(chan struct{})
	results := make(chan putResult, len(stores))
	for _, store := range stores {
		go func() {
			created, err := store.Put(ctx, sumOf(content), &gatedReader{data: []byte(content), arrived: arrived, gate: gate})
			results <- putResult{created, err}
		}()
	}
	for range stores {
		<-arrived
	}
	close(gate)
	var out []putResult
	for range stores {
		out = append(out, <-results)
	}
	return out
}

func checkCreatedOnce(t *testing.T, results []putResult) {
	t.Helper()
	created := 0
	for _, r := range results {
		if r.err != nil {
			t.Errorf("Put() error = %v, want nil", r.err)
		}
		if r.created {
			created++
		}
	}
	if created != 1 {
		t.Errorf("created が true の Put の数 = %d, want 1", created)
	}
}

func TestStore_ConcurrentPutsOfSameSumCreateOnce(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
	checkCreatedOnce(t, putTogether(t, []*Store{store, store, store}, "hello"))
}

// 同じ保存先を別のプロセスが使う場合を, 保存先を共有する別の Store で再現する.
func TestStore_PutsOfSameSumSharingRootCreateOnce(t *testing.T) {
	root := t.TempDir()
	stores := []*Store{New(NewLocalStore(root)), New(NewLocalStore(root)), New(NewLocalStore(root))}
	checkCreatedOnce(t, putTogether(t, stores, "hello"))

	local := NewLocalStore(root)
	r, err := local.Open(context.Background(), sumOf("hello"))
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	defer func() { _ = r.Close() }()
	if got, err := io.ReadAll(r); err != nil || string(got) != "hello" {
		t.Errorf("中身 = %q, %v, want %q, nil", got, err, "hello")
	}
	checkNoTemp(t, root)
}

// checkNoTemp は, root/.tmp の下に一時ファイルが残っていないことを確認する.
func checkNoTemp(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".tmp"))
	if err != nil {
		t.Fatalf("ReadDir(.tmp) error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf(".tmp の下に%d件のファイルが残っている, want 0", len(entries))
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
