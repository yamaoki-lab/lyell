-- schema version 1: item と revision の3つのテーブル (nk-a-79/hozonnyou#119).
--
-- このファイルは公開後に変更しない. 適用済みの DB には二度と適用されないので, 変更すると, 新しく作成した DB とマイグレーションしてきた DB でスキーマが食い違う. 変更は次の番号のファイルで行う.
-- 公開とは, Lyell の最初のタグ (v0.1.0) を打つこと. それより前は実データを記録した DB が存在しないので, 変更してよい (その場合は schema_test.go の SHA-256 も更新する).
--
-- テーブルの制約の理由は, 各 CREATE 文の中にコメントで書く. SQLite は文の中のコメントを DB のファイルに保存するので, sqlite3 の .schema やドキュメントの生成ツールからも読める (文の外のコメントは保存されない).

CREATE TABLE items (
  -- item の記録. head は現在指している revision.
  --
  -- STRICT: 列の型へ情報を失わずに変換できない値を拒否する (例: BLOB の列への TEXT. TEXT の列への 123 は '123' として受け付ける). 指定しないと, ID を BLOB と TEXT で挿入した行が混在しても検知できない.
  -- CHECK (length = 16): STRICT は BLOB であることしか検査しないので, ID が16バイト (UUIDv7) であることを検査する.
  -- 外部キー (id, head) → revisions (item_id, id): head は自身の item の revision だけを参照できる. items と revisions は互いを参照するので, 検査はコミット時にまとめて行う (DEFERRABLE INITIALLY DEFERRED).
  -- 参照されている行の削除は, 既定のとおり拒否する. 連鎖して削除する (CASCADE) と, 誤って削除した時に祖先との繋がりが警告なく失われる.
  -- WITHOUT ROWID: 行が小さいので, テーブル自体を主キーの順に格納する. 行番号と主キーのインデックスを二重に持たずに済む.
  id   BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  kind TEXT NOT NULL,                                       -- asset / collection. 作成後は変わらない. 値の検査は Go の側で行う (種類が増えた時にテーブルを作り直さずに済むように)
  head BLOB NOT NULL CHECK (length(head) = 16),
  FOREIGN KEY (id, head) REFERENCES revisions (item_id, id) DEFERRABLE INITIALLY DEFERRED
) STRICT, WITHOUT ROWID;

CREATE INDEX items_head
  -- revision を削除する時の "どの item の head にもなっていないか" の検査で, テーブル全体を読まずに済むように.
  ON items (head);

CREATE INDEX items_kind_id
  -- item の一覧を kind で絞り込む際, テーブル全体をスキャンせず, 続きの位置から ID 順に取得するため.
  ON items (kind, id);

CREATE TABLE revisions (
  -- revision の記録.
  --
  -- 中身 (created_at から body まで) は, 間引くと NULL になり, ID と親だけが残る. 間引かれていない (body が NULL でない) なら他の中身の列も必ず値を持つ, という一方向の CHECK にする. 間引いた後にどの列を残すかを後から変えても, テーブルを作り直さずに済む.
  -- blob_sha256 は blob への参照で, バイト列が手元にあるか (転送の対象から外した場合等) とは独立している. 手元にあるかは記録せず, その都度 blobstore に問い合わせる.
  -- blob_size も記録する. 作業コピーを OS のファイルシステムとして見せる時に, 一覧を表示するたびに blob のサイズを問い合わせることを避けるため. blob が手元に無いマシンでも答えられる.
  -- 主キーの NOT NULL: SQLite は過去との互換性のため, 行番号を持つテーブルの主キーに NULL を許してしまう.
  -- WITHOUT ROWID にしない: body が数 KB になりうるので, 行を主キーの順に格納すると遅くなる.
  -- UNIQUE (item_id, id): items の外部キー (id, head) の参照先. item_id で検索するインデックスも兼ねる.
  id             BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  item_id        BLOB NOT NULL REFERENCES items (id) DEFERRABLE INITIALLY DEFERRED CHECK (length(item_id) = 16),
  created_at     INTEGER,  -- 記録時刻. Unix ミリ秒 (UTC). 中身が表す本来の日時は body に持たせる
  author_user    TEXT,     -- 記録した利用者. 認証を経ない経路では anonymous
  author_machine TEXT,     -- 記録したマシン. 認証を経ない経路では anonymous
  blob_sha256    BLOB CHECK (length(blob_sha256) = 32),
  blob_size      INTEGER CHECK (blob_size >= 0),  -- blob のバイト数
  body           BLOB,     -- Protobuf. これが正であり, 上の5列は body から再計算できる. 空のメッセージは長さ0 (NULL ではない)
  UNIQUE (item_id, id),
  CHECK (body IS NULL OR (created_at IS NOT NULL AND author_user IS NOT NULL AND author_machine IS NOT NULL AND blob_sha256 IS NOT NULL AND blob_size IS NOT NULL))
) STRICT;

CREATE TABLE revision_parents (
  -- revision の親 (1つ前の revision). 1つの revision につき0個 (最初の revision) 以上.
  --
  -- 親は別の item の revision でもよい (item の統合では, 両方の head を親に持つ revision ができる).
  -- 親は必ず先に存在するので, 外部キーの検査は遅延させない.
  revision_id BLOB NOT NULL REFERENCES revisions (id) CHECK (length(revision_id) = 16),
  parent_id   BLOB NOT NULL REFERENCES revisions (id) CHECK (length(parent_id) = 16),
  PRIMARY KEY (revision_id, parent_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX revision_parents_parent_id
  -- "この revision の子はどれか" を辿る時と, revision を削除する時の "他の revision の親になっていないか" の検査のため.
  ON revision_parents (parent_id);
