package itemstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// unknownField は, この Lyell の RevisionBody に無い番号のフィールド. 新しい Lyell が書いた body を, 古い Lyell が読み書きする場面を再現する.
var unknownField = protowire.AppendString(protowire.AppendTag(nil, 1000, protowire.BytesType), "新しい Lyell のフィールド")

// bodyWith は, 名前と unknownField を持ち, 中身の列に写すフィールドには Content とは違う値を入れた body を作る.
func bodyWith(t *testing.T, name string) []byte {
	t.Helper()
	b, err := proto.Marshal(&storagev1alpha1.RevisionBody{Name: name, AuthorUser: "違う利用者", BlobSize: 12345})
	if err != nil {
		t.Fatalf("body を作れない: %v", err)
	}
	return append(b, unknownField...)
}

func TestCreateItem_BodyKeepsOtherFieldsAndTakesColumnsFromContent(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("data")
	content.Body = bodyWith(t, "名前")

	item, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: content, Data: strings.NewReader("data")})
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	rev, err := s.Revision(ctx, item.Head)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}

	var got storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(rev.Content.Body, &got); err != nil {
		t.Fatalf("記録した body を読めない: %v", err)
	}
	if got.GetName() != "名前" {
		t.Errorf("body の名前 = %q, want %q", got.GetName(), "名前")
	}
	if !bytes.Contains(got.ProtoReflect().GetUnknown(), unknownField) {
		t.Errorf("body に知らないフィールドが残っていない: %v", got.ProtoReflect().GetUnknown())
	}
	// 中身の列に写すフィールドは, Body に入っていた値ではなく Content の値になる.
	if got.GetAuthorUser() != content.AuthorUser || got.GetBlobSize() != int64(len("data")) {
		t.Errorf("body の利用者とサイズ = %q, %d, want %q, %d", got.GetAuthorUser(), got.GetBlobSize(), content.AuthorUser, len("data"))
	}
}

func TestCreateItem_RejectsUnreadableBodyBeforeWritingBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("data")
	content.Body = []byte{0xff} // タグとして読めない不正なバイト

	_, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: content, Data: strings.NewReader("data")})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("CreateItem() error = %v, want ErrInvalid", err)
	}
	if ok, err := s.blobs.Exists(ctx, content.Blob); err != nil || ok {
		t.Errorf("拒否した後の blob の有無 = %v (err = %v), want false", ok, err)
	}
}

func TestRevisions_ColumnsMatchBody(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// 作成, コミット, 他の item の revision を親に持つマージ, body に名前と知らないフィールドを持つものなど, 書き方を変えて revision を記録する.
	a, err := s.CreateItem(ctx, newTestItem("a1"))
	if err != nil {
		t.Fatalf("CreateItem(a) error = %v, want nil", err)
	}
	b, err := s.CreateItem(ctx, newTestItem("b1"))
	if err != nil {
		t.Fatalf("CreateItem(b) error = %v, want nil", err)
	}
	named := testContent("a2")
	named.Body = bodyWith(t, "名前")
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: a.ID, From: a.Head, Parents: []ID{a.Head}, Content: named, Data: strings.NewReader("a2")}); err != nil {
		t.Fatalf("CommitRevision(a) error = %v, want nil", err)
	}
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: b.ID, From: b.Head, Parents: []ID{b.Head, a.Head}, Content: testContent("b2"), Data: strings.NewReader("b2")}); err != nil {
		t.Fatalf("CommitRevision(b) error = %v, want nil", err)
	}

	assertColumnsMatchBody(t, s)
}

// assertColumnsMatchBody は, 間引かれていない全ての revision について, 中身の列の値が body から求めた値と一致するかを確かめる.
func assertColumnsMatchBody(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.read.Query("SELECT id, " + contentColumnList + " FROM revisions WHERE body IS NOT NULL")
	if err != nil {
		t.Fatalf("revisions を読めない: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var (
			id []byte
			c  contentColumns
		)
		if err := rows.Scan(append([]any{&id}, c.dest()...)...); err != nil {
			t.Fatalf("revisions を読めない: %v", err)
		}
		n++
		want, err := columnsFromBody(c.body)
		if err != nil {
			t.Errorf("revision %x の body を読めない: %v", id, err)
			continue
		}
		got := bodyColumns{createdAt: c.createdAt.Int64, authorUser: c.authorUser.String, authorMachine: c.authorMachine.String, blob: c.blob, blobSize: c.blobSize.Int64}
		if got.createdAt != want.createdAt || got.authorUser != want.authorUser || got.authorMachine != want.authorMachine || !bytes.Equal(got.blob, want.blob) || got.blobSize != want.blobSize {
			t.Errorf("revision %x の列 = %+v, body から求めた値 = %+v", id, got, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("revisions を読めない: %v", err)
	}
	if n == 0 {
		t.Fatalf("確かめた revision が無い")
	}
}

func TestEncodeBody_WithoutBlobLeavesBlobFieldsEmpty(t *testing.T) {
	c := testContent("")
	c.Blob = blobstore.Sum{}
	c.BlobSize = 5 // blob を持たない時は, 渡された大きさを書かない
	body, err := encodeBody(c)
	if err != nil {
		t.Fatalf("encodeBody() error = %v, want nil", err)
	}
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(body, &b); err != nil {
		t.Fatalf("body を読めない: %v", err)
	}
	if len(b.GetBlobSha256()) != 0 || b.GetBlobSize() != 0 {
		t.Errorf("body の blob_sha256 = %x, blob_size = %d, want 空, 0", b.GetBlobSha256(), b.GetBlobSize())
	}
	cols, err := columnsFromBody(body)
	if err != nil {
		t.Fatalf("columnsFromBody() error = %v, want nil", err)
	}
	if cols.blob != nil || cols.blobSizeColumn() != nil {
		t.Errorf("列の blob_sha256 = %x, blob_size = %v, want NULL, NULL", cols.blob, cols.blobSizeColumn())
	}
}

func TestRevision_ReadsRevisionWithoutBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	c := testContent("")
	c.Blob = blobstore.Sum{}
	err := inTx(t, s, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", testID(1), testID(2)); err != nil {
			return err
		}
		return insertRevision(ctx, tx, ID(testID(2)), ID(testID(1)), c)
	})
	if err != nil {
		t.Fatalf("blob を持たない revision を記録できない: %v", err)
	}
	var null bool
	if err := s.read.QueryRow("SELECT blob_sha256 IS NULL AND blob_size IS NULL FROM revisions").Scan(&null); err != nil || !null {
		t.Errorf("blob_sha256 と blob_size が NULL か = %v (error = %v), want true", null, err)
	}

	rev, err := s.Revision(ctx, ID(testID(2)))
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if rev.Content == nil || rev.Content.Blob != (blobstore.Sum{}) || rev.Content.BlobSize != 0 {
		t.Errorf("Revision().Content = %+v, want blob 無し", rev.Content)
	}
	assertColumnsMatchBody(t, s)
}

func TestColumnsFromBody_ExplicitEmptyBlobIsNull(t *testing.T) {
	// 他の実装が書いた body は, 長さ0の blob_sha256 を明示的に持つことがある. 列には長さ0の BLOB ではなく NULL を書く.
	body := protowire.AppendBytes(protowire.AppendTag(nil, 4, protowire.BytesType), nil)
	cols, err := columnsFromBody(body)
	if err != nil {
		t.Fatalf("columnsFromBody() error = %v, want nil", err)
	}
	if cols.blob != nil || cols.blobSizeColumn() != nil {
		t.Errorf("列の blob_sha256 = %#v, blob_size = %v, want NULL, NULL", cols.blob, cols.blobSizeColumn())
	}
}
