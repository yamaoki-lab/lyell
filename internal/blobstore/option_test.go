package blobstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testOption は, テストの中だけで使う書き込みの選択肢.
type testOption struct {
	required bool
}

func (o testOption) Required() bool { return o.required }

func TestLocalStore_CheckPutOptions(t *testing.T) {
	store := NewLocalStore(t.TempDir())

	if err := store.CheckPutOptions(testOption{required: false}); err != nil {
		t.Errorf("希望の選択肢で CheckPutOptions() error = %v, want nil", err)
	}

	required := testOption{required: true}
	err := store.CheckPutOptions(testOption{required: false}, required)
	var unsupported *UnsupportedOptionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("必須の選択肢で CheckPutOptions() error = %v, want *UnsupportedOptionError", err)
	}
	if unsupported.Option != required {
		t.Errorf("UnsupportedOptionError.Option = %v, want %v", unsupported.Option, required)
	}
	if unsupported.Reason == "" {
		t.Errorf("UnsupportedOptionError.Reason が空, want 理由")
	}
}

func TestStore_PutRejectsRequiredOptionBeforeReading(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))
	ctx := context.Background()

	// 読むとすぐエラーになる Reader を渡す. 読む前に断っていれば, 読み取りのエラーではなく UnsupportedOptionError が返る.
	err := store.Put(ctx, sumOf("hello"), &failingReader{}, testOption{required: true})
	var unsupported *UnsupportedOptionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Put() error = %v, want *UnsupportedOptionError", err)
	}

	ok, err := store.Exists(ctx, sumOf("hello"))
	if err != nil {
		t.Fatalf("Exists() error = %v, want nil", err)
	}
	if ok {
		t.Errorf("断った Put の blob が置かれている")
	}
}

func TestStore_PutIgnoresUnknownHint(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))

	if err := store.Put(context.Background(), sumOf("hello"), strings.NewReader("hello"), testOption{required: false}); err != nil {
		t.Errorf("希望の選択肢で Put() error = %v, want nil", err)
	}
}

func TestStore_CheckPutOptionsMatchesBackend(t *testing.T) {
	store := New(NewLocalStore(t.TempDir()))

	if err := store.CheckPutOptions(testOption{required: false}); err != nil {
		t.Errorf("希望の選択肢で CheckPutOptions() error = %v, want nil", err)
	}
	var unsupported *UnsupportedOptionError
	if err := store.CheckPutOptions(testOption{required: true}); !errors.As(err, &unsupported) {
		t.Errorf("必須の選択肢で CheckPutOptions() error = %v, want *UnsupportedOptionError", err)
	}
}
