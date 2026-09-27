-- schema version 1: item と revision の3つの表 (nk-a-79/hozonnyou#119).
--
-- このファイルは公開した後に書き換えない. 既に適用した DB には二度と適用されないので, 書き換えると新しく作った DB と上げてきた DB で形が食い違う. 変更は次の番号のファイルで行う.
--
-- 表の決まりの理由は, 各 CREATE の中にコメントで書く. SQLite は文の中のコメントを DB のファイルに残すので, sqlite3 の .schema やドキュメントの道具からも読める (文の外のコメントは残らない).

CREATE TABLE items (
  -- item の記録. head は今指している revision.
  --
  -- STRICT: 列の型へ失わずに変換できない値を断る (例: BLOB の列に TEXT. TEXT の列の 123 は '123' として通る). 付けないと ID を BLOB と TEXT で入れた行が混ざっても気付けない.
  -- CHECK (length = 16): STRICT は BLOB であることしか見ないので, ID が16バイト (UUIDv7) であることを確かめる.
  -- 外部キー (id, head) → revisions (item_id, id): head は自分の item の revision だけを指せる. items と revisions は互いを指すので, 検査はコミットの時にまとめる (DEFERRABLE INITIALLY DEFERRED).
  -- 指されている行を消した時は既定のとおり断る. 連鎖して消す (CASCADE) と, 誤って消した時に祖先の繋がりが黙って失われる.
  -- WITHOUT ROWID: 行が小さいので, 表そのものを主キーの順に持つ. 行番号と主キーの索引を二重に持たずに済む.
  id   BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  kind TEXT NOT NULL,                                       -- asset / collection. 作成後に変わらない. 値の検査は Go の側で行う (種類が増えた時に表を作り直さずに済むように)
  head BLOB NOT NULL CHECK (length(head) = 16),
  FOREIGN KEY (id, head) REFERENCES revisions (item_id, id) DEFERRABLE INITIALLY DEFERRED
) STRICT, WITHOUT ROWID;

CREATE INDEX items_head
  -- revision を消す時の "どれかの item の head になっていないか" の検査が, 表を全部読まずに済むように.
  ON items (head);

CREATE TABLE revisions (
  -- revision の記録.
  --
  -- 中身 (created_at から body まで) は, 間引くと NULL になり, ID と親だけの骨格が残る. 間引かれていない (body がある) なら他の中身の列も必ずある, という片向きの CHECK にする. 間引いた後にどの列を残すかは, 後から変えても表を作り直さずに済む.
  -- blob_sha256 は blob への参照で, バイト列を手元に持っているか (転送から外した等) とは別. 持っているかは記録せず, blobstore に都度聞く.
  -- 主キーの NOT NULL: SQLite は古い互換のため, 行番号を持つ表の主キーに NULL を許してしまう.
  -- WITHOUT ROWID にしない: body が数 KB になりうるので, 行を主キーの順に並べると遅くなる.
  -- UNIQUE (item_id, id): items の (id, head) の外部キーの参照先. item_id で引く索引も兼ねる.
  id             BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  item_id        BLOB NOT NULL REFERENCES items (id) DEFERRABLE INITIALLY DEFERRED CHECK (length(item_id) = 16),
  created_at     INTEGER,  -- 記録した時刻. Unix ミリ秒 (UTC). 中身が表す本来の日時は body の側
  author_user    TEXT,     -- 記録した利用者. 認証の無い経路なら anonymous
  author_machine TEXT,     -- 記録した機体. 認証の無い経路なら anonymous
  blob_sha256    BLOB CHECK (length(blob_sha256) = 32),
  body           BLOB,     -- Protobuf. これが正で, 上の4列は body から計算し直せる. 空のメッセージは長さ0 (NULL ではない)
  UNIQUE (item_id, id),
  CHECK (body IS NULL OR (created_at IS NOT NULL AND author_user IS NOT NULL AND author_machine IS NOT NULL AND blob_sha256 IS NOT NULL))
) STRICT;

CREATE TABLE revision_parents (
  -- revision の親 (1つ前の revision). 1つの revision に0個 (最初の revision) から複数.
  --
  -- 親は別の item の revision でもよい (item の統合で, 両方の head を親に持つ revision ができる).
  -- 親は必ず先にあるので, 外部キーの検査は延期しない.
  revision_id BLOB NOT NULL REFERENCES revisions (id) CHECK (length(revision_id) = 16),
  parent_id   BLOB NOT NULL REFERENCES revisions (id) CHECK (length(parent_id) = 16),
  PRIMARY KEY (revision_id, parent_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX revision_parents_parent_id
  -- "この revision の子はどれか" を辿る時と, revision を消す時の "誰かの親になっていないか" の検査のため.
  ON revision_parents (parent_id);
