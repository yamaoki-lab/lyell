package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

// createExtensionItem は, API の CreateItem がまだ extension の item を拒否するので, itemstore を直接呼んで作成する.
func createExtensionItem(t *testing.T, s *Server, name string, data []byte) itemstore.Item {
	t.Helper()
	body, err := proto.Marshal(&storagev1alpha1.RevisionBody{ExtensionData: data})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.items.CreateItem(context.Background(), itemstore.NewItem{
		Kind: itemstore.KindExtension, Extension: name,
		Content: itemstore.Content{CreatedAt: time.Now(), AuthorUser: "u", AuthorMachine: "m", Body: body},
	})
	if err != nil {
		t.Fatalf("itemstore.CreateItem() error = %v, want nil", err)
	}
	return item
}

func TestExtensionItem_ReturnedThroughAPI(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	data := []byte{1, 2, 3}
	ext := createExtensionItem(t, s, "lyell.clock", data)
	createItem(t, s, c, "asset")

	got, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: ext.ID.String()}))
	if err != nil {
		t.Fatalf("GetItem() error = %v", err)
	}
	if got.Msg.Item.Kind != apiv1alpha1.ItemKind_ITEM_KIND_EXTENSION || got.Msg.Item.Extension != "lyell.clock" {
		t.Errorf("GetItem() item = %v, want extension kind with lyell.clock", got.Msg.Item)
	}

	rev, err := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: ext.Head.String()}))
	if err != nil {
		t.Fatalf("GetRevision() error = %v", err)
	}
	if !bytes.Equal(rev.Msg.Revision.Content.ExtensionData, data) || rev.Msg.Revision.Content.BlobSha256 != "" {
		t.Errorf("GetRevision() content = %v, want extension_data %v without blob", rev.Msg.Revision.Content, data)
	}

	list, err := c.ListItems(ctx, connect.NewRequest(&apiv1alpha1.ListItemsRequest{Filter: "item.kind = ITEM_KIND_EXTENSION"}))
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(list.Msg.Entries) != 1 {
		t.Fatalf("ListItems() = %d 件, want 1", len(list.Msg.Entries))
	}
	e := list.Msg.Entries[0]
	if e.Item.Extension != "lyell.clock" || e.Item.Kind != apiv1alpha1.ItemKind_ITEM_KIND_EXTENSION {
		t.Errorf("ListItems() item = %v, want extension lyell.clock", e.Item)
	}
	if e.HeadRevision.Content == nil || e.HeadRevision.Content.ExtensionData != nil {
		t.Errorf("ListItems() content = %v, want content without extension_data", e.HeadRevision.Content)
	}
}

func TestCreateItem_RejectsExtensionKind(t *testing.T) {
	_, c := newTestServer(t)
	_, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_EXTENSION, Extension: "lyell.clock", Content: &apiv1alpha1.RevisionContent{ExtensionData: []byte{1}}}))
	wantViolation(t, "CreateItem(extension)", err, "kind", "KIND_UNSUPPORTED")
}

func TestCreateItem_RejectsExtensionFieldsForAsset(t *testing.T) {
	s, c := newTestServer(t)
	sha := putBlob(t, s, "a")
	asset := apiv1alpha1.ItemKind_ITEM_KIND_ASSET
	for _, validateOnly := range []bool{false, true} {
		_, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: asset, Extension: "lyell.clock", Content: &apiv1alpha1.RevisionContent{BlobSha256: sha}, ValidateOnly: validateOnly}))
		wantViolation(t, "CreateItem(extension)", err, "extension", "EXTENSION_FOR_ASSET")
		_, err = c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: asset, Content: &apiv1alpha1.RevisionContent{BlobSha256: sha, ExtensionData: []byte{1}}, ValidateOnly: validateOnly}))
		wantViolation(t, "CreateItem(extension_data)", err, "content.extension_data", "EXTENSION_DATA_FOR_ASSET")
	}
	// asset では引き続き blob が必須.
	_, err := c.CreateItem(context.Background(), connect.NewRequest(&apiv1alpha1.CreateItemRequest{Kind: asset, Content: &apiv1alpha1.RevisionContent{}}))
	wantViolation(t, "CreateItem(blob なし)", err, "content.blob_sha256", "BLOB_SHA256_REQUIRED")
}

func TestCommitRevision_RejectsExtensionDataForAsset(t *testing.T) {
	s, c := newTestServer(t)
	created := createItem(t, s, c, "a")
	base := func() *apiv1alpha1.CommitRevisionRequest {
		return &apiv1alpha1.CommitRevisionRequest{ItemId: created.Item.Id, ExpectedHead: created.Item.Head}
	}
	tests := []struct {
		name   string
		modify func(*apiv1alpha1.CommitRevisionRequest)
	}{
		{"値を設定", func(r *apiv1alpha1.CommitRevisionRequest) {
			r.Content = &apiv1alpha1.RevisionContent{ExtensionData: []byte{1}}
		}},
		{"update_mask で指定", func(r *apiv1alpha1.CommitRevisionRequest) {
			r.Content = &apiv1alpha1.RevisionContent{Name: "x", ExtensionData: []byte{1}}
			r.UpdateMask = &fieldmaskpb.FieldMask{Paths: []string{"name", "extension_data"}}
		}},
		{"update_mask で空にする", func(r *apiv1alpha1.CommitRevisionRequest) {
			r.Content = &apiv1alpha1.RevisionContent{Name: "x"}
			r.UpdateMask = &fieldmaskpb.FieldMask{Paths: []string{"name", "extension_data"}}
		}},
		{"validate_only", func(r *apiv1alpha1.CommitRevisionRequest) {
			r.Content = &apiv1alpha1.RevisionContent{ExtensionData: []byte{1}}
			r.ValidateOnly = true
		}},
	}
	for _, tt := range tests {
		req := base()
		tt.modify(req)
		_, err := c.CommitRevision(context.Background(), connect.NewRequest(req))
		wantViolation(t, "CommitRevision("+tt.name+")", err, "content.extension_data", "EXTENSION_DATA_FOR_ASSET")
	}
	// asset では引き続き blob を空にできない.
	req := base()
	req.UpdateMask = &fieldmaskpb.FieldMask{Paths: []string{"blob_sha256"}}
	_, err := c.CommitRevision(context.Background(), connect.NewRequest(req))
	wantViolation(t, "CommitRevision(blob を空に)", err, "content.blob_sha256", "BLOB_SHA256_REQUIRED")
}

func TestCommitRevision_RejectsExtensionItem(t *testing.T) {
	s, c := newTestServer(t)
	ext := createExtensionItem(t, s, "lyell.clock", []byte{1, 2, 3})
	// extension_data を変えずに名前だけを変更しても拒否する.
	for _, content := range []*apiv1alpha1.RevisionContent{{ExtensionData: []byte{9}}, {Name: "x"}} {
		for _, validateOnly := range []bool{false, true} {
			_, err := c.CommitRevision(context.Background(), connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{
				ItemId: ext.ID.String(), ExpectedHead: ext.Head.String(), Content: content, ValidateOnly: validateOnly,
			}))
			wantViolation(t, "CommitRevision(extension の item)", err, "item_id", "KIND_UNSUPPORTED")
		}
	}
	got, err := s.items.Item(context.Background(), ext.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Head != ext.Head {
		t.Errorf("拒否した後の head = %s, want %s", got.Head, ext.Head)
	}
}
