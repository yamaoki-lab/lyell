package itemstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lyell.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// pragma は, conn で PRAGMA name を読む.
func pragma(t *testing.T, conn *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s error = %v, want nil", name, err)
	}
	return v
}

// userVersion は, path の DB の schema version を, Store を通さずに読む.
func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v, want nil", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil { // yamaoki-lint:ignore
		t.Fatalf("DB の schema version を読めない: error = %v, want nil", err)
	}
	return v
}

func backups(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".v*.bak")
	if err != nil {
		t.Fatalf("Glob() error = %v, want nil", err)
	}
	return m
}

func TestOpen_WriteConnectionHasPragmas(t *testing.T) {
	s, _ := openTestStore(t)
	conn, err := s.write.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn() error = %v, want nil", err)
	}
	defer conn.Close()

	want := map[string]string{
		"foreign_keys": "1",
		"journal_mode": "wal",
		"synchronous":  "2", // FULL
		"query_only":   "0",
	}
	for name, w := range want {
		if got := pragma(t, conn, name); got != w {
			t.Errorf("書き込み用の PRAGMA %s = %s, want %s", name, got, w)
		}
	}
	if n := s.write.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("書き込み用の接続の上限 = %d, want 1", n)
	}
}

func TestOpen_EveryReadConnectionHasPragmas(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// 接続を返さずに複数取り出して, database/sql に新しい接続を作らせる. 最初の1本だけに設定が効いている, という誤りを見つけるため.
	const n = 3
	for i := range n {
		conn, err := s.read.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn() (%d本目) error = %v, want nil", i+1, err)
		}
		defer conn.Close()
		if got := pragma(t, conn, "foreign_keys"); got != "1" {
			t.Errorf("読み取り用 %d本目の PRAGMA foreign_keys = %s, want 1", i+1, got)
		}
		if got := pragma(t, conn, "query_only"); got != "1" {
			t.Errorf("読み取り用 %d本目の PRAGMA query_only = %s, want 1", i+1, got)
		}
	}
	if got := s.read.Stats().OpenConnections; got < n {
		t.Errorf("読み取り用の接続の数 = %d, want %d 以上", got, n)
	}
}

func TestOpen_ReadConnectionRejectsWrites(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.read.Exec("PRAGMA user_version = 99"); err == nil { // yamaoki-lint:ignore
		t.Errorf("読み取り用の接続で書き込めた, want error")
	}
}

func TestOpen_SQLiteIsNewEnough(t *testing.T) {
	s, _ := openTestStore(t)
	var v string
	if err := s.read.QueryRow("SELECT sqlite_version()").Scan(&v); err != nil { // yamaoki-lint:ignore
		t.Fatalf("SQLite のバージョンを読めない: error = %v, want nil", err)
	}
	t.Logf("同梱の SQLite のバージョン: %s", v)
	if err := checkSQLiteVersion(context.Background(), s.read); err != nil {
		t.Errorf("checkSQLiteVersion() error = %v, want nil", err)
	}
}

func TestOpen_AppliesSchemaOnceAndWithoutBackupWhenNew(t *testing.T) {
	s, path := openTestStore(t)
	for _, table := range []string{"items", "revisions", "revision_parents"} {
		var name string
		if err := s.read.QueryRow("SELECT name FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil {
			t.Errorf("表 %s が無い: %v", table, err)
		}
	}
	s.Close()

	// 開き直しても, schema version はそのままで, 複製も作らない.
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("2回目の Open() error = %v, want nil", err)
	}
	s2.Close()
	if got := userVersion(t, path); got != len(migrations) {
		t.Errorf("DB の schema version = %d, want %d", got, len(migrations))
	}
	if b := backups(t, path); len(b) != 0 {
		t.Errorf("新しい DB で複製が作られた: %v", b)
	}
}

func TestOpen_RejectsNewerSchema(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.write.Exec("PRAGMA user_version = 99"); err != nil { // yamaoki-lint:ignore
		t.Fatalf("DB の schema version を書けない: %v", err)
	}
	s.Close()

	if s2, err := Open(context.Background(), path); err == nil {
		s2.Close()
		t.Fatalf("新しい schema version の DB を開けた, want error")
	}
}

func TestOpen_RejectsForeignDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v, want nil", err)
	}
	if _, err := db.Exec("CREATE TABLE something (x)"); err != nil {
		t.Fatalf("CREATE TABLE error = %v, want nil", err)
	}
	db.Close()

	if s, err := Open(context.Background(), path); err == nil {
		s.Close()
		t.Fatalf("Lyell のものではない DB を開けた, want error")
	}
}

func TestOpen_RejectsQuestionMarkInPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a?b.db")
	if s, err := Open(context.Background(), path); err == nil {
		s.Close()
		t.Fatalf("\"?\" を含むパスで開けた, want error")
	}
}

func createTable(name string) migration {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE "+name+" (x INTEGER) STRICT")
		return err
	}
}

func TestMigrate_UpgradeBacksUpFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lyell.db")
	ctx := context.Background()

	s, err := open(ctx, path, migrations)
	if err != nil {
		t.Fatalf("open() error = %v, want nil", err)
	}
	s.Close()

	next := append(append([]migration{}, migrations...), createTable("added"))
	s, err = open(ctx, path, next)
	if err != nil {
		t.Fatalf("schema version を上げる open() error = %v, want nil", err)
	}
	defer s.Close()

	if got := userVersion(t, path); got != len(next) {
		t.Errorf("DB の schema version = %d, want %d", got, len(next))
	}
	if _, err := s.read.Exec("SELECT x FROM added"); err != nil {
		t.Errorf("上げた schema version の表が無い: %v", err)
	}

	b := backups(t, path)
	if len(b) != 1 {
		t.Fatalf("複製の数 = %d (%v), want 1", len(b), b)
	}
	if got := userVersion(t, b[0]); got != len(migrations) {
		t.Errorf("複製の DB の schema version = %d, want 上げる前の %d", got, len(migrations))
	}
}

func TestMigrate_FailedStepIsRolledBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lyell.db")
	ctx := context.Background()

	s, err := open(ctx, path, migrations)
	if err != nil {
		t.Fatalf("open() error = %v, want nil", err)
	}
	s.Close()

	failing := func(ctx context.Context, tx *sql.Tx) error {
		if err := createTable("half")(ctx, tx); err != nil {
			return err
		}
		return errors.New("途中で失敗した")
	}
	next := append(append([]migration{}, migrations...), failing)
	if s, err := open(ctx, path, next); err == nil {
		s.Close()
		t.Fatalf("失敗する schema version で open() error = nil, want error")
	}

	if got := userVersion(t, path); got != len(migrations) {
		t.Errorf("DB の schema version = %d, want 失敗する前の %d", got, len(migrations))
	}
	s, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatalf("開き直す open() error = %v, want nil", err)
	}
	defer s.Close()
	if _, err := s.read.Exec("SELECT x FROM half"); err == nil {
		t.Errorf("失敗した schema version で作った表が残っている")
	}
}

// testID は, テスト用の16バイトの ID を作る.
func testID(b byte) []byte {
	id := make([]byte, 16)
	id[15] = b
	return id
}

func TestSchema_IsStrict(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// items の外部キーは延期されるので, トランザクションの中なら, INSERT で失敗するのは型だけになる.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v, want nil", err)
	}
	defer tx.Rollback()

	// ID を16バイトの BLOB ではなく文字列で入れると, STRICT が断る.
	_, err = tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", "0190a0b0-c0d0-7000-8000-000000000001", testID(2))
	if err == nil || !strings.Contains(err.Error(), "cannot store") {
		t.Errorf("BLOB の列に文字列を入れた時の error = %v, want cannot store", err)
	}
}

func TestSchema_ForeignKeysAreEnforced(t *testing.T) {
	s, _ := openTestStore(t)
	// revision_parents の外部キーは延期しないので, 無い revision を指すとこの文で失敗する.
	_, err := s.write.Exec("INSERT INTO revision_parents (revision_id, parent_id) VALUES (?, ?)", testID(1), testID(2))
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("無い revision を指した時の error = %v, want FOREIGN KEY", err)
	}
}

func TestSchema_DeferredForeignKeysAreCheckedAtCommit(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// 無い revision を head に指す item は, INSERT は通るが, コミットで失敗する.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v, want nil", err)
	}
	if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", testID(1), testID(2)); err != nil {
		t.Fatalf("延期される INSERT の error = %v, want nil", err)
	}
	if err := tx.Commit(); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("Commit() error = %v, want FOREIGN KEY", err)
	}

	// item と revision が揃っていれば, 互いを指していてもコミットできる.
	tx, err = s.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v, want nil", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", testID(1), testID(2)); err != nil {
		t.Fatalf("INSERT items error = %v, want nil", err)
	}
	if _, err := tx.Exec("INSERT INTO revisions (id, item_id) VALUES (?, ?)", testID(2), testID(1)); err != nil {
		t.Fatalf("INSERT revisions error = %v, want nil", err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("揃った時の Commit() error = %v, want nil", err)
	}
}

// inTx は, 書き込み用のトランザクションの中で f を実行し, コミットの結果を返す. f がエラーを返したら, 取り消してそのエラーを返す.
func inTx(t *testing.T, s *Store, f func(tx *sql.Tx) error) error {
	t.Helper()
	tx, err := s.write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v, want nil", err)
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// insertTestItem は, head だけを持つ item と, その head の revision (中身あり) を入れる.
func insertTestItem(tx *sql.Tx, item, head []byte) error {
	if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", item, head); err != nil {
		return err
	}
	return insertTestRevision(tx, head, item)
}

func insertTestRevision(tx *sql.Tx, id, item []byte) error {
	_, err := tx.Exec(`INSERT INTO revisions (id, item_id, created_at, author_user, author_machine, blob_sha256, body)
		VALUES (?, ?, 1700000000000, 'user', 'machine', ?, x'00')`, id, item, make([]byte, 32))
	return err
}

func TestSchema_RejectsWrongLengthIDs(t *testing.T) {
	s, _ := openTestStore(t)

	err := inTx(t, s, func(tx *sql.Tx) error { return insertTestItem(tx, make([]byte, 17), testID(2)) })
	if err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("17バイトの ID の error = %v, want CHECK", err)
	}

	err = inTx(t, s, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", testID(1), testID(2)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO revisions (id, item_id, created_at, author_user, author_machine, blob_sha256, body)
			VALUES (?, ?, 0, 'user', 'machine', ?, x'00')`, testID(2), testID(1), make([]byte, 31))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("31バイトの blob_sha256 の error = %v, want CHECK", err)
	}
}

func TestSchema_HeadMustBelongToItsItem(t *testing.T) {
	s, _ := openTestStore(t)
	itemA, headA, itemB, headB := testID(1), testID(2), testID(3), testID(4)

	if err := inTx(t, s, func(tx *sql.Tx) error {
		if err := insertTestItem(tx, itemA, headA); err != nil {
			return err
		}
		return insertTestItem(tx, itemB, headB)
	}); err != nil {
		t.Fatalf("2つの item を入れる error = %v, want nil", err)
	}

	// A の head を B の revision にすると, コミットで断られる.
	err := inTx(t, s, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE items SET head = ? WHERE id = ?", headB, itemA)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("別の item の revision を head にした error = %v, want FOREIGN KEY", err)
	}
}

func TestSchema_ParentMayBelongToAnotherItem(t *testing.T) {
	s, _ := openTestStore(t)
	itemA, headA, itemB, headB, merged := testID(1), testID(2), testID(3), testID(4), testID(5)

	// item の統合: B の新しい revision が, A と B の両方の head を親に持つ.
	err := inTx(t, s, func(tx *sql.Tx) error {
		for _, f := range []func() error{
			func() error { return insertTestItem(tx, itemA, headA) },
			func() error { return insertTestItem(tx, itemB, headB) },
			func() error { return insertTestRevision(tx, merged, itemB) },
			func() error {
				_, err := tx.Exec("INSERT INTO revision_parents VALUES (?, ?), (?, ?)", merged, headA, merged, headB)
				return err
			},
			func() error {
				_, err := tx.Exec("UPDATE items SET head = ? WHERE id = ?", merged, itemB)
				return err
			},
		} {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("統合の error = %v, want nil", err)
	}

	// 統合で親として指されている A の revision は, 消せない.
	err = inTx(t, s, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM revisions WHERE id = ?", headA)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("親として指されている revision を消した error = %v, want FOREIGN KEY", err)
	}
}

func TestSchema_LiveRevisionMustBeComplete(t *testing.T) {
	s, _ := openTestStore(t)

	insert := func(created, user, machine, sha, body any) error {
		return inTx(t, s, func(tx *sql.Tx) error {
			if _, err := tx.Exec("INSERT INTO items (id, kind, head) VALUES (?, 'asset', ?)", testID(1), testID(2)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO revisions (id, item_id, created_at, author_user, author_machine, blob_sha256, body)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, testID(2), testID(1), created, user, machine, sha, body)
			return err
		})
	}

	// body があるのに blob への参照が無い revision は断る.
	if err := insert(int64(0), "user", "machine", nil, []byte{0}); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("blob_sha256 の無い revision の error = %v, want CHECK", err)
	}
	// 間引いて作成者だけを残した骨格は, 将来の選択肢として通す.
	if err := insert(nil, "user", nil, nil, nil); err != nil {
		t.Errorf("作成者だけを残した骨格の error = %v, want nil", err)
	}
}

func TestSchema_Layout(t *testing.T) {
	s, _ := openTestStore(t)

	// WITHOUT ROWID の表には, 行番号の列が無い.
	for _, table := range []string{"items", "revision_parents"} {
		if _, err := s.read.Exec("SELECT rowid FROM " + table); err == nil {
			t.Errorf("%s に行番号がある, want WITHOUT ROWID", table)
		}
	}
	if _, err := s.read.Exec("SELECT rowid FROM revisions"); err != nil {
		t.Errorf("revisions に行番号が無い: %v", err)
	}

	for _, index := range []string{"items_head", "revision_parents_parent_id"} {
		var name string
		if err := s.read.QueryRow("SELECT name FROM sqlite_schema WHERE type = 'index' AND name = ?", index).Scan(&name); err != nil {
			t.Errorf("索引 %s が無い: %v", index, err)
		}
	}
}
