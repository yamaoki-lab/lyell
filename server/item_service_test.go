package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// newTestServer は, startTestServer でテスト用サーバを起動し, ItemService のクライアントを返す.
func newTestServer(t *testing.T) (*Server, apiv1alpha1connect.ItemServiceClient) {
	t.Helper()
	s, hs := startTestServer(t)
	return s, apiv1alpha1connect.NewItemServiceClient(hs.Client(), hs.URL)
}

// startTestServer は, 一時ディレクトリに DB と blob の保存先を作成し, Register した mux で httptest のサーバを起動する.
func startTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), Config{DBPath: filepath.Join(dir, "lyell.db"), BlobDir: filepath.Join(dir, "blobs")})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	// Cleanup は登録と逆の順に実行されるので, サーバを停止してから DB を閉じる.
	t.Cleanup(func() { _ = s.Close() })
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return s, hs
}

// putBlob は, data を blob として保存し, その SHA-256 を API の表記で返す. blob の HTTP エンドポイントを経由せずに, blobstore へ直接保存する.
func putBlob(t *testing.T, s *Server, data string) string {
	t.Helper()
	sum := blobstore.Sum(sha256.Sum256([]byte(data)))
	if _, err := s.blobs.Put(context.Background(), sum, strings.NewReader(data)); err != nil {
		t.Fatalf("blob を保存できません: %v", err)
	}
	return sum.String()
}

func createItem(t *testing.T, s *Server, c apiv1alpha1connect.ItemServiceClient, data string) *apiv1alpha1.CreateItemResponse {
	t.Helper()
	res, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, data)}}))
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

// wantViolation は, err が INVALID_ARGUMENT で, 詳細情報の BadRequest に, field と reason の組が1つだけ入っていることを確かめる. reason が空なら, reason が設定されていないことを確かめる.
func wantViolation(t *testing.T, name string, err error, field, reason string) {
	t.Helper()
	wantCode(t, name, err, connect.CodeInvalidArgument)
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return
	}
	var got []*errdetails.BadRequest_FieldViolation
	for _, d := range ce.Details() {
		if v, err := d.Value(); err == nil {
			if br, ok := v.(*errdetails.BadRequest); ok {
				got = append(got, br.GetFieldViolations()...)
			}
		}
	}
	if len(got) != 1 || got[0].GetField() != field || got[0].GetReason() != reason || got[0].GetDescription() == "" {
		t.Errorf("%s の BadRequest = %v, want field %q, reason %q と空でない description の1件", name, got, field, reason)
	}
}

func TestFieldViolationReason_Names(t *testing.T) {
	// 値の名前から接頭辞を除いた reason は, google.rpc.BadRequest の決まり (63文字以下の UPPER_SNAKE_CASE) を満たす必要がある.
	valid := regexp.MustCompile(`^[A-Z][A-Z0-9_]+[A-Z0-9]$`)
	for n, name := range apiv1alpha1.FieldViolationReason_name {
		if n == 0 {
			continue
		}
		got := reasonName(apiv1alpha1.FieldViolationReason(n))
		if !strings.HasPrefix(name, reasonPrefix) || len(got) > 63 || !valid.MatchString(got) {
			t.Errorf("%s の reason = %q, want %s で始まる名前から接頭辞を除いた, 63文字以下の UPPER_SNAKE_CASE", name, got, reasonPrefix)
		}
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
		// field と reason は, want が INVALID_ARGUMENT の時に BadRequest で返すもの.
		field, reason string
	}{
		{name: "種類が指定されていない", req: &apiv1alpha1.CreateItemRequest{Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "kind", reason: "KIND_UNSPECIFIED"},
		{name: "存在しない種類", req: &apiv1alpha1.CreateItemRequest{Kind: 99, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "kind", reason: "KIND_UNKNOWN"},
		{name: "collection", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "kind", reason: "KIND_UNSUPPORTED"},
		{name: "SHA-256 が大文字", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: strings.ToUpper(sum)}}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_MALFORMED"},
		{name: "SHA-256 が短い", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum[:63]}}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_MALFORMED"},
		{name: "SHA-256 が空", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{}}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_REQUIRED"},
		{name: "content が無い", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_REQUIRED"},
		{name: "blob が存在しない", req: &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: missing}}, want: connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateItem(ctx, connect.NewRequest(tt.req))
			if tt.want == connect.CodeInvalidArgument {
				wantViolation(t, "CreateItem()", err, tt.field, tt.reason)
				return
			}
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
		{name: "空", id: "", want: connect.CodeInvalidArgument},
		{name: "ID の形式でない", id: "item", want: connect.CodeInvalidArgument},
		// uuid.Parse は受け付けるが, API の表記ではない.
		{name: "波括弧付き", id: "{" + unknown.String() + "}", want: connect.CodeInvalidArgument},
		{name: "ハイフン無し", id: strings.ReplaceAll(unknown.String(), "-", ""), want: connect.CodeInvalidArgument},
		// 36文字だが, 応答で返す表記 (小文字) と異なる.
		{name: "大文字", id: "0190A0B0-C0D0-7000-8000-000000000001", want: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: tt.id}))
			_, rerr := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: tt.id}))
			if tt.want == connect.CodeInvalidArgument {
				wantViolation(t, "GetItem()", err, "id", "ID_MALFORMED")
				wantViolation(t, "GetRevision()", rerr, "id", "ID_MALFORMED")
				return
			}
			wantCode(t, "GetItem()", err, tt.want)
			wantCode(t, "GetRevision()", rerr, tt.want)
		})
	}
}

func TestCommitRevision_AdvancesHead(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	created := createItem(t, s, c, "r1")
	r1 := created.GetItem().GetHead()

	// parents を指定しなければ, expected_head だけが親になる.
	res, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r2")}}))
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
	res, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r2")}}))
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	r2 := res.Msg.GetRevision().GetId()

	// head は r2 に進んでいるが, 古い r1 を前提として追加しようとする.
	_, err = c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "stale")}}))
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

	other := createItem(t, s, c, "other").GetItem().GetHead()
	another := createItem(t, s, c, "another").GetItem().GetHead()

	tests := []struct {
		name string
		req  *apiv1alpha1.CommitRevisionRequest
		want connect.Code
		// field と reason は, want が INVALID_ARGUMENT の時に BadRequest で返すもの.
		field, reason string
	}{
		{name: "item の ID の形式でない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: "item", ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "item_id", reason: "ID_MALFORMED"},
		{name: "expected_head が空", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "expected_head", reason: "ID_MALFORMED"},
		{name: "親の ID の形式でない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, "parent"}, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "parents[1]", reason: "ID_MALFORMED"},
		{name: "SHA-256 が大文字", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: strings.ToUpper(sum)}}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_MALFORMED"},
		{name: "content が無い", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1}, want: connect.CodeInvalidArgument, field: "content.blob_sha256", reason: "BLOB_SHA256_REQUIRED"},
		{name: "親が重複している", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, r1}, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "parents[1]", reason: "PARENTS_DUPLICATE"},
		{name: "親が上限を超える", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, other, another}, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "parents", reason: "PARENTS_TOO_MANY"},
		{name: "親に expected_head が含まれていない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{other}, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeInvalidArgument, field: "parents", reason: "PARENTS_MISSING_EXPECTED_HEAD"},
		{name: "存在しない revision を親にする", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Parents: []string{r1, unknown}, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeNotFound},
		{name: "存在しない item", req: &apiv1alpha1.CommitRevisionRequest{ItemId: unknown, ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum}}, want: connect.CodeNotFound},
		{name: "blob が存在しない", req: &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1, Content: &apiv1alpha1.RevisionContent{BlobSha256: blobstore.Sum(sha256.Sum256([]byte("missing"))).String()}}, want: connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CommitRevision(ctx, connect.NewRequest(tt.req))
			if tt.want == connect.CodeInvalidArgument {
				wantViolation(t, "CommitRevision()", err, tt.field, tt.reason)
				return
			}
			wantCode(t, "CommitRevision()", err, tt.want)
		})
	}
}

func TestLimitsMessageSize(t *testing.T) {
	_, hs := startTestServer(t)
	// gzip で送るため, 展開後のサイズが上限を超えていても, 送信するバイト数は少ない.
	c := apiv1alpha1connect.NewItemServiceClient(hs.Client(), hs.URL, connect.WithSendGzip())

	tests := []struct {
		name string
		size int
		want connect.Code
	}{
		{name: "上限を超える", size: maxMessageBytes + 1, want: connect.CodeResourceExhausted},
		// 上限未満なら, バリデーションまで到達する.
		{name: "上限未満", size: maxMessageBytes / 2, want: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.GetItem(context.Background(), connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: strings.Repeat("a", tt.size)}))
			wantCode(t, "GetItem()", err, tt.want)
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
