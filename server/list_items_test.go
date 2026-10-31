package server

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
)

// listItemIDs は, req から開始して next_page_token が空になるまでページを読み進め, 取得した item の ID を順に返す. 各ページの total_size が wantTotal と一致することも確認する.
func listItemIDs(t *testing.T, c apiv1alpha1connect.ItemServiceClient, req *apiv1alpha1.ListItemsRequest, wantTotal int32) []string {
	t.Helper()
	var ids []string
	for page := 0; ; page++ {
		res, err := c.ListItems(context.Background(), connect.NewRequest(req))
		if err != nil {
			t.Fatalf("%d ページ目の ListItems() error = %v, want nil", page, err)
		}
		if got := res.Msg.GetTotalSize(); got != wantTotal {
			t.Errorf("%d ページ目の total_size = %d, want %d", page, got, wantTotal)
		}
		for _, e := range res.Msg.GetEntries() {
			ids = append(ids, e.GetItem().GetId())
		}
		next := res.Msg.GetNextPageToken()
		if next == "" {
			return ids
		}
		req = proto.CloneOf(req)
		req.PageToken = next
	}
}

func TestListItems_PagesNewestFirstByDefault(t *testing.T) {
	s, c := newTestServer(t)
	var created []*apiv1alpha1.CreateItemResponse
	var oldestFirst []string
	for _, data := range []string{"a", "b", "c"} {
		res := createItem(t, s, c, data)
		created = append(created, res)
		oldestFirst = append(oldestFirst, res.GetItem().GetId())
	}
	newestFirst := slices.Clone(oldestFirst)
	slices.Reverse(newestFirst)

	if got := listItemIDs(t, c, &apiv1alpha1.ListItemsRequest{PageSize: 2}, 3); !slices.Equal(got, newestFirst) {
		t.Errorf("order_by 未指定時の ID = %v, want 新しい順 %v", got, newestFirst)
	}
	if got := listItemIDs(t, c, &apiv1alpha1.ListItemsRequest{PageSize: 2, OrderBy: "item.id"}, 3); !slices.Equal(got, oldestFirst) {
		t.Errorf(`order_by "item.id" の ID = %v, want 古い順 %v`, got, oldestFirst)
	}

	// 一覧の head_revision は, 作成時に返された revision と一致する.
	res, err := c.ListItems(context.Background(), connect.NewRequest(&apiv1alpha1.ListItemsRequest{PageSize: 1, OrderBy: "item.id"}))
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if got, want := res.Msg.GetEntries()[0], created[0]; !proto.Equal(got.GetItem(), want.GetItem()) || !proto.Equal(got.GetHeadRevision(), want.GetRevision()) {
		t.Errorf("1件目 = %v, want item %v と revision %v", got, want.GetItem(), want.GetRevision())
	}
}

func TestListItems_PageTokenMustMatchRequest(t *testing.T) {
	s, c := newTestServer(t)
	for _, data := range []string{"a", "b", "c"} {
		createItem(t, s, c, data)
	}
	ctx := context.Background()
	first, err := c.ListItems(ctx, connect.NewRequest(&apiv1alpha1.ListItemsRequest{PageSize: 1}))
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	token := first.Msg.GetNextPageToken()
	if token == "" {
		t.Fatal("1件ずつ取得したにもかかわらず next_page_token が空です")
	}

	// page_size は途中で変更してよい (AIP-158).
	if _, err := c.ListItems(ctx, connect.NewRequest(&apiv1alpha1.ListItemsRequest{PageSize: 2, PageToken: token})); err != nil {
		t.Errorf("page_size を変更した ListItems() error = %v, want nil", err)
	}

	tests := []struct {
		name string
		req  *apiv1alpha1.ListItemsRequest
	}{
		{name: "order_by が異なる", req: &apiv1alpha1.ListItemsRequest{PageToken: token, OrderBy: "item.id"}},
		{name: "形式が不正", req: &apiv1alpha1.ListItemsRequest{PageToken: "not-a-token"}},
		{name: "checksum 部分を変更", req: &apiv1alpha1.ListItemsRequest{PageToken: replaceByte(token, len(token)-3)}},
	}
	for _, tt := range tests {
		_, err := c.ListItems(ctx, connect.NewRequest(tt.req))
		wantCode(t, tt.name, err, connect.CodeInvalidArgument)
	}
}

// replaceByte は, s の i バイト目を別の base64 文字に置き換える.
func replaceByte(s string, i int) string {
	c := byte('A')
	if s[i] == c {
		c = 'B'
	}
	return s[:i] + string(c) + s[i+1:]
}

func TestListItems_RejectsBadRequest(t *testing.T) {
	_, c := newTestServer(t)
	tests := []struct {
		name string
		req  *apiv1alpha1.ListItemsRequest
	}{
		{name: "負の page_size", req: &apiv1alpha1.ListItemsRequest{PageSize: -1}},
		{name: "item.id 以外の並び順", req: &apiv1alpha1.ListItemsRequest{OrderBy: "item.kind"}},
		{name: "並び順のフィールドが2つ", req: &apiv1alpha1.ListItemsRequest{OrderBy: "item.id, item.id desc"}},
		{name: "order_by の形式が不正", req: &apiv1alpha1.ListItemsRequest{OrderBy: "item.id up"}},
		{name: "asc", req: &apiv1alpha1.ListItemsRequest{OrderBy: "item.id asc"}},
		{name: "filter", req: &apiv1alpha1.ListItemsRequest{Filter: "item.kind = ITEM_KIND_ASSET"}},
	}
	for _, tt := range tests {
		_, err := c.ListItems(context.Background(), connect.NewRequest(tt.req))
		wantCode(t, tt.name, err, connect.CodeInvalidArgument)
	}
}

func TestPageSize_DefaultAndCap(t *testing.T) {
	tests := []struct {
		in   int32
		want int
	}{
		{in: 0, want: defaultPageSize},
		{in: 1, want: 1},
		{in: maxPageSize, want: maxPageSize},
		{in: maxPageSize + 1, want: maxPageSize},
	}
	for _, tt := range tests {
		if got, err := pageSize(tt.in); err != nil || got != tt.want {
			t.Errorf("pageSize(%d) = %d, %v, want %d, nil", tt.in, got, err, tt.want)
		}
	}
}

func TestListItems_CanBeCalledWithHTTPGet(t *testing.T) {
	s, hs := startTestServer(t)
	c := apiv1alpha1connect.NewItemServiceClient(hs.Client(), hs.URL, connect.WithHTTPGet())
	createItem(t, s, c, "a")

	// NO_SIDE_EFFECTS を指定しているため, Connect クライアントは GET で呼び出す. ブラウザや中継キャッシュを利用できる.
	res, err := c.ListItems(context.Background(), connect.NewRequest(&apiv1alpha1.ListItemsRequest{}))
	if err != nil {
		t.Fatalf("GET による ListItems() error = %v, want nil", err)
	}
	if got := res.Msg.GetTotalSize(); got != 1 {
		t.Errorf("total_size = %d, want 1", got)
	}
}
