package itemstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

func testContent(body string) Content {
	return Content{
		// ミリ秒より細かい部分は切り捨てられることを確かめるため, ナノ秒まで入れておく.
		CreatedAt:     time.Date(2026, 9, 28, 12, 34, 56, 789_123_456, time.UTC),
		AuthorUser:    "user",
		AuthorMachine: "machine",
		Blob:          blobstore.Sum(sha256.Sum256([]byte(body))),
		Body:          []byte(body),
	}
}

func TestNewID_IsVersion7AndOrdered(t *testing.T) {
	a, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v, want nil", err)
	}
	time.Sleep(2 * time.Millisecond)
	b, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v, want nil", err)
	}

	if v := a[6] >> 4; v != 7 {
		t.Errorf("UUID のバージョンの桁 = %d, want 7 (%s)", v, a)
	}
	if bytes.Compare(a[:], b[:]) >= 0 {
		t.Errorf("後に作った ID が前の ID より大きくない: %s, %s", a, b)
	}
}

func TestCreateItem_ThenRead(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("body")

	item, err := s.CreateItem(ctx, KindAsset, content)
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	got, err := s.Item(ctx, item.ID)
	if err != nil {
		t.Fatalf("Item() error = %v, want nil", err)
	}
	if got != item {
		t.Errorf("Item() = %+v, want %+v", got, item)
	}

	rev, err := s.Revision(ctx, item.Head)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if rev.ID != item.Head || rev.ItemID != item.ID {
		t.Errorf("Revision() の ID = %s, ItemID = %s, want %s, %s", rev.ID, rev.ItemID, item.Head, item.ID)
	}
	if len(rev.Parents) != 0 {
		t.Errorf("最初の revision の親 = %v, want 無し", rev.Parents)
	}
	if rev.Content == nil {
		t.Fatalf("Revision().Content = nil, want 中身")
	}
	want := content
	want.CreatedAt = content.CreatedAt.Truncate(time.Millisecond)
	c := rev.Content
	if !c.CreatedAt.Equal(want.CreatedAt) || c.AuthorUser != want.AuthorUser || c.AuthorMachine != want.AuthorMachine || c.Blob != want.Blob || !bytes.Equal(c.Body, want.Body) {
		t.Errorf("Revision().Content = %+v, want %+v", *c, want)
	}
}

func TestCreateItem_EmptyBodyIsNotThinned(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// 空の Protobuf のメッセージは0バイトになる. nil で渡されても, 間引かれた印 (NULL) と取り違えない.
	for _, body := range [][]byte{nil, {}} {
		content := testContent("")
		content.Body = body
		item, err := s.CreateItem(ctx, KindAsset, content)
		if err != nil {
			t.Fatalf("CreateItem() error = %v, want nil", err)
		}

		var isNull bool
		if err := s.read.QueryRow("SELECT body IS NULL FROM revisions WHERE id = ?", item.Head[:]).Scan(&isNull); err != nil {
			t.Fatalf("body を読めない: %v", err)
		}
		if isNull {
			t.Errorf("空の body (%#v) が NULL として記録された", body)
		}

		rev, err := s.Revision(ctx, item.Head)
		if err != nil {
			t.Fatalf("Revision() error = %v, want nil", err)
		}
		if rev.Content == nil || rev.Content.Body == nil || len(rev.Content.Body) != 0 {
			t.Errorf("空の body (%#v) の Revision().Content = %+v, want 長さ0の Body", body, rev.Content)
		}
	}
}

func TestCreateItem_RejectsInvalidInputWithoutWriting(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	noUser := testContent("body")
	noUser.AuthorUser = ""
	noTime := testContent("body")
	noTime.CreatedAt = time.Time{}

	tests := []struct {
		name    string
		kind    Kind
		content Content
	}{
		{name: "知らない種類", kind: "folder", content: testContent("body")},
		{name: "利用者が無い", kind: KindAsset, content: noUser},
		{name: "時刻が無い", kind: KindAsset, content: noTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.CreateItem(ctx, tt.kind, tt.content); err == nil {
				t.Errorf("CreateItem() error = nil, want error")
			}
		})
	}

	var n int
	if err := s.read.QueryRow("SELECT (SELECT count(*) FROM items) + (SELECT count(*) FROM revisions)").Scan(&n); err != nil {
		t.Fatalf("数を読めない: %v", err)
	}
	if n != 0 {
		t.Errorf("断った CreateItem の行が %d 件残っている", n)
	}
}

func TestItemAndRevision_NotFound(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Item(ctx, ID(testID(1))); !errors.Is(err, ErrNotFound) {
		t.Errorf("Item() error = %v, want ErrNotFound", err)
	}
	if _, err := s.Revision(ctx, ID(testID(1))); !errors.Is(err, ErrNotFound) {
		t.Errorf("Revision() error = %v, want ErrNotFound", err)
	}
}

func TestRevision_ParentsSurviveThinning(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	r1 := item.Head
	r2, r3 := ID(testID(2)), ID(testID(3))

	// revision を足す関数はまだ無いので, r1 ← r2 ← r3 と繋がる revision を直接入れる.
	if err := inTx(t, s, func(tx *sql.Tx) error {
		for _, r := range []struct{ id, parent ID }{{r2, r1}, {r3, r2}} {
			if err := insertRevision(ctx, tx, r.id, item.ID, testContent("r")); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO revision_parents VALUES (?, ?)", r.id[:], r.parent[:]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("revision を入れられない: %v", err)
	}

	// r2 を間引く. 中身の列だけが消え, ID と親が残る.
	if _, err := s.write.Exec(`UPDATE revisions SET created_at = NULL, author_user = NULL, author_machine = NULL, blob_sha256 = NULL, body = NULL WHERE id = ?`, r2[:]); err != nil {
		t.Fatalf("間引けない: %v", err)
	}

	thinned, err := s.Revision(ctx, r2)
	if err != nil {
		t.Fatalf("間引いた revision の Revision() error = %v, want nil", err)
	}
	if thinned.Content != nil {
		t.Errorf("間引いた revision の Content = %+v, want nil", thinned.Content)
	}

	// r3 から親を辿ると, 間引いた r2 を通って r1 に着く.
	var path []ID
	for id := r3; ; {
		rev, err := s.Revision(ctx, id)
		if err != nil {
			t.Fatalf("Revision(%s) error = %v, want nil", id, err)
		}
		path = append(path, id)
		if len(rev.Parents) == 0 {
			break
		}
		id = rev.Parents[0]
	}
	want := []ID{r3, r2, r1}
	if len(path) != len(want) || path[0] != want[0] || path[1] != want[1] || path[2] != want[2] {
		t.Errorf("辿った道 = %v, want %v", path, want)
	}
}
