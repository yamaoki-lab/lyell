package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
)

// putBlobHTTP は, data を blob の PUT で保存し, その SHA-256 を API の表記で返す.
func putBlobHTTP(t *testing.T, base, data string) string {
	t.Helper()
	url := blobURL(base, data)
	if res := do(t, http.MethodPut, url, data, nil); res.status != http.StatusCreated {
		t.Fatalf("PUT %q の status = %d, want 201", data, res.status)
	}
	return url[strings.LastIndex(url, "/")+1:]
}

// checkout は, item の head の revision を取得し, その中身を blob の GET で取り出す. head の revision の ID と中身を返す.
func checkout(t *testing.T, base string, c apiv1alpha1connect.ItemServiceClient, itemID string) (head, data string) {
	t.Helper()
	ctx := context.Background()
	item, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: itemID}))
	if err != nil {
		t.Fatalf("GetItem() error = %v, want nil", err)
	}
	head = item.Msg.GetItem().GetHead()
	rev, err := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: head}))
	if err != nil {
		t.Fatalf("GetRevision() error = %v, want nil", err)
	}
	res := do(t, http.MethodGet, base+"/lyell/api/v1alpha1/blobs/sha256/"+rev.Msg.GetRevision().GetContent().GetBlobSha256(), "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("head の blob の GET の status = %d, want 200", res.status)
	}
	return head, res.body
}

// TestWorkingCopy_RoundTrip は, 作業コピーの一連の処理を, 外部の呼び出し側と同じく blob の HTTP と ItemService だけで行う. blob を保存 → item を作成 → 取り出す → 書き換えて新しい revision としてコミットする → 古い head を前提とした CommitRevision が ABORTED になる.
func TestWorkingCopy_RoundTrip(t *testing.T) {
	_, hs := startTestServer(t)
	base := hs.URL
	c := apiv1alpha1connect.NewItemServiceClient(hs.Client(), base)
	ctx := context.Background()

	created, err := c.CreateItem(ctx, connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, BlobSha256: putBlobHTTP(t, base, "v1")}))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	itemID := created.Msg.GetItem().GetId()

	head, data := checkout(t, base, c, itemID)
	if data != "v1" {
		t.Fatalf("取り出した中身 = %q, want %q", data, "v1")
	}

	// 書き換えた中身を保存し, 取り出した時の head を前提として, 新しい revision をコミットする.
	res, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: head, BlobSha256: putBlobHTTP(t, base, "v2")}))
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	r2 := res.Msg.GetRevision().GetId()
	if newHead, data := checkout(t, base, c, itemID); newHead != r2 || data != "v2" {
		t.Fatalf("コミットした後の head と中身 = %s %q, want %s %q", newHead, data, r2, "v2")
	}

	// 別の作業コピーが, 古い head を前提としてコミットしようとする. head は更新されない.
	_, err = c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: head, BlobSha256: putBlobHTTP(t, base, "v3")}))
	wantCode(t, "古い head を前提とした CommitRevision()", err, connect.CodeAborted)
	if newHead, data := checkout(t, base, c, itemID); newHead != r2 || data != "v2" {
		t.Errorf("拒否された後の head と中身 = %s %q, want %s %q", newHead, data, r2, "v2")
	}
}
