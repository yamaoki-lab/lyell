package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
	"github.com/yamaoki-lab/lyell/internal/itemname"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
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

func TestNameReasons_EveryRuleHasReason(t *testing.T) {
	for r := itemname.RuleForbiddenCharacter; r <= itemname.RuleBlank; r++ {
		if nameReasons[r] == apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_UNSPECIFIED {
			t.Errorf("規則 %d の拒否理由がありません", r)
		}
	}
}

func TestRevisionName_IsFixedAndReturned(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()

	created := createNamed(t, s, c, "r1", " 撮影\u200Bの記録\t1 ")
	if got := created.GetRevision().GetContent().GetName(); got != "撮影の記録 1" {
		t.Errorf("CreateItem() の name = %+q, want %+q", got, "撮影の記録 1")
	}
	r2 := commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: created.GetRevision().GetId(), Content: &apiv1alpha1.RevisionContent{Name: "e\u0301\r\n"}})
	if got := r2.GetContent().GetName(); got != "\u00E9" {
		t.Errorf("CommitRevision() の name = %+q, want %+q", got, "\u00E9")
	}
	// 保存されるのも修正後の名前.
	got, err := c.GetRevision(ctx, connect.NewRequest(&apiv1alpha1.GetRevisionRequest{Id: r2.GetId()}))
	if err != nil {
		t.Fatalf("GetRevision() error = %v, want nil", err)
	}
	if name := got.Msg.GetRevision().GetContent().GetName(); name != "\u00E9" {
		t.Errorf("GetRevision() の name = %+q, want %+q", name, "\u00E9")
	}
}

func TestRevisionName_RejectsEveryViolatedRule(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()

	sum := putBlob(t, s, "r1")
	create := &apiv1alpha1.CreateItemRequest{Kind: apiv1alpha1.ItemKind_ITEM_KIND_ASSET, Content: &apiv1alpha1.RevisionContent{BlobSha256: sum, Name: "nul.a:\u202E."}}
	_, err := c.CreateItem(ctx, connect.NewRequest(create))
	wantViolations(t, "規則に違反する名前の CreateItem()", err, "content.name", "NAME_FORBIDDEN_CHARACTER", "NAME_BIDI_CONTROL", "NAME_TRAILING_PERIOD", "NAME_RESERVED")
	wantNameSuggestion(t, err, "nul_.a__")
	wantCreateValidationError(t, c, create, err)

	create.Content.Name = " "
	_, err = c.CreateItem(ctx, connect.NewRequest(create))
	wantViolation(t, "名前が空白のみの CreateItem()", err, "content.name", "NAME_BLANK")
	wantNameSuggestion(t, err, "")
	wantCreateValidationError(t, c, create, err)

	list, err := c.ListItems(ctx, connect.NewRequest(&apiv1alpha1.ListItemsRequest{}))
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if n := len(list.Msg.GetEntries()); n != 0 {
		t.Errorf("拒否した後の item の数 = %d, want 0", n)
	}

	created := createNamed(t, s, c, "r1", "名前")
	itemID, head := created.GetItem().GetId(), created.GetRevision().GetId()
	tests := []struct {
		name       string
		content    *apiv1alpha1.RevisionContent
		mask       []string
		reason     string
		suggestion string
	}{
		{name: "長すぎる名前", content: &apiv1alpha1.RevisionContent{Name: strings.Repeat("a", 256)}, reason: "NAME_TOO_LONG", suggestion: strings.Repeat("a", 255)},
		{name: "空白だけの名前", content: &apiv1alpha1.RevisionContent{Name: " \t "}, reason: "NAME_BLANK"},
		{name: "update_mask に name を指定した空白だけの名前", content: &apiv1alpha1.RevisionContent{Name: " "}, mask: []string{"name"}, reason: "NAME_BLANK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &apiv1alpha1.CommitRevisionRequest{ItemId: itemID, ExpectedHead: head, Content: tt.content}
			if tt.mask != nil {
				req.UpdateMask = mask(tt.mask...)
			}
			_, err := c.CommitRevision(ctx, connect.NewRequest(req))
			wantViolations(t, "CommitRevision()", err, "content.name", tt.reason)
			wantNameSuggestion(t, err, tt.suggestion)
			wantCommitValidationError(t, c, req, err)
		})
	}
	item, err := c.GetItem(ctx, connect.NewRequest(&apiv1alpha1.GetItemRequest{Id: itemID}))
	if err != nil {
		t.Fatalf("GetItem() error = %v, want nil", err)
	}
	if got := item.Msg.GetItem().GetHead(); got != head {
		t.Errorf("拒否した後の head = %s, want %s (変わらない)", got, head)
	}
}

// wantNameSuggestion は, BadRequest に続いて指定の候補が付与されることを検証する. want が空なら, 詳細情報が BadRequest のみであることを検証する.
func wantNameSuggestion(t *testing.T, err error, want string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want *connect.Error", err)
	}
	details := ce.Details()
	if want == "" {
		if len(details) != 1 {
			t.Errorf("詳細情報 = %v, want BadRequest のみ", details)
		}
		return
	}
	if len(details) != 2 {
		t.Fatalf("詳細情報 = %v, want BadRequest と NameSuggestion", details)
	}
	v, err := details[1].Value()
	if err != nil {
		t.Fatalf("NameSuggestion の読み取り = %v", err)
	}
	got, ok := v.(*apiv1alpha1.NameSuggestion)
	if !ok || got.GetName() != want {
		t.Errorf("NameSuggestion = %v, want name %q", v, want)
	}
}

func TestCommitRevision_DoesNotRecheckInheritedName(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	item := createItem(t, s, c, "r1").GetItem()

	// 規則が変わる前に記録された名前の代わりに, 規則に違反する名前の revision を, itemstore に直接記録する.
	body, err := withName(nil, "a:b")
	if err != nil {
		t.Fatalf("withName() error = %v, want nil", err)
	}
	data := "r2"
	r2, err := s.items.CommitRevision(ctx, itemstore.NewRevision{
		ItemID: itemstore.ID(uuid.MustParse(item.GetId())), From: itemstore.ID(uuid.MustParse(item.GetHead())),
		Content: itemstore.Content{CreatedAt: time.Now(), AuthorUser: "old", AuthorMachine: "old", Blob: blobstore.Sum(sha256.Sum256([]byte(data))), Body: body},
		Data:    strings.NewReader(data),
	})
	if err != nil {
		t.Fatalf("itemstore の CommitRevision() error = %v, want nil", err)
	}

	r3 := commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: item.GetId(), ExpectedHead: r2.String(), Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r3")}})
	if got := r3.GetContent().GetName(); got != "a:b" {
		t.Errorf("引き継いだ name = %q, want %q", got, "a:b")
	}

	// update_mask に含まれない name は変更しないので, 規則に違反していても検証しない.
	r4 := commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: item.GetId(), ExpectedHead: r3.GetId(), Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r4"), Name: "c:d"}, UpdateMask: mask("blob_sha256")})
	if got := r4.GetContent().GetName(); got != "a:b" {
		t.Errorf("update_mask に name を含めない更新の後の name = %q, want %q", got, "a:b")
	}
}

func TestCommitRevision_ComparesFixedName(t *testing.T) {
	s, c := newTestServer(t)
	created := createNamed(t, s, c, "r1", "名前")
	r1 := created.GetRevision()

	// 修正すると同じ名前になるなら, 変更のない revision として拒否する.
	_, err := c.CommitRevision(context.Background(), connect.NewRequest(&apiv1alpha1.CommitRevisionRequest{ItemId: created.GetItem().GetId(), ExpectedHead: r1.GetId(), Content: &apiv1alpha1.RevisionContent{BlobSha256: r1.GetContent().GetBlobSha256(), Name: " 名前\u200B"}}))
	wantViolation(t, "修正すると同じ名前になる CommitRevision()", err, "content", "REVISION_UNCHANGED")
}
