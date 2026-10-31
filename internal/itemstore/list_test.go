package itemstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// createTestItems は, asset を n 個作成し, 作成した順に返す.
func createTestItems(t *testing.T, s *Store, n int) []Item {
	t.Helper()
	var items []Item
	for i := range n {
		item, err := s.CreateItem(context.Background(), newTestItem(strings.Repeat("x", i+1)))
		if err != nil {
			t.Fatalf("CreateItem() error = %v, want nil", err)
		}
		items = append(items, item)
	}
	return items
}

// listAll は, Limit 件ずつページを読み進め, 返った item の ID を順に返す. 各ページの TotalSize が want と一致し, 最後のページだけ More が false であることも確認する.
func listAll(t *testing.T, s *Store, o ListOptions, wantTotal int) []ID {
	t.Helper()
	var ids []ID
	for page := 0; ; page++ {
		got, err := s.ListItems(context.Background(), o)
		if err != nil {
			t.Fatalf("%d ページ目の ListItems() error = %v, want nil", page, err)
		}
		if got.TotalSize != wantTotal {
			t.Errorf("%d ページ目の TotalSize = %d, want %d", page, got.TotalSize, wantTotal)
		}
		for _, e := range got.Entries {
			ids = append(ids, e.Item.ID)
		}
		if !got.More {
			return ids
		}
		if len(got.Entries) == 0 {
			t.Fatalf("%d ページ目が空なのに More = true です", page)
		}
		o.StartAfter = got.Entries[len(got.Entries)-1].Item.ID
	}
}

func itemIDs(items []Item) []ID {
	ids := make([]ID, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func TestListItems_PagesInIDOrder(t *testing.T) {
	s, _ := openTestStore(t)
	items := createTestItems(t, s, 5)
	want := itemIDs(items)

	// 件数が割り切れない場合 (5 = 2 + 2 + 1) と, 割り切れる場合 (5 = 5) の両方を確認する.
	for _, limit := range []int{2, 5} {
		if got := listAll(t, s, ListOptions{Limit: limit}, 5); !reflect.DeepEqual(got, want) {
			t.Errorf("Limit %d で昇順に取得した ID = %v, want %v", limit, got, want)
		}
	}

	reversed := make([]ID, len(want))
	for i, id := range want {
		reversed[len(want)-1-i] = id
	}
	if got := listAll(t, s, ListOptions{Order: OrderIDDescending, Limit: 2}, 5); !reflect.DeepEqual(got, reversed) {
		t.Errorf("降順に取得した ID = %v, want %v", got, reversed)
	}
}

func TestListItems_EmptyStore(t *testing.T) {
	s, _ := openTestStore(t)
	got, err := s.ListItems(context.Background(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if len(got.Entries) != 0 || got.TotalSize != 0 || got.More {
		t.Errorf("ListItems() = %+v, want 空", got)
	}
}

func TestListItems_ReturnsHeadRevision(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item := createTestItems(t, s, 1)[0]
	r2, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("r2"), Data: strings.NewReader("r2")})
	if err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}

	got, err := s.ListItems(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListItems() error = %v, want nil", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("件数 = %d, want 1", len(got.Entries))
	}
	// 一覧の head の revision は, Revision で読んだものと一致する (親の一覧も含む).
	want, err := s.Revision(ctx, r2)
	if err != nil {
		t.Fatalf("Revision() error = %v, want nil", err)
	}
	if e := got.Entries[0]; e.Item.Head != r2 || !reflect.DeepEqual(e.Head, want) {
		t.Errorf("取得した item = %+v, want head %s の revision %+v", e, r2, want)
	}
}

func TestListItems_FiltersByKind(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	assets := createTestItems(t, s, 3)
	collection, err := s.CreateItem(ctx, NewItem{Kind: KindCollection, Content: testContent("c"), Data: strings.NewReader("c")})
	if err != nil {
		t.Fatalf("CreateItem(collection) error = %v, want nil", err)
	}

	if got := listAll(t, s, ListOptions{Kind: KindAsset, Limit: 2}, 3); !reflect.DeepEqual(got, itemIDs(assets)) {
		t.Errorf("asset で絞り込んだ ID = %v, want %v", got, itemIDs(assets))
	}
	if got := listAll(t, s, ListOptions{Kind: KindCollection, Limit: 2}, 1); !reflect.DeepEqual(got, []ID{collection.ID}) {
		t.Errorf("collection で絞り込んだ ID = %v, want [%s]", got, collection.ID)
	}
}

func TestListItems_FiltersByHeadUpdateTime(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// 3つの item を, 1時間ずつずらした時刻で作成する. 最初の item だけ, 後から head を5時間後に進める.
	var items []Item
	for i := range 3 {
		n := newTestItem(strings.Repeat("x", i+1))
		n.Content.CreatedAt = base.Add(time.Duration(i) * time.Hour)
		item, err := s.CreateItem(ctx, n)
		if err != nil {
			t.Fatalf("CreateItem() error = %v, want nil", err)
		}
		items = append(items, item)
	}
	c := testContent("later")
	c.CreatedAt = base.Add(5 * time.Hour)
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: items[0].ID, From: items[0].Head, Content: c, Data: strings.NewReader("later")}); err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}

	tests := []struct {
		name          string
		since, before time.Time
		want          []Item
	}{
		{name: "以降は境界を含む", since: base.Add(1 * time.Hour), want: items},
		{name: "head を進めた item は新しい時刻で判定する", since: base.Add(4 * time.Hour), want: items[:1]},
		{name: "より前は境界を含まない", before: base.Add(2 * time.Hour), want: items[1:2]},
		{name: "範囲", since: base.Add(1 * time.Hour), before: base.Add(3 * time.Hour), want: items[1:]},
		// DB はミリ秒単位なので, 1ナノ秒後は次のミリ秒に切り上げて比較する.
		{name: "以降のミリ秒未満は切り上げる", since: base.Add(2*time.Hour + time.Nanosecond), want: items[:1]},
		{name: "より前のミリ秒未満は切り上げる", before: base.Add(1*time.Hour + time.Nanosecond), want: items[1:2]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := itemIDs(tt.want)
			if got := listAll(t, s, ListOptions{UpdatedSince: tt.since, UpdatedBefore: tt.before, Limit: 10}, len(want)); !reflect.DeepEqual(got, want) {
				t.Errorf("ID = %v, want %v", got, want)
			}
		})
	}
}

func TestListItems_RejectsInvalidOptions(t *testing.T) {
	s, _ := openTestStore(t)
	tests := []struct {
		name string
		o    ListOptions
	}{
		{name: "Limit が0", o: ListOptions{}},
		{name: "不明な種類", o: ListOptions{Kind: "unknown", Limit: 1}},
		{name: "不明な並び順", o: ListOptions{Order: Order(99), Limit: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.ListItems(context.Background(), tt.o); !errors.Is(err, ErrInvalid) {
				t.Errorf("ListItems() error = %v, want ErrInvalid", err)
			}
		})
	}
}
