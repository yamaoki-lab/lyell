package itemstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// assertNoDanglingBlobs は, 間引かれていない全ての revision が指す blob が blobstore にあることを確かめる. 記録が無い blob を指していたら, 書く順序が崩れている.
func assertNoDanglingBlobs(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.read.Query("SELECT id, blob_sha256 FROM revisions WHERE body IS NOT NULL")
	if err != nil {
		t.Fatalf("revision を読めない: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatalf("revision を読めない: %v", err)
		}
		var sum blobstore.Sum
		copy(sum[:], raw)
		ok, err := s.blobs.Exists(context.Background(), sum)
		if err != nil {
			t.Fatalf("Exists() error = %v, want nil", err)
		}
		if !ok {
			t.Errorf("revision %x が, 無い blob %s を指している", id, sum)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("revision を読めない: %v", err)
	}
}

func countRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.read.QueryRow("SELECT (SELECT count(*) FROM items) + (SELECT count(*) FROM revisions)").Scan(&n); err != nil {
		t.Fatalf("数を読めない: %v", err)
	}
	return n
}

func TestCreateItem_WritesBlobThenRecords(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("hello")

	if _, err := s.CreateItem(ctx, KindAsset, content, strings.NewReader("hello")); err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	r, err := s.blobs.Open(ctx, content.Blob)
	if err != nil {
		t.Fatalf("blob を開けない: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("blob を読めない: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("blob の中身 = %q, want %q", got, "hello")
	}
	assertNoDanglingBlobs(t, s)
}

func TestCreateItem_BlobFailureRecordsNothing(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("hello")

	// 中身が名前 (SHA-256) と合わない.
	_, err := s.CreateItem(ctx, KindAsset, content, strings.NewReader("goodbye"))
	var mismatch *blobstore.MismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("合わない中身の CreateItem() error = %v, want *blobstore.MismatchError", err)
	}

	// 中身を読む途中で失敗する.
	if _, err := s.CreateItem(ctx, KindAsset, content, io.MultiReader(strings.NewReader("hel"), errReader{})); err == nil {
		t.Errorf("読む途中で失敗する CreateItem() error = nil, want error")
	}

	if n := countRows(t, s); n != 0 {
		t.Errorf("blob を書けなかったのに, 行が %d 件記録された", n)
	}
	if ok, _ := s.blobs.Exists(ctx, content.Blob); ok {
		t.Errorf("書けなかった blob が置かれている")
	}
}

func TestCreateItem_RecordFailureLeavesOnlyOrphanBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("hello")

	// blob を書いた後の記録を失敗させるため, 書き込み用の DB を先に閉じておく.
	s.write.Close()
	if _, err := s.CreateItem(ctx, KindAsset, content, strings.NewReader("hello")); err == nil {
		t.Fatalf("記録に失敗する CreateItem() error = nil, want error")
	}

	// blob は残るが, それを指す記録は無い. 孤立した blob は無害で, 後の GC で回収する.
	if ok, _ := s.blobs.Exists(ctx, content.Blob); !ok {
		t.Errorf("先に書いたはずの blob が無い")
	}
	if n := countRows(t, s); n != 0 {
		t.Errorf("記録に失敗したのに, 行が %d 件ある", n)
	}
}

func TestCommitRevision_RejectedLeavesOnlyOrphanBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"), strings.NewReader("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	if _, err := s.CommitRevision(ctx, item.ID, item.Head, []ID{item.Head}, testContent("r2"), strings.NewReader("r2")); err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	before := countRows(t, s)

	// 古い head を前提にして断られても, 先に書いた blob は孤立して残るだけ.
	stale := testContent("stale")
	if _, err := s.CommitRevision(ctx, item.ID, item.Head, []ID{item.Head}, stale, strings.NewReader("stale")); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("CommitRevision() error = %v, want ErrNotFastForward", err)
	}
	if ok, _ := s.blobs.Exists(ctx, stale.Blob); !ok {
		t.Errorf("先に書いたはずの blob が無い")
	}
	if after := countRows(t, s); after != before {
		t.Errorf("断った時の行の数 = %d, want %d", after, before)
	}
	assertNoDanglingBlobs(t, s)
}

// errReader は, 読むと必ず失敗する.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("読み取りに失敗した") }
