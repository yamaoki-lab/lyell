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
	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"), strings.NewReader("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}

	r2, err := s.CommitRevision(ctx, item.ID, item.Head, []ID{item.Head}, testContent("r2"), strings.NewReader("r2"))
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
	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"), strings.NewReader("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	r1 := item.Head
	r2, err := s.CommitRevision(ctx, item.ID, r1, []ID{r1}, testContent("r2"), strings.NewReader("r2"))
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}
	before := countRevisions(t, s)

	// head は r2 に進んでいるのに, 古い r1 を前提にして足そうとする.
	_, err = s.CommitRevision(ctx, item.ID, r1, []ID{r1}, testContent("stale"), strings.NewReader("stale"))
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("古い head を前提にした CommitRevision() error = %v, want ErrNotFastForward", err)
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
	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"), strings.NewReader("r1"))
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
			_, err := s.CommitRevision(ctx, item.ID, item.Head, []ID{item.Head}, testContent("concurrent"), strings.NewReader("concurrent"))
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
	a, err := s.CreateItem(ctx, KindAsset, testContent("a"), strings.NewReader("a"))
	if err != nil {
		t.Fatalf("CreateItem(a) error = %v, want nil", err)
	}
	b, err := s.CreateItem(ctx, KindAsset, testContent("b"), strings.NewReader("b"))
	if err != nil {
		t.Fatalf("CreateItem(b) error = %v, want nil", err)
	}

	// b に a を統合する: 新しい revision は, b の head と a の head の両方を親に持つ.
	if _, err := s.CommitRevision(ctx, b.ID, b.Head, []ID{b.Head, a.Head}, testContent("merged"), strings.NewReader("merged")); err != nil {
		t.Errorf("統合の CommitRevision() error = %v, want nil", err)
	}
}

func TestCommitRevision_RejectsBadInput(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, KindAsset, testContent("r1"), strings.NewReader("r1"))
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
		{name: "親に前提の head が無い", itemID: item.ID, from: item.Head, parents: nil},
		{name: "親が重なっている", itemID: item.ID, from: item.Head, parents: []ID{item.Head, item.Head}},
		{name: "無い revision を親にする", itemID: item.ID, from: item.Head, parents: []ID{item.Head, other}},
		{name: "無い item", itemID: other, from: item.Head, parents: []ID{item.Head}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := countRevisions(t, s)
			if _, err := s.CommitRevision(ctx, tt.itemID, tt.from, tt.parents, testContent("bad"), strings.NewReader("bad")); err == nil {
				t.Errorf("CommitRevision() error = nil, want error")
			}
			if after := countRevisions(t, s); after != before {
				t.Errorf("断った時の revision の数 = %d, want %d", after, before)
			}
		})
	}
}
