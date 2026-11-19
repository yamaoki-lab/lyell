package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
)

// createNamed は, data を中身とし, name を名前とする asset を作成する.
func createNamed(t *testing.T, s *Server, c apiv1alpha1connect.ItemServiceClient, data, name string) *apiv1alpha1.CreateItemResponse {
	t.Helper()
	res, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, data), Name: name}}))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	return res.Msg
}

func TestRevisionName_EveryRPCReturnsStoredName(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()

	created := createNamed(t, s, c, "r1", "名前")
	if got := created.GetRevision().GetContent().GetName(); got != "名前" {
		t.Errorf("CreateItem() の name = %q, want %q", got, "名前")
	}
	got, err := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: created.GetRevision().GetId()}))
	if err != nil {
		t.Fatalf("GetRevision() error = %v, want nil", err)
	}
	if name := got.Msg.GetRevision().GetContent().GetName(); name != "名前" {
		t.Errorf("GetRevision() の name = %q, want %q", name, "名前")
	}
	r2 := commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: created.GetRevision().GetId(), Content: &apiv1alpha1.RevisionContent{Name: "新しい名前"}})
	if name := r2.GetContent().GetName(); name != "新しい名前" {
		t.Errorf("CommitRevision() の name = %q, want %q", name, "新しい名前")
	}
	list, err := c.ListItems(ctx, connect.NewRequest(&apiv1alpha1.ListItemsRequest{}))
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if n := len(list.Msg.GetEntries()); n != 1 {
		t.Fatalf("ListItems() の件数 = %d, want 1", n)
	}
	if name := list.Msg.GetEntries()[0].GetHeadRevision().GetContent().GetName(); name != "新しい名前" {
		t.Errorf("ListItems() の head_revision.content.name = %q, want %q", name, "新しい名前")
	}
}

func TestCommitRevision_InheritsChangesAndClearsName(t *testing.T) {
	s, c := newTestServer(t)

	tests := []struct {
		name    string
		content *apiv1alpha1.RevisionContent
		mask    []string
		want    string
	}{
		{name: "name を省略すると引き継ぐ", content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r2")}, want: "前の名前"},
		{name: "name を設定すると変わる", content: &apiv1alpha1.RevisionContent{Name: "新しい名前"}, want: "新しい名前"},
		{name: "update_mask に name を指定して空にすると消える", mask: []string{"name"}, want: ""},
		// update_mask を指定すると, それに含まれないフィールドは, content に値があっても変更しない.
		{name: "update_mask に含まれない name は引き継ぐ", content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r2"), Name: "無視される名前"}, mask: []string{"blob_sha256"}, want: "前の名前"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created := createNamed(t, s, c, "r1", "前の名前")
			req := &apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: created.GetRevision().GetId(), Content: tt.content}
			if tt.mask != nil {
				req.UpdateMask = mask(tt.mask...)
			}
			if got := commit(t, c, req).GetContent().GetName(); got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommitRevision_RejectsSameBlobAndName(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	created := createNamed(t, s, c, "r1", "名前")
	itemID, r1 := created.GetItem().GetId(), created.GetRevision()
	same := r1.GetContent().GetBlobSha256()

	_, err := c.CommitRevision(ctx, connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1.GetId(), Content: &apiv1alpha1.RevisionContent{BlobSha256: same, Name: "名前"}}))
	wantViolation(t, "同じ blob と名前の CommitRevision()", err, "content", "REVISION_UNCHANGED")

	// blob が同じでも, 名前が変われば変更として記録する.
	commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: r1.GetId(), Content: &apiv1alpha1.RevisionContent{BlobSha256: same, Name: "別の名前"}})
}
