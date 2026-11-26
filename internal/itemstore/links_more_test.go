package itemstore

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
)

// linksTo は, LinksTo の結果を返す.
func linksTo(t *testing.T, s *Store, target LinkTarget, typ LinkType) []ID {
	t.Helper()
	got, err := s.LinksTo(context.Background(), target, typ)
	if err != nil {
		t.Fatalf("LinksTo(%+v, %s) error = %v, want nil", target, typ.Name, err)
	}
	return got
}

func TestLinks_RecordedAndFoundByTarget(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	src, err := s.CreateItem(ctx, newTestItem("src"))
	if err != nil {
		t.Fatal(err)
	}
	missing := ID(testID(9)) // このマシンに無いリンク先
	onRev, onItem := LinkTarget{Revision: src.Head}, LinkTarget{Item: src.ID}

	// 最初の revision から, revision 宛てと item 宛てのリンクを持てる. 同じリンク先に複数の型を持てる.
	first := []Link{
		{Type: LinkDerivedFrom, Target: onRev},
		{Type: LinkBelongsTo, Target: onRev},
		{Type: LinkRequired, Target: onItem},
		{Type: LinkDerivedFrom, Target: LinkTarget{Item: missing}},
	}
	rendition, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: testContent("r"), Data: strings.NewReader("r"), Links: first})
	if err != nil {
		t.Fatalf("リンク付きの CreateItem() error = %v, want nil", err)
	}
	wantFirst := []Link{first[1], first[3], first[0], first[2]} // 型の名前, リンク先の順
	rev, err := s.Revision(ctx, rendition.Head)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rev.Links, wantFirst) || rev.Parents != nil {
		t.Errorf("最初の revision の Links = %+v, Parents = %v, want %+v と親無し", rev.Links, rev.Parents, wantFirst)
	}

	// 次の revision は From のリンクを引き継がず, 渡したリンクだけを持つ. 親はリンクに含めない.
	next := []Link{{Type: LinkDerivedFrom, Target: onItem}, {Type: LinkRequired, Target: LinkTarget{Revision: missing}}}
	preview, err := s.ValidateCommitRevision(ctx, NewRevision{ItemID: rendition.ID, From: rendition.Head, Content: testContent("r"), Links: next})
	if err != nil {
		t.Fatalf("ValidateCommitRevision() error = %v, want nil", err)
	}
	head, err := s.CommitRevision(ctx, NewRevision{ItemID: rendition.ID, From: rendition.Head, Content: testContent("r"), Links: next})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	if rev, err = s.Revision(ctx, head); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rev.Links, next) || !reflect.DeepEqual(preview.Links, next) {
		t.Errorf("Revision().Links = %+v, ValidateCommitRevision().Links = %+v, want %+v", rev.Links, preview.Links, next)
	}
	list, err := s.ListItems(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.Item.ID == rendition.ID && !reflect.DeepEqual(e.Head.Links, next) {
			t.Errorf("ListItems() の head の Links = %+v, want %+v", e.Head.Links, next)
		}
	}

	// 別のリンク元が同じ item を同じ型で指せる.
	other, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: testContent("o"), Data: strings.NewReader("o"), Links: []Link{{Type: LinkDerivedFrom, Target: onItem}}})
	if err != nil {
		t.Fatalf("同じ item を同じ型で指す別のリンク元の CreateItem() error = %v, want nil", err)
	}

	for _, tt := range []struct {
		target LinkTarget
		typ    LinkType
		want   []ID
	}{
		{onRev, LinkDerivedFrom, []ID{rendition.Head}},
		{onRev, LinkBelongsTo, []ID{rendition.Head}},
		{onRev, LinkRequired, nil},
		{onItem, LinkRequired, []ID{rendition.Head}},
		{onItem, LinkDerivedFrom, []ID{head, other.Head}},
		{onItem, LinkBelongsTo, nil},
		{LinkTarget{Item: missing}, LinkDerivedFrom, []ID{rendition.Head}},
		{LinkTarget{Revision: missing}, LinkRequired, []ID{head}},
		{LinkTarget{Revision: rendition.Head}, LinkParent, []ID{head}}, // 親の型では子を返す
	} {
		if got := linksTo(t, s, tt.target, tt.typ); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("LinksTo(%+v, %s) = %v, want %v", tt.target, tt.typ.Name, got, tt.want)
		}
	}
	if _, err := s.LinksTo(ctx, LinkTarget{}, LinkRequired); !errors.Is(err, ErrInvalid) {
		t.Errorf("リンク先の無い LinksTo() error = %v, want ErrInvalid", err)
	}
}

func TestLinks_RejectsInvalidLinks(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatal(err)
	}
	onRev := LinkTarget{Revision: item.Head}
	unknown := LinkType{Name: "thumbnail_of", Major: 1}
	for _, tt := range []struct {
		name  string
		links []Link
		defs  []LinkTypeDefinition
	}{
		{"親を Links で渡す", []Link{{Type: LinkParent, Target: onRev}}, nil},
		{"リンク先が無い", []Link{{Type: LinkRequired}}, nil},
		{"リンク先が2つ", []Link{{Type: LinkRequired, Target: LinkTarget{Revision: item.Head, Item: item.ID}}}, nil},
		{"同じリンクが2つ", []Link{{Type: LinkRequired, Target: onRev}, {Type: LinkRequired, Target: onRev}}, nil},
		{"知らない型を定義無しで", []Link{{Type: unknown, Target: onRev}}, nil},
		{"知らない major を定義無しで", []Link{{Type: LinkType{Name: "required", Major: 2}, Target: onRev}}, nil},
		{"別の型の定義だけがある", []Link{{Type: unknown, Target: onRev}}, []LinkTypeDefinition{{LinkType: LinkType{Name: "other", Major: 1}}}},
		{"型の名前の形式", []Link{{Type: LinkType{Name: "Bad", Major: 1}, Target: onRev}}, []LinkTypeDefinition{{LinkType: LinkType{Name: "Bad", Major: 1}}}},
		{"定義の名前の形式", nil, []LinkTypeDefinition{{LinkType: LinkType{Name: "a-b", Major: 1}}}},
		{"定義の版が負", nil, []LinkTypeDefinition{{LinkType: unknown, Minor: -1}}},
		{"定義の major が負", nil, []LinkTypeDefinition{{LinkType: LinkType{Name: "x", Major: -1}}}},
		{"定義が重複", nil, []LinkTypeDefinition{{LinkType: unknown}, {LinkType: unknown, Minor: 1}}},
	} {
		_, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: testContent("b"), Data: strings.NewReader("b"), Links: tt.links, LinkTypes: tt.defs})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CreateItem() error = %v, want ErrInvalid", tt.name, err)
		}
		r := NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("b"), Data: strings.NewReader("b"), Links: tt.links, LinkTypes: tt.defs}
		if _, err := s.CommitRevision(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CommitRevision() error = %v, want ErrInvalid", tt.name, err)
		}
		r.Data = nil
		if _, err := s.ValidateCommitRevision(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ValidateCommitRevision() error = %v, want ErrInvalid", tt.name, err)
		}
	}
	if n := countRevisions(t, s); n != 1 {
		t.Errorf("拒否後の revision の数 = %d, want 1", n)
	}
}

// linkTypeRow は, link_types の1行を "minor.patch keeps copyable unknown" の形で返す. 無ければ空文字列.
func linkTypeRow(t *testing.T, s *Store, typ LinkType) string {
	t.Helper()
	var minor, patch, keeps, copyable int
	var unknown []byte
	err := s.read.QueryRow("SELECT minor, patch, keeps_target, copyable, unknown_fields FROM link_types WHERE name = ? AND major = ?", typ.Name, typ.Major).Scan(&minor, &patch, &keeps, &copyable, &unknown)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Join([]string{strconv.Itoa(minor) + "." + strconv.Itoa(patch), strconv.Itoa(keeps), strconv.Itoa(copyable), string(unknown)}, " "))
}

func TestLinks_UnknownTypeNeedsDefinition(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatal(err)
	}
	thumb := LinkType{Name: "thumbnail_of", Major: 1}
	def := func(minor, patch int, keeps bool, unknown string) LinkTypeDefinition {
		return LinkTypeDefinition{LinkType: thumb, Minor: minor, Patch: patch, KeepsTarget: keeps, Copyable: true, UnknownFields: []byte(unknown)}
	}
	head := item.Head
	commit := func(data string, d ...LinkTypeDefinition) error {
		links := []Link{{Type: thumb, Target: LinkTarget{Item: ID(testID(byte(len(data))))}}}
		id, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: head, Content: testContent(data), Data: strings.NewReader(data), Links: links, LinkTypes: d})
		if err == nil {
			head = id
		}
		return err
	}

	// 検証だけでは定義を登録しない.
	if _, err := s.ValidateCommitRevision(ctx, NewRevision{ItemID: item.ID, From: head, Content: testContent("a"), Links: []Link{{Type: thumb, Target: LinkTarget{Item: item.ID}}}, LinkTypes: []LinkTypeDefinition{def(0, 0, false, "u")}}); err != nil {
		t.Fatalf("定義付きの ValidateCommitRevision() error = %v, want nil", err)
	}
	if got := linkTypeRow(t, s, thumb); got != "" {
		t.Errorf("検証後の定義 = %q, want 登録しない", got)
	}

	// 定義と一緒なら保存し, 知らない項目ごと定義を登録する.
	if err := commit("b", def(1, 0, false, "u1")); err != nil {
		t.Fatalf("定義付きの CommitRevision() error = %v, want nil", err)
	}
	if got := linkTypeRow(t, s, thumb); got != "1.0 0 1 u1" {
		t.Errorf("登録した定義 = %q, want 1.0 0 1 u1", got)
	}
	rev, err := s.Revision(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Links) != 1 || rev.Links[0].Type != thumb {
		t.Errorf("知らない型のリンク = %+v, want %s", rev.Links, thumb.Name)
	}

	// 登録済みでも, この版からは定義無しで知らない型のリンクを書けない.
	if err := commit("cc"); !errors.Is(err, ErrInvalid) {
		t.Errorf("登録済みの知らない型を定義無しで書いた error = %v, want ErrInvalid", err)
	}

	// 同じ major の新しい定義だけが置き換える. 古い定義と同じ版の定義は, 知らない項目が違っても置き換えない.
	for i, tt := range []struct {
		d    LinkTypeDefinition
		want string
	}{
		{def(0, 9, false, "old"), "1.0 0 1 u1"},
		{def(1, 0, false, "same"), "1.0 0 1 u1"},
		{def(1, 1, false, "u2"), "1.1 0 1 u2"},
		{def(2, 0, false, ""), "2.0 0 1"},
	} {
		if err := commit(strings.Repeat("d", 3+i), tt.d); err != nil {
			t.Fatalf("定義 %d.%d の CommitRevision() error = %v, want nil", tt.d.Minor, tt.d.Patch, err)
		}
		if got := linkTypeRow(t, s, thumb); got != tt.want {
			t.Errorf("定義 %d.%d (%s) の後の定義 = %q, want %q", tt.d.Minor, tt.d.Patch, tt.d.UnknownFields, got, tt.want)
		}
	}

	// フラグが登録済みの定義と食い違えば, 新しい版でも拒否し, 何も記録しない. 組み込みの型も同じ.
	before := countRevisions(t, s)
	if err := commit("eeeeeeeeeeee", def(3, 0, true, "")); !errors.Is(err, ErrInvalid) {
		t.Errorf("フラグが食い違う定義の error = %v, want ErrInvalid", err)
	}
	conflict := LinkTypeDefinition{LinkType: LinkRequired, Minor: 1, KeepsTarget: false, Copyable: true}
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: head, Content: testContent("f"), Data: strings.NewReader("f"), LinkTypes: []LinkTypeDefinition{conflict}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("組み込みの型とフラグが食い違う定義の error = %v, want ErrInvalid", err)
	}
	if _, err := s.ValidateCommitRevision(ctx, NewRevision{ItemID: item.ID, From: head, Content: testContent("a"), LinkTypes: []LinkTypeDefinition{conflict}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("検証で, 組み込みの型とフラグが食い違う定義の error = %v, want ErrInvalid", err)
	}
	if got := linkTypeRow(t, s, thumb); got != "2.0 0 1" {
		t.Errorf("拒否後の定義 = %q, want 2.0 0 1", got)
	}
	if got := linkTypeRow(t, s, LinkRequired); got != "0.0 1 1" {
		t.Errorf("拒否後の required の定義 = %q, want 0.0 1 1", got)
	}
	if n := countRevisions(t, s); n != before {
		t.Errorf("拒否後の revision の数 = %d, want %d", n, before)
	}

	// CreateItem でも, 定義と一緒なら知らない型のリンクを書ける.
	other := LinkType{Name: "preview_of", Major: 3}
	created, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: testContent("g"), Data: strings.NewReader("g"),
		Links: []Link{{Type: other, Target: LinkTarget{Revision: item.Head}}}, LinkTypes: []LinkTypeDefinition{{LinkType: other, KeepsTarget: true}}})
	if err != nil {
		t.Fatalf("定義付きの CreateItem() error = %v, want nil", err)
	}
	if got := linksTo(t, s, LinkTarget{Revision: item.Head}, other); !reflect.DeepEqual(got, []ID{created.Head}) {
		t.Errorf("LinksTo(知らない型) = %v, want [%s]", got, created.Head)
	}
	if got := linkTypeRow(t, s, other); got != "0.0 1 0" {
		t.Errorf("CreateItem で登録した定義 = %q, want 0.0 1 0", got)
	}
}

func TestCommitRevision_RejectsUnchangedRevision(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	named := func(name string) Content {
		c := testContent("a")
		body, err := proto.Marshal(&storagev1alpha1.RevisionBody{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		c.Body = body
		return c
	}
	item, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: named("n"), Data: strings.NewReader("a")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateItem(ctx, newTestItem("o"))
	if err != nil {
		t.Fatal(err)
	}
	head := item.Head
	link := []Link{{Type: LinkDerivedFrom, Target: LinkTarget{Item: other.ID}}}
	try := func(c Content, links []Link) error {
		r := NewRevision{ItemID: item.ID, From: head, Content: c, Links: links}
		_, verr := s.ValidateCommitRevision(ctx, r)
		id, err := s.CommitRevision(ctx, r)
		if (verr == nil) != (err == nil) {
			t.Errorf("ValidateCommitRevision() error = %v, CommitRevision() error = %v, want 同じ結果", verr, err)
		}
		if err == nil {
			head = id
		}
		return err
	}
	// 記録時刻と作成者だけが違っても, 変更ではない.
	authored := named("n")
	authored.AuthorUser, authored.CreatedAt = "someone", authored.CreatedAt.Add(1e9)
	for _, tt := range []struct {
		name    string
		content Content
		links   []Link
		want    error
	}{
		{"何も変えない", named("n"), nil, ErrUnchanged},
		{"記録時刻と作成者だけを変える", authored, nil, ErrUnchanged},
		{"リンクだけを足す", named("n"), link, nil},
		{"同じリンクのまま", named("n"), link, ErrUnchanged},
		{"リンクの型だけを変える", named("n"), []Link{{Type: LinkRequired, Target: LinkTarget{Item: other.ID}}}, nil},
		{"リンクを外す", named("n"), nil, nil},
		{"body (名前) だけを変える", named("m"), nil, nil},
		{"blob だけを変える", func() Content { c := named("m"); c.Blob = testContent("o").Blob; return c }(), nil, nil},
	} {
		if err := try(tt.content, tt.links); !errors.Is(err, tt.want) || (tt.want != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
		}
	}

	// 統合は, 中身もリンクも同じでも記録する.
	r := NewRevision{ItemID: item.ID, From: head, Parents: []ID{head, other.Head}, Content: func() Content { c := named("m"); c.Blob = testContent("o").Blob; return c }()}
	if head, err = s.CommitRevision(ctx, r); err != nil {
		t.Errorf("中身の同じ統合の error = %v, want nil", err)
	}
	// From が間引かれていれば比べられないので, 記録する.
	if _, err := s.write.Exec("UPDATE revisions SET created_at = NULL, author_user = NULL, author_machine = NULL, blob_sha256 = NULL, blob_size = NULL, body = NULL WHERE id = ?", head[:]); err != nil {
		t.Fatal(err)
	}
	r = NewRevision{ItemID: item.ID, From: head, Content: testContent("o")}
	if _, err := s.CommitRevision(ctx, r); err != nil {
		t.Errorf("間引かれた From からの error = %v, want nil", err)
	}
	// blob も body の中身も持たない revision は, 間引かれた列 (NULL) と比べると同じに見えるが, 比べずに記録する.
	ext, err := s.CreateItem(ctx, NewItem{Kind: KindExtension, Extension: "lyell.clock", Content: extensionContent(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.Exec("UPDATE revisions SET created_at = NULL, author_user = NULL, author_machine = NULL, body = NULL WHERE id = ?", ext.Head[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: ext.ID, From: ext.Head, Content: extensionContent(t, nil)}); err != nil {
		t.Errorf("間引かれた extension の From からの error = %v, want nil", err)
	}
}
