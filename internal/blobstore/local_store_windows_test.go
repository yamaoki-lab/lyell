package blobstore

import (
	"context"
	"strings"
	"syscall"
	"testing"
)

// Go のパーミッションの表現を介さずに, Windows の読み取り専用の属性そのものを確かめる.
func TestLocalStore_PutSetsReadOnlyAttribute(t *testing.T) {
	store := NewLocalStore(t.TempDir())
	sum := sumOf("hello")

	if err := store.Put(context.Background(), sum, strings.NewReader("hello")); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}

	p, err := syscall.UTF16PtrFromString(store.path(sum))
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v, want nil", err)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatalf("GetFileAttributes() error = %v, want nil", err)
	}
	if attrs&syscall.FILE_ATTRIBUTE_READONLY == 0 {
		t.Errorf("blob の属性 = %#x, want FILE_ATTRIBUTE_READONLY (%#x) を含む", attrs, syscall.FILE_ATTRIBUTE_READONLY)
	}
}
