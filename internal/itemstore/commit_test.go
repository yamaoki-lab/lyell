package itemstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func countRevisions(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.read.QueryRow("SELECT count(*) FROM revisions").Scan(&n); err != nil {
		t.Fatalf("revision の数を読めない: %v", err)
	}
	return n
}

func TestCommitRevision_AdvancesHead(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	r2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Parents: []ID{item.Head}, Content: testContent("r2"), Data: strings.NewReader("r2")})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}

	got, err := s.Item(ctx, item.ID)
	if err != nil {
		t.Fatalf("Item() error = %v, want nil", err)
	}
	if got.Head != r2 {
		t.Errorf("head = %s, want 足した revision %s", got.Head, r2)
	}
	rev, err := s.Revision(ctx, r2)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if len(rev.Parents) != 1 || rev.Parents[0] != item.Head {
		t.Errorf("足した revision の親 = %v, want [%s]", rev.Parents, item.Head)
	}
}

func TestCommitRevision_RejectsStaleHeadWithoutWriting(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	r1 := item.Head
	r2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: r1, Parents: []ID{r1}, Content: testContent("r2"), Data: strings.NewReader("r2")})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	before := countRevisions(t, s)

	// head は r2 に進んでいるのに, 古い r1 を前提にして足そうとする.
	_, err = s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: r1, Parents: []ID{r1}, Content: testContent("stale"), Data: strings.NewReader("stale")})
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("古い head を前提にした CommitRevision() error = %v, want ErrNotFastForward", err)
	}
	// 呼び出し側が読み直さずに統合へ回せるよう, 断った時点の head が分かる.
	var nff *NotFastForwardError
	if !errors.As(err, &nff) {
		t.Fatalf("error = %T, want *NotFastForwardError", err)
	}
	if nff.ItemID != item.ID || nff.From != r1 || nff.Head != r2 {
		t.Errorf("NotFastForwardError = {ItemID: %s, From: %s, Head: %s}, want {%s, %s, %s}", nff.ItemID, nff.From, nff.Head, item.ID, r1, r2)
	}

	if got, _ := s.Item(ctx, item.ID); got.Head != r2 {
		t.Errorf("head = %s, want 動かずに %s", got.Head, r2)
	}
	if after := countRevisions(t, s); after != before {
		t.Errorf("断った時の revision の数 = %d, want %d (残らない)", after, before)
	}
}

func TestCommitRevision_OnlyOneOfConcurrentWritersWins(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	// 同じ head を前提にした書き込みを, 同時に何本も走らせる.
	const writers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, writers)
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Parents: []ID{item.Head}, Content: testContent("concurrent"), Data: strings.NewReader("concurrent")})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var won, lost int
	for err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrNotFastForward):
			lost++
		default:
			t.Errorf("CommitRevision() error = %v, want nil か ErrNotFastForward", err)
		}
	}
	if won != 1 || lost != writers-1 {
		t.Errorf("通った数 = %d, 断られた数 = %d, want 1, %d", won, lost, writers-1)
	}
	if n := countRevisions(t, s); n != 2 {
		t.Errorf("revision の数 = %d, want 2 (最初と, 通った1つ)", n)
	}
}

func TestCommitRevision_MergeMayHaveParentInAnotherItem(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	a, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatalf("CreateItem(a) error = %v, want nil", err)
	}
	b, err := s.CreateItem(ctx, newTestItem("b"))
	if err != nil {
		t.Fatalf("CreateItem(b) error = %v, want nil", err)
	}

	// b に a を統合する: 新しい revision は, b の head と a の head の両方を親に持つ.
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: b.ID, From: b.Head, Parents: []ID{b.Head, a.Head}, Content: testContent("merged"), Data: strings.NewReader("merged")}); err != nil {
		t.Errorf("統合の CommitRevision() error = %v, want nil", err)
	}
}

func TestCommitRevision_RejectsBadInput(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	other := ID(testID(99))

	tests := []struct {
		name    string
		itemID  ID
		from    ID
		parents []ID
		want    error
	}{
		{name: "親を書いたのに前提の head が無い", itemID: item.ID, from: item.Head, parents: []ID{other}},
		{name: "親が重なっている", itemID: item.ID, from: item.Head, parents: []ID{item.Head, item.Head}},
		{name: "無い revision を親にする", itemID: item.ID, from: item.Head, parents: []ID{item.Head, other}},
		{name: "無い item", itemID: other, from: item.Head, parents: []ID{item.Head}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := countRevisions(t, s)
			if _, err := s.CommitRevision(ctx, NewRevision{ItemID: tt.itemID, From: tt.from, Parents: tt.parents, Content: testContent("bad"), Data: strings.NewReader("bad")}); err == nil {
				t.Errorf("CommitRevision() error = nil, want error")
			}
			if after := countRevisions(t, s); after != before {
				t.Errorf("断った時の revision の数 = %d, want %d", after, before)
			}
		})
	}
}

func TestCommitRevision_EmptyParentsMeansFastForwardFromHead(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	// Parents を書かなければ, From だけが親になる.
	r2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("r2"), Data: strings.NewReader("r2")})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	rev, err := s.Revision(ctx, r2)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if len(rev.Parents) != 1 || rev.Parents[0] != item.Head {
		t.Errorf("親 = %v, want [%s]", rev.Parents, item.Head)
	}
}

// API では, blob を RPC の外で先に置き, 記録の時は SHA-256 だけを渡す. その時 Data は nil になる.
func TestCreateItem_WithoutDataUsesPlacedBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	content := testContent("placed")

	// 置かれていない blob を指して作ろうとしても, 何も残らない. 残すと, 無い blob を指す revision ができる.
	if _, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: content}); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("blob の無い CreateItem() error = %v, want ErrBlobNotFound", err)
	}
	if n := countRows(t, s); n != 0 {
		t.Errorf("断った後に行が %d 件ある, want 0", n)
	}

	if err := s.blobs.Put(ctx, content.Blob, strings.NewReader("placed")); err != nil {
		t.Fatalf("blob を置けない: %v", err)
	}
	item, err := s.CreateItem(ctx, NewItem{Kind: KindAsset, Content: content})
	if err != nil {
		t.Fatalf("置いた blob を指す CreateItem() error = %v, want nil", err)
	}
	rev, err := s.Revision(ctx, item.Head)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if rev.Content.Blob != content.Blob || rev.Content.BlobSize != int64(len("placed")) {
		t.Errorf("記録した blob = %s (%d バイト), want %s (%d バイト)", rev.Content.Blob, rev.Content.BlobSize, content.Blob, len("placed"))
	}
}

func TestCommitRevision_WithoutDataUsesPlacedBlob(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	content := testContent("r2")
	before := countRevisions(t, s)

	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: content}); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("blob の無い CommitRevision() error = %v, want ErrBlobNotFound", err)
	}
	if got, _ := s.Item(ctx, item.ID); got.Head != item.Head {
		t.Errorf("断った後の head = %s, want 動かずに %s", got.Head, item.Head)
	}
	if after := countRevisions(t, s); after != before {
		t.Errorf("断った後の revision の数 = %d, want %d", after, before)
	}

	if err := s.blobs.Put(ctx, content.Blob, strings.NewReader("r2")); err != nil {
		t.Fatalf("blob を置けない: %v", err)
	}
	r2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: content})
	if err != nil {
		t.Fatalf("置いた blob を指す CommitRevision() error = %v, want nil", err)
	}
	rev, err := s.Revision(ctx, r2)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if rev.Content.BlobSize != int64(len("r2")) {
		t.Errorf("記録した blob の大きさ = %d, want %d", rev.Content.BlobSize, len("r2"))
	}
}
