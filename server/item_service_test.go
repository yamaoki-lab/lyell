package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// newTestServer は, startTestServer で待ち受け, ItemService のクライアントを返す.
func newTestServer(t *testing.T) (*Server, apiv1alpha1connect.ItemServiceClient) {
	t.Helper()
	s, hs := startTestServer(t)
	return s, apiv1alpha1connect.NewItemServiceClient(hs.Client(), hs.URL)
}

// startTestServer は, 一時ディレクトリに DB と blob の保存先を作成し, Register した mux を httptest のサーバで待ち受ける.
func startTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), Config{DBPath: filepath.Join(dir, "lyell.db"), BlobDir: filepath.Join(dir, "blobs")})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	// Cleanup は登録と逆の順に実行されるので, 待ち受けを止めてから DB を閉じる.
	t.Cleanup(func() { _ = s.Close() })
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return s, hs
}

// putBlob は, data を blob として保存し, その SHA-256 を API の表記で返す. blob の HTTP を経ずに, blobstore へ直接保存する.
func putBlob(t *testing.T, s *Server, data string) string {
	t.Helper()
	sum := blobstore.Sum(sha256.Sum256([]byte(data)))
	if _, err := s.blobs.Put(context.Background(), sum, strings.NewReader(data)); err != nil {
		t.Fatalf("blob を保存できない: %v", err)
	}
	return sum.String()
}

func createItem(t *testing.T, s *Server, c apiv1alpha1connect.ItemServiceClient, data string) *apiv1alpha1.CreateItemResponse {
	t.Helper()
	res, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, BlobSha256: putBlob(t, s, data)}))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	return res.Msg
}

func wantCode(t *testing.T, name string, err error, want connect.Code) {
	t.Helper()
	if got := connect.CodeOf(err); got != want {
		t.Errorf("%s の error = %v (%v), want %v", name, err, got, want)
	}
}

func TestCreateItem_RecordsServerSideFields(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	before := time.Now().Truncate(time.Millisecond)
	created := createItem(t, s, c, "hello")
	after := time.Now()

	item, rev := created.GetItem(), created.GetRevision()
	if item.GetKind() != apiv1alpha1.ItemKind_ITEM_KIND_ASSET || item.GetHead() != rev.GetId() || rev.GetItemId() != item.GetId() || len(rev.GetParents()) != 0 {
		t.Errorf("CreateItem() = %v, want head が最初の revision を指す asset", created)
	}
	// 時刻と author はサーバが決める. 認証が無いので author は anonymous になる.
	content := rev.GetContent()
	if got := content.GetCreateTime().AsTime(); got.Before(before) || got.After(after) {
		t.Errorf("create_time = %v, want %v から %v の間", got, before, after)
	}
	if content.GetAuthorUser() != anonymous || content.GetAuthorMachine() != anonymous {
		t.Errorf("author = %q, %q, want 両方 %q", content.GetAuthorUser(), content.GetAuthorMachine(), anonymous)
	}
	if want := blobstore.Sum(sha256.Sum256([]byte("hello"))).String(); content.GetBlobSha256() != want || content.GetBlobSize() != int64(len("hello")) {
		t.Errorf("blob = %s (%d バイト), want %s (%d バイト)", content.GetBlobSha256(), content.GetBlobSize(), want, len("hello"))
	}

	got, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: item.GetId()}))
	if err != nil {
		t.Fatalf("GetItem() error = %v, want nil", err)
	}
	if !proto.Equal(got.Msg.GetItem(), item) {
		t.Errorf("GetItem() = %v, want %v", got.Msg.GetItem(), item)
	}
	gotRev, err := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: rev.GetId()}))
	if err != nil {
		t.Fatalf("GetRevision() error = %v, want nil", err)
	}
	if !proto.Equal(gotRev.Msg.GetRevision(), rev) {
		t.Errorf("GetRevision() = %v, want %v", gotRev.Msg.GetRevision(), rev)
	}
}

func TestCreateItem_RejectsBadRequest(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	sum := putBlob(t, s, "hello")
	missing := blobstore.Sum(sha256.Sum256([]byte("missing"))).String()

	tests := []struct {
		name string
		req  *apiv1alpha1.CreateItemRequest
		want connect.Code
	}{
		{name: "種類が指定されていない", req: &apiv1alpha1.CreateItemRequest{BlobSha256: sum}, want: connect.CodeInvalidArgument},
		{name: "collection", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION, BlobSha256: sum}, want: connect.CodeInvalidArgument},
		{name: "SHA-256 が大文字", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, BlobSha256: strings.ToUpper(sum)}, want: connect.CodeInvalidArgument},
		{name: "SHA-256 が短い", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, BlobSha256: sum[:63]}, want: connect.CodeInvalidArgument},
		{name: "blob が存在しない", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, BlobSha256: missing}, want: connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateItem(ctx, connect.NewRequest(tt.req))
			wantCode(t, "CreateItem()", err, tt.want)
		})
	}
}

func TestGet_RejectsUnknownAndMalformedID(t *testing.T) {
	_, c := newTestServer(t)
	ctx := context.Background()
	unknown := uuid.Must(uuid.NewV7())

	tests := []struct {
		name string
		id   string
		want connect.Code
	}{
		{name: "存在しない", id: unknown.String(), want: connect.CodeNotFound},
		{name: "ID の形式でない", id: "item", want: connect.CodeInvalidArgument},
		// uuid.Parse は受け付けるが, API の表記ではない.
		{name: "波括弧付き", id: "{" + unknown.String() + "}", want: connect.CodeInvalidArgument},
		{name: "ハイフン無し", id: strings.ReplaceAll(unknown.String(), "-", ""), want: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: tt.id}))
			wantCode(t, "GetItem()", err, tt.want)
			_, err = c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: tt.id}))
			wantCode(t, "GetRevision()", err, tt.want)
		})
	}
}

func TestCommitRevision_AdvancesHead(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	created := createItem(t, s, c, "r1")
	r1 := created.GetItem().GetHead()

	// parents を指定しなければ, expected_head だけが親になる.
	res, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: r1, BlobSha256: putBlob(t, s, "r2")}))
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	r2 := res.Msg.GetRevision()
	if len(r2.GetParents()) != 1 || r2.GetParents()[0] != r1 {
		t.Errorf("追加した revision の親 = %v, want [%s]", r2.GetParents(), r1)
	}
	item, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: created.GetItem().GetId()}))
	if err != nil {
		t.Fatalf("GetItem() error = %v, want nil", err)
	}
	if item.Msg.GetItem().GetHead() != r2.GetId() {
		t.Errorf("head = %s, want 追加した revision %s", item.Msg.GetItem().GetHead(), r2.GetId())
	}
}

func TestCommitRevision_StaleHeadIsAbortedWithHeadMoved(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	created := createItem(t, s, c, "r1")
	itemID, r1 := created.GetItem().GetId(), created.GetItem().GetHead()
	res, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, BlobSha256: putBlob(t, s, "r2")}))
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	r2 := res.Msg.GetRevision().GetId()

	// head は r2 に進んでいるが, 古い r1 を前提として追加しようとする.
	_, err = c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, BlobSha256: putBlob(t, s, "stale")}))
	wantCode(t, "古い head を前提とした CommitRevision()", err, connect.CodeAborted)

	// 呼び出し側が読み直さずに現在の head を取得できるよう, 詳細に HeadMoved が付いている.
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error = %T, want *connect.Error", err)
	}
	var moved *apiv1alpha1.HeadMoved
	for _, d := range ce.Details() {
		if v, err := d.Value(); err == nil {
			if m, ok := v.(*apiv1alpha1.HeadMoved); ok {
				moved = m
			}
		}
	}
	if moved.GetCurrentHead() != r2 {
		t.Errorf("HeadMoved = %v, want current_head %s", moved, r2)
	}
}

func TestCommitRevision_ErrorCodes(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	created := createItem(t, s, c, "r1")
	itemID, r1 := created.GetItem().GetId(), created.GetItem().GetHead()
	sum := putBlob(t, s, "r2")
	unknown := uuid.Must(uuid.NewV7()).String()

	tests := []struct {
		name string
		req  *apiv1alpha1.CommitRevisionRequest
		want connect.Code
	}{
		{name: "親が重複している", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, r1}, BlobSha256: sum}, want: connect.CodeInvalidArgument},
		{name: "親の ID の形式でない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, "parent"}, BlobSha256: sum}, want: connect.CodeInvalidArgument},
		{name: "存在しない revision を親にする", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, unknown}, BlobSha256: sum}, want: connect.CodeNotFound},
		{name: "存在しない item", req: &apiv1alpha1.CommitRevisionRequest{ItemId: unknown, ExpectedHead: r1, BlobSha256: sum}, want: connect.CodeNotFound},
		{name: "blob が存在しない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, BlobSha256: blobstore.Sum(sha256.Sum256([]byte("missing"))).String()}, want: connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CommitRevision(ctx, connect.NewRequest(tt.req))
			wantCode(t, "CommitRevision()", err, tt.want)
		})
	}
}

func TestInternalErrorHidesDetails(t *testing.T) {
	s, c := newTestServer(t)
	created := createItem(t, s, c, "hello")

	// DB を先に閉じて, 呼び出し側の誤りではない失敗を起こす.
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	_, err := c.GetItem(context.Background(), connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: created.GetItem().GetId()}))
	wantCode(t, "DB を閉じた後の GetItem()", err, connect.CodeInternal)
	// 内部エラーの詳細 (DB のパス等) は, ログにだけ出力し, 呼び出し側には返さない.
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Message() != "内部エラー" {
		t.Errorf("内部エラーのメッセージ = %q, want %q", ce.Message(), "内部エラー")
	}
}
