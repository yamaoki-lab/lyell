//go:build !windows

package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

// withLinkFile は, テストの間だけ linkFile を fn に差し替える.
func withLinkFile(t *testing.T, fn func(oldname, newname string) error) {
	t.Helper()
	orig := linkFile
	linkFile = fn
	t.Cleanup(func() { linkFile = orig })
}

func TestLocalStore_PutWithoutHardLinksFallsBackToRename(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		t.Run(errno.Error(), func(t *testing.T) {
			withLinkFile(t, func(oldname, newname string) error {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
			})
			root := t.TempDir()
			store := NewLocalStore(root)
			ctx := context.Background()

			if created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("hello")); err != nil || !created {
				t.Fatalf("Put() = %v, %v, want true, nil", created, err)
			}
			// 既にあれば, rename で置き換えずに false を返す.
			if created, err := store.Put(ctx, sumOf("hello"), strings.NewReader("other")); err != nil || created {
				t.Fatalf("2回目の Put() = %v, %v, want false, nil", created, err)
			}
			r, err := store.Open(ctx, sumOf("hello"))
			if err != nil {
				t.Fatalf("Open() error = %v, want nil", err)
			}
			defer func() { _ = r.Close() }()
			if got, _ := io.ReadAll(r); string(got) != "hello" {
				t.Errorf("中身 = %q, want %q (置き換えられていない)", got, "hello")
			}
			checkNoTemp(t, root)
		})
	}
}

// ハードリンクを使えない時も, 同じプロセスの Put 同士では, 保存先を共有する別の Store からでも, true を返すのは1つだけになる.
func TestLocalStore_PutWithoutHardLinksCreatesOnceInOneProcess(t *testing.T) {
	withLinkFile(t, func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	})
	root := t.TempDir()
	stores := []*Store{New(NewLocalStore(root)), New(NewLocalStore(root)), New(NewLocalStore(root))}
	checkCreatedOnce(t, putTogether(t, stores, "hello"))
}

func TestLocalStore_PutReturnsOtherLinkErrors(t *testing.T) {
	withLinkFile(t, func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EIO}
	})
	root := t.TempDir()
	store := NewLocalStore(root)

	if created, err := store.Put(context.Background(), sumOf("hello"), strings.NewReader("hello")); !errors.Is(err, syscall.EIO) || created {
		t.Errorf("Put() = %v, %v, want false, EIO", created, err)
	}
	if _, err := os.Stat(store.path(sumOf("hello"))); !os.IsNotExist(err) {
		t.Errorf("リンクに失敗したのに blob が置かれている (statErr = %v)", err)
	}
	checkNoTemp(t, root)
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
