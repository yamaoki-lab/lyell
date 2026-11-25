package itemstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// failingReader は, 読まれるとエラーを返す. blob を書き込まないはずの場面で, 書き込もうとしたことを検知する.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Errorf("blob を持たない item の Data が読まれた")
	return 0, errors.New("読まれてはならない")
}

// extensionContent は, data を extension_data に持ち, blob を持たない Content を作る.
func extensionContent(t *testing.T, data []byte) Content {
	t.Helper()
	c := testContent("")
	c.Blob = blobstore.Sum{}
	body, err := proto.Marshal(&storagev1alpha1.RevisionBody{ExtensionData: data})
	if err != nil {
		t.Fatalf("body を作れない: %v", err)
	}
	c.Body = body
	return c
}

func extensionDataOf(t *testing.T, c *Content) []byte {
	t.Helper()
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(c.Body, &b); err != nil {
		t.Fatalf("body を読めない: %v", err)
	}
	return b.GetExtensionData()
}

func TestExtensionItem_SavesAndReadsNameAndData(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	item, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, []byte("v1")), Data: failingReader{t}})
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	if item.Kind != KindExtension || item.Extension != "lyell.clock" {
		t.Errorf("CreateItem() = %+v, want extension, lyell.clock", item)
	}
	got, err := s.Item(ctx, item.ID)
	if err != nil {
		t.Fatalf("Item() error = %v, want nil", err)
	}
	if got != item {
		t.Errorf("Item() = %+v, want %+v", got, item)
	}

	rev2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: extensionContent(t, []byte("v2")), Data: failingReader{t}})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	for id, want := range map[ID]string{item.Head: "v1", rev2: "v2"} {
		rev, err := s.Revision(ctx, id)
		if err != nil {
			t.Fatalf("Revision(%s) error = %v, want nil", id, err)
		}
		if rev.Content.Blob != (blobstore.Sum{}) || rev.Content.BlobSize != 0 {
			t.Errorf("Revision(%s) の blob = %s, %d, want 無し", id, rev.Content.Blob, rev.Content.BlobSize)
		}
		if d := extensionDataOf(t, rev.Content); string(d) != want {
			t.Errorf("Revision(%s) の extension_data = %q, want %q", id, d, want)
		}
	}
	var nulls int
	if err := s.read.QueryRow("SELECT count(*) FROM revisions WHERE blob_sha256 IS NULL AND blob_size IS NULL").Scan(&nulls); err != nil || nulls != 2 {
		t.Errorf("blob の列が NULL の revision = %d (error = %v), want 2", nulls, err)
	}
	assertColumnsMatchBody(t, s)

	list, err := s.ListItems(ctx, ListOptions{Kind: KindExtension, Limit: 10})
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Item.Extension != "lyell.clock" || list.Entries[0].Head.ID != rev2 {
		t.Errorf("ListItems() = %+v, want lyell.clock の item 1件 (head %s)", list.Entries, rev2)
	}
	if d := extensionDataOf(t, list.Entries[0].Head.Content); string(d) != "v2" {
		t.Errorf("ListItems() の head の extension_data = %q, want v2", d)
	}
}

func TestAssetItem_HasNoExtensionName(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	var null bool
	if err := s.read.QueryRow("SELECT extension IS NULL FROM items WHERE id = ?", item.ID[:]).Scan(&null); err != nil || !null {
		t.Errorf("asset の extension の列が NULL か = %v (error = %v), want true", null, err)
	}
	list, err := s.ListItems(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Item.Extension != "" {
		t.Errorf("ListItems() = %+v, want 名前の無い item 1件", list.Entries)
	}
}

func TestCreateItem_RejectsWrongKindNameBlobCombinations(t *testing.T) {
	assetWithData := testContent("a")
	assetWithData.Body = extensionContent(t, []byte("x")).Body
	extWithBlob := extensionContent(t, nil)
	extWithBlob.Blob = testContent("a").Blob
	tests := []struct {
		name string
		item NewItem
	}{
		{"extension に名前が無い", NewItem{Kind: KindExtension, Content: extensionContent(t, nil)}},
		{"extension が blob を持つ", NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extWithBlob, Data: strings.NewReader("a")}},
		{"asset が名前を持つ", NewItem{Kind: KindAsset, Extension: "lyell.clock", Content: testContent("a"), Data: strings.NewReader("a")}},
		{"asset が blob を持たない", NewItem{Kind: KindAsset, Content: extensionContent(t, nil)}},
		{"asset が extension_data を持つ", NewItem{Kind: KindAsset, Content: assetWithData, Data: strings.NewReader("a")}},
		{"collection が名前を持つ", NewItem{Kind: KindCollection, Extension: "lyell.clock", Content: testContent("a"), Data: strings.NewReader("a")}},
		{"collection が blob を持たない", NewItem{Kind: KindCollection, Content: extensionContent(t, nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			ctx := context.Background()
			if _, err := s.CreateItem(ctx, tt.item); !errors.Is(err, ErrInvalid) {
				t.Errorf("CreateItem() error = %v, want ErrInvalid", err)
			}
			if _, err := s.ValidateCreateItem(ctx, tt.item.Kind, tt.item.Extension, tt.item.Content, true); !errors.Is(err, ErrInvalid) {
				t.Errorf("ValidateCreateItem() error = %v, want ErrInvalid", err)
			}
			var n int
			if err := s.read.QueryRow("SELECT count(*) FROM items").Scan(&n); err != nil || n != 0 {
				t.Errorf("item の数 = %d (error = %v), want 0", n, err)
			}
			if _, err := s.blobs.Size(ctx, testContent("a").Blob); err == nil {
				t.Errorf("拒否したのに blob が書き込まれた")
			}
		})
	}
}

func TestValidateCreateItem_ExtensionSkipsBlob(t *testing.T) {
	s, _ := openTestStore(t)
	for _, hasBlob := range []bool{true, false} {
		c, err := s.ValidateCreateItem(context.Background(), KindExtension, "lyell.clock", extensionContent(t, []byte("d")), hasBlob)
		if err != nil {
			t.Fatalf("ValidateCreateItem(hasBlob = %v) error = %v, want nil", hasBlob, err)
		}
		if c.Blob != (blobstore.Sum{}) || c.BlobSize != 0 {
			t.Errorf("ValidateCreateItem(hasBlob = %v) の blob = %s, %d, want 無し", hasBlob, c.Blob, c.BlobSize)
		}
	}
}

func TestCommitRevision_RejectsContentNotMatchingKind(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ext, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, nil)})
	if err != nil {
		t.Fatalf("CreateItem(extension) error = %v, want nil", err)
	}
	asset, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatalf("CreateItem(asset) error = %v, want nil", err)
	}
	assetWithData := testContent("a")
	assetWithData.Body = extensionContent(t, []byte("x")).Body

	tests := []struct {
		name string
		item Item
		c    Content
		data io.Reader
	}{
		{"extension の revision が blob を持つ", ext, testContent("b"), strings.NewReader("b")},
		{"asset の revision が blob を持たない", asset, extensionContent(t, nil), nil},
		{"asset の revision が extension_data を持つ", asset, assetWithData, strings.NewReader("a")},
		{"extension_data が上限を超える", ext, extensionContent(t, bytes.Repeat([]byte{1}, maxExtensionDataLen+1)), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRevision{ItemID: tt.item.ID, From: tt.item.Head, Content: tt.c, Data: tt.data}
			if _, err := s.CommitRevision(ctx, r); !errors.Is(err, ErrInvalid) {
				t.Errorf("CommitRevision() error = %v, want ErrInvalid", err)
			}
			if _, err := s.ValidateCommitRevision(ctx, r); !errors.Is(err, ErrInvalid) {
				t.Errorf("ValidateCommitRevision() error = %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := s.blobs.Size(ctx, testContent("b").Blob); err == nil {
		t.Errorf("拒否したのに blob が書き込まれた")
	}
}

func TestExtensionName_Limits(t *testing.T) {
	part := func(n int) string { return "a" + strings.Repeat("b", n-1) }
	ok := []string{"lyell.collection", "a.b", "a_1.b2.c_", "x9.y", part(63) + "." + part(64)}
	bad := []string{"", "lyell", "Lyell.x", "lyell.X", "1a.b", "_a.b", "a._b", "a.1b", "a..b", "a.b.", ".a.b", "a.b-c", "a.b c", "a.ｂ", part(64) + "." + part(64)}
	if n := len(ok[len(ok)-1]); n != maxExtensionNameLen {
		t.Fatalf("上限ちょうどの名前が %d バイト, want %d", n, maxExtensionNameLen)
	}
	s, _ := openTestStore(t)
	ctx := context.Background()
	for _, name := range ok {
		if _, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: name, Content: extensionContent(t, nil)}); err != nil {
			t.Errorf("CreateItem(%q) error = %v, want nil", name, err)
		}
	}
	for _, name := range bad {
		if _, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: name, Content: extensionContent(t, nil)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateItem(%q) error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestExtensionData_Limit(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, bytes.Repeat([]byte{1}, maxExtensionDataLen))})
	if err != nil {
		t.Fatalf("上限ちょうどのデータで CreateItem() error = %v, want nil", err)
	}
	if _, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, bytes.Repeat([]byte{1}, maxExtensionDataLen+1))}); !errors.Is(err, ErrInvalid) {
		t.Errorf("上限を超えるデータで CreateItem() error = %v, want ErrInvalid", err)
	}
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: extensionContent(t, bytes.Repeat([]byte{2}, maxExtensionDataLen))}); err != nil {
		t.Errorf("上限ちょうどのデータで CommitRevision() error = %v, want nil", err)
	}
	if maxExtensionDataLen != 16384 {
		t.Errorf("maxExtensionDataLen = %d, want 16 KiB", maxExtensionDataLen)
	}
}

func TestValidateCommitRevision_ExtensionIgnoresGivenBlobSize(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, nil)})
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	c := extensionContent(t, []byte("d"))
	c.BlobSize = 5 // blob を持たないので, 渡された大きさは使わない
	rev, err := s.ValidateCommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: c})
	if err != nil {
		t.Fatalf("ValidateCommitRevision() error = %v, want nil", err)
	}
	if rev.Content.BlobSize != 0 {
		t.Errorf("ValidateCommitRevision() の BlobSize = %d, want 0", rev.Content.BlobSize)
	}
}

func TestValidateCommitRevision_DoesNotDeadlockWithOneReadConn(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	// 読み取り用接続が1本しかなくても, ValidateCommitRevision は接続を2本同時に必要としない.
	s.read.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.ValidateCommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("a")}); err != nil {
		t.Fatalf("ValidateCommitRevision() error = %v, want nil", err)
	}
}

func TestExtensionItem_DropsBlobFieldsGivenInBody(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	// Content.Blob はゼロ値でも, Body が blob のフィールドを持っていれば, 記録する時にそれらを空にする.
	withBlob := func(data string) Content {
		c := extensionContent(t, []byte(data))
		body, err := proto.Marshal(&storagev1alpha1.RevisionBody{BlobSha256: bytes.Repeat([]byte{7}, 32), BlobSize: 3, ExtensionData: []byte(data)})
		if err != nil {
			t.Fatalf("body を作れない: %v", err)
		}
		c.Body = body
		return c
	}
	item, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: withBlob("v1")})
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	rev2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: withBlob("v2")})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	for _, id := range []ID{item.Head, rev2} {
		rev, err := s.Revision(ctx, id)
		if err != nil {
			t.Fatalf("Revision(%s) error = %v, want nil", id, err)
		}
		var b storagev1alpha1.RevisionBody
		if err := proto.Unmarshal(rev.Content.Body, &b); err != nil {
			t.Fatalf("body を読めない: %v", err)
		}
		if len(b.GetBlobSha256()) != 0 || b.GetBlobSize() != 0 || rev.Content.Blob != (blobstore.Sum{}) || rev.Content.BlobSize != 0 {
			t.Errorf("Revision(%s) の body の blob = %x, %d, Content の blob = %s, %d, want 無し", id, b.GetBlobSha256(), b.GetBlobSize(), rev.Content.Blob, rev.Content.BlobSize)
		}
	}
	var nulls int
	if err := s.read.QueryRow("SELECT count(*) FROM revisions WHERE blob_sha256 IS NULL AND blob_size IS NULL").Scan(&nulls); err != nil || nulls != 2 {
		t.Errorf("blob の列が NULL の revision = %d (error = %v), want 2", nulls, err)
	}
}
