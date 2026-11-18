package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

func TestCommitRevision_KeepsFieldsOfBodyThatThisLyellDoesNotKnow(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()
	item := createItem(t, s, c, "r1").GetItem()

	// 新しい Lyell が, この Lyell が知らないフィールドを body に書いた revision を, itemstore に直接記録する.
	unknown := protowire.AppendString(protowire.AppendTag(nil, 1000, protowire.BytesType), "新しい Lyell のフィールド")
	itemID, head := uuid.MustParse(item.GetId()), uuid.MustParse(item.GetHead())
	data := "r2"
	sum := blobstore.Sum(sha256.Sum256([]byte(data)))
	r2, err := s.items.CommitRevision(ctx, itemstore.NewRevision{
		ItemID: itemstore.ID(itemID), From: itemstore.ID(head),
		Content: itemstore.Content{CreatedAt: time.Now(), AuthorUser: "new", AuthorMachine: "new", Blob: sum, Body: unknown},
		Data:    strings.NewReader(data),
	})
	if err != nil {
		t.Fatalf("itemstore の CommitRevision() error = %v, want nil", err)
	}

	// この Lyell の API でその上にコミットしても, 知らないフィールドは新しい body に残る.
	r3 := commit(t, c, &apiv1alpha1.CommitRevisionRequest{ItemId: item.GetId(), ExpectedHead: r2.String(), Content: &apiv1alpha1.RevisionContent{BlobSha256: putBlob(t, s, "r3")}})
	rev, err := s.items.Revision(ctx, itemstore.ID(uuid.MustParse(r3.GetId())))
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	var body storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(rev.Content.Body, &body); err != nil {
		t.Fatalf("body を読めません: %v", err)
	}
	if !bytes.Contains(body.ProtoReflect().GetUnknown(), unknown) {
		t.Errorf("新しい body の知らないフィールド = %v (want %v を含む)", body.ProtoReflect().GetUnknown(), unknown)
	}
	if body.GetAuthorUser() != "anonymous" {
		t.Errorf("新しい body の利用者 = %q, want %q (サーバが決める値)", body.GetAuthorUser(), "anonymous")
	}
}
