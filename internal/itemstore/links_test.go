package itemstore

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// insertTestLink は, リンク元 source から, 型 name (major 1) のリンクを挿入する. targetRevision と targetItem の一方だけを指定する (nil は NULL).
func insertTestLink(tx *sql.Tx, source, targetRevision, targetItem []byte, name string) error {
	_, err := tx.Exec("INSERT INTO revision_links (source_revision, target_revision, target_item, type_name, type_major) VALUES (?, ?, ?, ?, 1)", source, targetRevision, targetItem, name)
	return err
}

func TestSchema_KnownLinkTypes(t *testing.T) {
	s, _ := openTestStore(t)
	rows, err := s.read.Query("SELECT name, major, minor, patch, keeps_target, copyable, unknown_fields IS NULL FROM link_types ORDER BY name, major")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name string
		var major, minor, patch, keeps, copyable int
		var noUnknown bool
		if err := rows.Scan(&name, &major, &minor, &patch, &keeps, &copyable, &noUnknown); err != nil {
			t.Fatal(err)
		}
		if !noUnknown {
			t.Errorf("%s の unknown_fields が NULL でない", name)
		}
		if major != 1 || minor != 0 || patch != 0 {
			t.Errorf("%s の版 = %d.%d.%d, want 1.0.0", name, major, minor, patch)
		}
		got = append(got, name+map[int]string{0: "", 1: "+keeps"}[keeps]+map[int]string{0: "", 1: "+copyable"}[copyable])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"belongs_to+copyable", "derived_from+copyable", "parent+keeps", "required+keeps+copyable"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("リンクの型 = %v, want %v", got, want)
	}
}

func TestSchema_LinkTypeNameAndVersion(t *testing.T) {
	s, _ := openTestStore(t)
	insert := func(name string, major, minor, patch, keeps, copyable int) error {
		_, err := s.write.Exec("INSERT INTO link_types (name, major, minor, patch, keeps_target, copyable) VALUES (?, ?, ?, ?, ?, ?)", name, major, minor, patch, keeps, copyable)
		return err
	}
	for _, name := range []string{"a", "z9_", "a" + strings.Repeat("b", 63)} {
		if err := insert(name, 1, 0, 0, 0, 0); err != nil {
			t.Errorf("名前 %q の error = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "A", "aB", "1a", "_a", "a-b", "a b", "aé", "a" + strings.Repeat("b", 64)} {
		if err := insert(name, 1, 0, 0, 0, 0); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("名前 %q の error = %v, want CHECK", name, err)
		}
	}
	for _, v := range [][5]int{{-1, 0, 0, 0, 0}, {1, -1, 0, 0, 0}, {1, 0, -1, 0, 0}, {1, 0, 0, 2, 0}, {1, 0, 0, 0, 2}} {
		if err := insert("v", v[0], v[1], v[2], v[3], v[4]); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("版とフラグ %v の error = %v, want CHECK", v, err)
		}
	}
	// 名前と major 番号ごとに1行. major が違えば別の定義になる.
	if err := insert("a", 1, 1, 0, 0, 0); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("同じ名前と major の2行目の error = %v, want UNIQUE", err)
	}
	if err := insert("a", 2, 0, 0, 1, 0); err != nil {
		t.Errorf("別の major の error = %v, want nil", err)
	}
}

func TestSchema_RevisionLinkConstraints(t *testing.T) {
	s, _ := openTestStore(t)
	item, r1, r2 := testID(1), testID(2), testID(3)
	if err := inTx(t, s, func(tx *sql.Tx) error {
		if err := insertTestItem(tx, item, r1); err != nil {
			return err
		}
		return insertTestRevision(tx, r2, item)
	}); err != nil {
		t.Fatal(err)
	}
	link := func(target, targetItem []byte, name string, major int) error {
		_, err := s.write.Exec("INSERT INTO revision_links (source_revision, target_revision, target_item, type_name, type_major) VALUES (?, ?, ?, ?, ?)", r2, target, targetItem, name, major)
		return err
	}
	for _, tt := range []struct {
		name               string
		target, targetItem []byte
		typ                string
		major              int
		want               string
	}{
		{"リンク先が無い", nil, nil, "required", 1, "CHECK"},
		{"リンク先が2つ", r1, item, "required", 1, "CHECK"},
		{"parent が item を指す", nil, item, "parent", 1, "CHECK"},
		{"定義の無い型", r1, nil, "unknown", 1, "FOREIGN KEY"},
		{"定義の無い major", r1, nil, "parent", 2, "FOREIGN KEY"},
		{"リンク先の ID の長さ", make([]byte, 17), nil, "required", 1, "CHECK"},
	} {
		if err := link(tt.target, tt.targetItem, tt.typ, tt.major); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %s", tt.name, err, tt.want)
		}
	}

	// 同じリンク元とリンク先に, 型ごとに1行. 別の型なら同じ組に何行でも持てる. リンク先はこのマシンに無くてもよい.
	missing := testID(9)
	for _, l := range []struct {
		target, targetItem []byte
		typ                string
	}{{r1, nil, "parent"}, {r1, nil, "derived_from"}, {r1, nil, "belongs_to"}, {nil, item, "derived_from"}, {nil, item, "required"}, {missing, nil, "derived_from"}, {nil, missing, "derived_from"}} {
		if err := link(l.target, l.targetItem, l.typ, 1); err != nil {
			t.Errorf("%s → %x%x の error = %v, want nil", l.typ, l.target, l.targetItem, err)
		}
	}
	if err := link(r1, nil, "derived_from", 1); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("revision 宛ての同じ型の2行目の error = %v, want UNIQUE", err)
	}
	if err := link(nil, item, "required", 1); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("item 宛ての同じ型の2行目の error = %v, want UNIQUE", err)
	}
}

func TestCommitRevision_RecordsParentsAsLinks(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	a, err := s.CreateItem(ctx, newTestItem("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateItem(ctx, newTestItem("b"))
	if err != nil {
		t.Fatal(err)
	}
	ff, err := s.CommitRevision(ctx, NewRevision{ItemID: b.ID, From: b.Head, Content: testContent("ff"), Data: strings.NewReader("ff")})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.CommitRevision(ctx, NewRevision{ItemID: b.ID, From: ff, Parents: []ID{ff, a.Head}, Content: testContent("m"), Data: strings.NewReader("m")})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		rev  ID
		want []ID
	}{{b.Head, nil}, {ff, []ID{b.Head}}, {merged, sortedIDs(ff, a.Head)}} {
		rows, err := s.read.Query("SELECT target_revision, target_item IS NULL, type_name, type_major, unknown_fields IS NULL FROM revision_links WHERE source_revision = ? ORDER BY target_revision", tt.rev[:])
		if err != nil {
			t.Fatal(err)
		}
		var got []ID
		for rows.Next() {
			var raw []byte
			var noItem, noUnknown bool
			var typ string
			var major int
			if err := rows.Scan(&raw, &noItem, &typ, &major, &noUnknown); err != nil {
				t.Fatal(err)
			}
			if !noItem || typ != "parent" || major != 1 || !noUnknown {
				t.Errorf("%s のリンク = (item 無し %v, %s %d, 未知のフィールド無し %v), want parent 1 で item と未知のフィールドは NULL", tt.rev, noItem, typ, major, noUnknown)
			}
			got = append(got, ID(raw))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s の親のリンク = %v, want %v", tt.rev, got, tt.want)
		}
	}
}

// TestReadParents_IgnoresOtherLinks は, 親を読む箇所 (Revision と ListItems) が parent 型のリンクだけを親として返すことを確かめる.
func TestReadParents_IgnoresOtherLinks(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	other, err := s.CreateItem(ctx, newTestItem("o"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ctx, newTestItem("i"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("h"), Data: strings.NewReader("h")})
	if err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, s, func(tx *sql.Tx) error {
		for _, typ := range []string{"derived_from", "belongs_to", "required"} {
			if err := insertTestLink(tx, head[:], other.Head[:], nil, typ); err != nil {
				return err
			}
		}
		return insertTestLink(tx, head[:], nil, other.ID[:], "derived_from")
	}); err != nil {
		t.Fatal(err)
	}

	rev, err := s.Revision(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rev.Parents, []ID{item.Head}) {
		t.Errorf("Revision().Parents = %v, want [%s]", rev.Parents, item.Head)
	}
	list, err := s.ListItems(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list.Entries {
		if e.Item.ID != item.ID {
			continue
		}
		found = true
		if !reflect.DeepEqual(e.Head.Parents, []ID{item.Head}) {
			t.Errorf("ListItems() の head の親 = %v, want [%s]", e.Head.Parents, item.Head)
		}
	}
	if !found {
		t.Errorf("ListItems() に %s が無い", item.ID)
	}
}

// sortedIDs は, ids を ID 順に並べて返す (revision_links を target_revision の順に読んだ時の順序).
func sortedIDs(ids ...ID) []ID {
	if string(ids[0][:]) > string(ids[1][:]) {
		ids[0], ids[1] = ids[1], ids[0]
	}
	return ids
}
