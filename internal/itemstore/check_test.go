package itemstore

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck_HealthyDatabase(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	item, err := s.CreateItem(ctx, newTestItem("r1"))
	if err != nil {
		t.Fatalf("CreateItem() error = %v, want nil", err)
	}
	if _, err := s.CommitRevision(ctx, NewRevision{ItemID: item.ID, From: item.Head, Content: testContent("r2"), Data: strings.NewReader("r2")}); err != nil {
		t.Fatalf("CommitRevision() error = %v, want nil", err)
	}

	r, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if !r.OK() {
		t.Errorf("Check() = %+v, want 問題なし", r)
	}
}

func TestCheck_FindsViolationWrittenWithoutForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lyell.db")
	ctx := context.Background()
	var buf bytes.Buffer
	s, err := Open(ctx, Config{Path: path, Blobs: newTestBlobs(t), Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	defer s.Close()

	// 外部キーの検査を有効にし忘れた接続 (SQLite の既定) から書くと, 無い revision を指す行でもエラーにならない.
	raw, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v, want nil", err)
	}
	defer raw.Close()
	if _, err := raw.Exec("INSERT INTO revision_parents (revision_id, parent_id) VALUES (?, ?)", testID(1), testID(2)); err != nil {
		t.Fatalf("検査の無い接続での INSERT error = %v, want nil (エラーにならないことが問題)", err)
	}
	buf.Reset()

	r, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if r.OK() || len(r.ForeignKeys) == 0 {
		t.Fatalf("Check() = %+v, want 外部キーの違反", r)
	}
	for _, v := range r.ForeignKeys {
		if v.Table != "revision_parents" || v.Parent != "revisions" {
			t.Errorf("違反 = %+v, want revision_parents から revisions", v)
		}
	}
	if recs := readLog(t, &buf); len(recs) != 1 || recs[0].Level != "ERROR" {
		t.Errorf("ログ = %+v, want ERROR の1行", recs)
	}
}
