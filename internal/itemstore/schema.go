package itemstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migration は, schema version を1つ上げる. 渡されたトランザクションの中だけで変更する.
type migration func(ctx context.Context, tx *sql.Tx) error

// migrations の i 番目 (0 始まり) は, schema version i を i+1 へ上げる. schema version は DB のファイルの user_version に入る. // yamaoki-lint:ignore
//
// 一度公開した関数は書き換えない. 既に上げ終えた DB には二度と適用されないので, 書き換えると, 新しく作った DB と上げてきた DB で形が食い違う. 変更は, 新しい関数を末尾に足して行う.
var migrations = []migration{
	migrateTo1,
}

// migrateTo1 は, item と revision の3つの表を作る (nk-a-79/hozonnyou#119).
//
// STRICT は, 列の型へ失わずに変換できない値を入れた時にエラーにする (例: BLOB の列に TEXT. 逆に TEXT の列の 123 は '123' に変換されて通る). 付けないと, SQLite は型を目安としてしか扱わず, 例えば ID を BLOB と TEXT で入れた行が混ざっても気付けない. 同じ ID なのに型が違うと一致しないので, "あるはずの行が見つからない" という形でしか表に出ない.
//
// STRICT は BLOB であることしか見ないので, ID は16バイト (UUIDv7), blob_sha256 は32バイトであることを CHECK で確かめる. 16進の文字列をそのままバイト列にして渡した, といった誤りを断る. 主キーには NOT NULL も付ける. SQLite は古い互換のため, 行番号を持つ表の主キーに NULL を許してしまう.
//
// items.head と revisions.item_id は互いを指すので, どちらを先に入れても1文ごとの検査では違反になる. そこで検査をコミットの時にまとめる (DEFERRABLE INITIALLY DEFERRED). head は (id, head) の組で revisions の (item_id, id) を指し, 別の item の revision を head にすると断る. revision_parents の親は別の item の revision でもよい (item の統合で, 両方の head を親に持つ revision ができる). 親は必ず先にあるので, 延期しない. 指されている行を消した時の動きは既定 (消せない) のままにする. 連鎖して消す (CASCADE) と, 誤って消した時に祖先の繋がりが黙って失われる.
//
// revisions の中身の4列と body は, 間引くと NULL になり, ID と親だけの骨格が残る. body があれば (間引かれていなければ) 他の4列も必ずある, という片向きの CHECK にする. 間引いた後にどの列を残すかは, 後から変えても表を作り直さずに済む. blob_sha256 は blob への参照で, バイト列を手元に持っているか (転送から外した等) とは別. 持っているかは記録せず, blobstore に都度聞く.
//
// 1行が小さい items と revision_parents は WITHOUT ROWID にし, 表そのものを主キーの順に持つ. 行番号と主キーの索引を二重に持たずに済み, 1回で引ける. body が数 KB になりうる revisions は, 行が大きいと遅くなるので付けない.
//
// 索引は, 外部キーの子の側の列に付ける. 無いと, revision を1つ消すたびに "どこかから指されていないか" の検査が表を全部読む. revisions の item_id は UNIQUE (item_id, id) が兼ねる.
func migrateTo1(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE items (
  id   BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  kind TEXT NOT NULL,                                       -- asset / collection. 作成後に変わらない
  head BLOB NOT NULL CHECK (length(head) = 16),
  FOREIGN KEY (id, head) REFERENCES revisions (item_id, id) DEFERRABLE INITIALLY DEFERRED
) STRICT, WITHOUT ROWID;

CREATE INDEX items_head ON items (head);

CREATE TABLE revisions (
  id             BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  item_id        BLOB NOT NULL REFERENCES items (id) DEFERRABLE INITIALLY DEFERRED CHECK (length(item_id) = 16),
  created_at     INTEGER,  -- 記録した時刻. Unix ミリ秒 (UTC). 中身が表す本来の日時は body の側
  author_user    TEXT,     -- 記録した利用者
  author_machine TEXT,     -- 記録した機体
  blob_sha256    BLOB CHECK (length(blob_sha256) = 32),
  body           BLOB,     -- Protobuf. これが正で, 上の4列は body から計算し直せる
  UNIQUE (item_id, id),
  CHECK (body IS NULL OR (created_at IS NOT NULL AND author_user IS NOT NULL AND author_machine IS NOT NULL AND blob_sha256 IS NOT NULL))
) STRICT;

CREATE TABLE revision_parents (
  revision_id BLOB NOT NULL REFERENCES revisions (id) CHECK (length(revision_id) = 16),
  parent_id   BLOB NOT NULL REFERENCES revisions (id) CHECK (length(parent_id) = 16),
  PRIMARY KEY (revision_id, parent_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX revision_parents_parent_id ON revision_parents (parent_id);
`)
	return err
}

// migrate は, DB を ms の最新の schema version まで上げる. schema version ごとに1つのトランザクションで適用するので, 途中で失敗しても, 失敗した schema version の変更だけが取り消される.
//
// 既に中身がある DB を上げる時は, 先に VACUUM INTO で DB を丸ごと複製する. 失敗した適用はトランザクションが取り消すが, "成功したが中身を壊した" 適用は取り消せないので, その時は複製から戻す.
func migrate(ctx context.Context, db *sql.DB, path string, ms []migration) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil { // yamaoki-lint:ignore
		return err
	}
	switch {
	case current > len(ms):
		// 古いプログラムで新しい DB を触ると, 知らない列や表を壊しうる.
		return fmt.Errorf("DB の schema version %d は, このプログラムが知る最新の schema version %d より新しい. 新しいプログラムで開くこと", current, len(ms))
	case current == len(ms):
		return nil
	case current == 0:
		// schema version が 0 なのに表があるなら, Lyell が作った DB ではない.
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%s は schema version が 0 だが, 既に %d 個の表などがある. Lyell の DB ではないかもしれない", path, n)
		}
	default:
		backup := fmt.Sprintf("%s.v%d-%s.bak", path, current, time.Now().UTC().Format("20060102T150405Z"))
		if _, err := db.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("schema version を上げる前の複製 (%s) を作れない: %w", backup, err)
		}
	}

	for v := current; v < len(ms); v++ {
		if err := applyMigration(ctx, db, ms[v], v+1); err != nil {
			return fmt.Errorf("schema version %d へ上げられない: %w", v+1, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, to int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit の後は何もしない
	if err := m(ctx, tx); err != nil {
		return err
	}
	// user_version もトランザクションの中で変わるので, 適用と schema version がずれることは無い. // yamaoki-lint:ignore
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", to)); err != nil { // yamaoki-lint:ignore
		return err
	}
	return tx.Commit()
}
