package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
)

func TestValidateOnly(t *testing.T) {
	dir := t.TempDir()
	s, hs := startTestServerIn(t, dir)
	c := apiv1alpha1connect.NewItemServiceClient(hs.Client(), hs.URL)
	ctx := context.Background()

	db, err := sql.Open("sqlite", filepath.Join(dir, "lyell.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var created *apiv1alpha1.CreateItemResponse
	t.Run("作成結果を返し何も保存しない", func(t *testing.T) {
		sum := putBlob(t, s, "新しい blob")
		req := &apiv1alpha1.CreateItemRequest{
			Kind:         apiv1alpha1.ItemKind_ITEM_KIND_ASSET,
			Content:      &apiv1alpha1.RevisionContent{BlobSha256: sum, Name: " e\u0301 "},
			ValidateOnly: true,
		}
		got, err := c.CreateItem(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("検証のみの CreateItem() error = %v", err)
		}
		if name := got.Msg.GetRevision().GetContent().GetName(); name != "é" {
			t.Errorf("検証で返った name = %q, want %q", name, "é")
		}
		req.Content.BlobSha256 = ""
		withoutBlob, err := c.CreateItem(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("blob を省略した CreateItem() error = %v", err)
		}
		wantWithoutBlob := proto.CloneOf(got.Msg)
		wantWithoutBlob.Revision.Content.BlobSha256, wantWithoutBlob.Revision.Content.BlobSize = "", 0
		if !proto.Equal(withoutBlob.Msg, wantWithoutBlob) {
			t.Errorf("blob を省略した CreateItem() = %v, want %v", withoutBlob.Msg, wantWithoutBlob)
		}
		wantRecordCounts(t, db, 0, 0)

		req.Content.BlobSha256, req.ValidateOnly = sum, false
		res, err := c.CreateItem(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("CreateItem() error = %v", err)
		}
		created = res.Msg
		want := proto.CloneOf(created)
		want.Item.Id, want.Item.Head = "", ""
		want.Revision.Id, want.Revision.ItemId, want.Revision.Content.CreateTime = "", "", nil
		if !proto.Equal(got.Msg, want) {
			t.Errorf("検証のみの CreateItem() = %v, want %v", got.Msg, want)
		}
	})
	if created == nil {
		return
	}

	itemID, head := created.GetItem().GetId(), created.GetItem().GetHead()
	other := createItem(t, s, c, "別の item").GetItem().GetHead()
	req := &apiv1alpha1.CommitRevisionRequest{
		ItemId: itemID, ExpectedHead: head, Content: &apiv1alpha1.RevisionContent{Name: " 次の名前 "},
		ValidateOnly: true,
	}

	t.Run("書き込みロック中も両方の検証が完了する", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

		createCtx, cancelCreate := context.WithTimeout(ctx, 2*time.Second)
		defer cancelCreate()
		_, err = c.CreateItem(createCtx, connect.NewRequest(&apiv1alpha1.CreateItemRequest{
			Kind:         apiv1alpha1.ItemKind_ITEM_KIND_ASSET,
			Content:      &apiv1alpha1.RevisionContent{BlobSha256: created.GetRevision().GetContent().GetBlobSha256()},
			ValidateOnly: true,
		}))
		if err != nil {
			t.Errorf("書き込みロック中の CreateItem() error = %v", err)
		}

		commitCtx, cancelCommit := context.WithTimeout(ctx, 2*time.Second)
		defer cancelCommit()
		got, err := c.CommitRevision(commitCtx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("書き込みロック中の CommitRevision() error = %v", err)
		}
		if parents := got.Msg.GetRevision().GetParents(); len(parents) != 1 || parents[0] != head {
			t.Errorf("parents = %v, want [%s]", parents, head)
		}
	})

	t.Run("マージの応答を返し head を進めない", func(t *testing.T) {
		// 保存済み revision は親を ID 順で返すため, リクエストでは逆順に指定する.
		req.Parents = []string{other, head}
		got, err := c.CommitRevision(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("検証のみの CommitRevision() error = %v", err)
		}
		wantRecordCounts(t, db, 2, 2)
		item, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: itemID}))
		if err != nil {
			t.Fatal(err)
		}
		if item.Msg.GetItem().GetHead() != head {
			t.Errorf("検証後の head = %s, want %s", item.Msg.GetItem().GetHead(), head)
		}

		req.ValidateOnly = false
		want := commit(t, c, req)
		want.Id, want.Content.CreateTime = "", nil
		if !proto.Equal(got.Msg.GetRevision(), want) {
			t.Errorf("検証のみの CommitRevision() = %v, want %v", got.Msg.GetRevision(), want)
		}
	})
}

// wantRecordCounts は, DB に記録されている item と revision の件数を検証する.
func wantRecordCounts(t *testing.T, db *sql.DB, items, revisions int) {
	t.Helper()
	var gotItems, gotRevisions int
	err := db.QueryRow("SELECT (SELECT count(*) FROM items), (SELECT count(*) FROM revisions)").Scan(&gotItems, &gotRevisions)
	if err != nil {
		t.Fatal(err)
	}
	if gotItems != items || gotRevisions != revisions {
		t.Errorf("DB 内の item, revision の件数 = %d, %d, want %d, %d", gotItems, gotRevisions, items, revisions)
	}
}

// wantCreateValidationError は, 作成時と検証時の RPC エラーが同じであることを検証する.
func wantCreateValidationError(t *testing.T, c apiv1alpha1connect.ItemServiceClient, req *apiv1alpha1.CreateItemRequest, want error) {
	t.Helper()
	validate := proto.CloneOf(req)
	validate.ValidateOnly = true
	_, got := c.CreateItem(context.Background(), connect.NewRequest(validate))
	wantSameRPCError(t, got, want)
}

// wantCommitValidationError は, コミット時と検証時の RPC エラーが同じであることを検証する.
func wantCommitValidationError(t *testing.T, c apiv1alpha1connect.ItemServiceClient, req *apiv1alpha1.CommitRevisionRequest, want error) {
	t.Helper()
	validate := proto.CloneOf(req)
	validate.ValidateOnly = true
	_, got := c.CommitRevision(context.Background(), connect.NewRequest(validate))
	wantSameRPCError(t, got, want)
}

// wantSameRPCError は, エラーコード, メッセージ, 詳細情報の型と値を比較する.
func wantSameRPCError(t *testing.T, got, want error) {
	t.Helper()
	var a, b *connect.Error
	if !errors.As(got, &a) || !errors.As(want, &b) {
		t.Fatalf("検証時と通常時の両方で RPC エラーが返る必要がある: 検証時 %v, 通常時 %v", got, want)
	}
	if a.Code() != b.Code() || a.Message() != b.Message() || len(a.Details()) != len(b.Details()) {
		t.Fatalf("検証時のエラー = %v (%d 件の詳細情報), want %v (%d 件の詳細情報)", a, len(a.Details()), b, len(b.Details()))
	}
	for i, detail := range a.Details() {
		gotValue, gotErr := detail.Value()
		wantValue, wantErr := b.Details()[i].Value()
		if gotErr != nil || wantErr != nil || !proto.Equal(gotValue, wantValue) {
			t.Errorf("検証時の詳細情報 [%d] = %v (%v), want %v (%v)", i, gotValue, gotErr, wantValue, wantErr)
		}
	}
}
