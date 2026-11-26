-- schema version 1: item と revision, revision のリンクとリンクの型の4つのテーブル (nk-a-79/hozonnyou#119, yamaoki-lab/lyell#41).
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
  -- extension: 開かれた時に中身を用意する拡張機能の名前 (lyell.collection 等). kind と同じく item に属し, 作成後は変わらず, revision ではなく item と一緒に移る. kind が extension の item だけが持ち, 他は NULL. kind との組み合わせと形式の検査は Go の側で行う.
  -- WITHOUT ROWID: 行が小さいので, テーブル自体を主キーの順に格納する. 行番号と主キーのインデックスを二重に持たずに済む.
  id   BLOB NOT NULL PRIMARY KEY CHECK (length(id) = 16),  -- UUIDv7
  kind TEXT NOT NULL,                                       -- asset / collection / extension. 作成後は変わらない. 値の検査は Go の側で行う (種類が増えた時にテーブルを作り直さずに済むように)
  extension TEXT,                                           -- 拡張機能の名前. 作成後は変わらない
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
  -- 中身 (created_at から body まで) は, 間引くと NULL になり, ID と一部のリンク (親を含む. revision_links を参照) だけが残る. 間引かれていない (body が NULL でない) なら記録時刻と作成者の列も必ず値を持つ, という一方向の CHECK にする. 間引いた後にどの列を残すかを後から変えても, テーブルを作り直さずに済む.
  -- blob_sha256 と blob_size は, 両方が値を持つか, 両方が NULL かのどちらか. 間引かれていない revision でも, 開かれた時に拡張機能が中身を用意する item の revision は blob を持たない. どの種類の item の revision が blob を持つかは, 種類が増えた時にテーブルを作り直さずに済むよう, Go の側で検査する.
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
  CHECK (body IS NULL OR (created_at IS NOT NULL AND author_user IS NOT NULL AND author_machine IS NOT NULL)),
  CHECK ((blob_sha256 IS NULL) = (blob_size IS NULL))
) STRICT;

CREATE TABLE link_types (
  -- revision のリンクの型の定義. 名前と major 番号ごとに1行.
  --
  -- ある型を知らない Lyell がそのリンクをどう扱うかは, 型の名前の綴りではなく, この定義のフラグで決める. フラグは行ごとではなく型ごとに1度だけ持つので, 二重に記録されて食い違うことはない.
  -- 同じ major 番号の中では, 新しい定義は, 古い Lyell が無視してよいフィールドを足す (minor) か, リンクの扱いを変えない修正 (patch) だけを行う. 定義を新しいものに置き換えても, 既存の行の意味は変わらない. 意味を変える時は新しい major 番号を割り当て, 古い定義とは別の行にする.
  -- 名前を数値にしないのは, 別の版の Lyell で同じ数値が別の型を表しうるため.
  -- 名前の CHECK: 英小文字で始まり, 英小文字, 数字, _ だけからなり, 64 バイト以下.
  -- keeps_target: リンクがある間は, リンク先を削除しない (1) か.
  -- copyable: 新しい revision を作る時に, 元の revision のリンクを引き継いでよい (1) か.
  -- unknown_fields: この版の Lyell が知らない, 定義の項目. minor で定義に項目を足すので, これが無いと, 新しい定義が古い Lyell を通った時に足された項目が失われる. revision_links の unknown_fields と同じく, sync で定義を Protobuf のメッセージとして運び, 知っている項目はそれぞれの列に, それ以外のバイト列はこの列に入れ, 送る時に組み立て直す. この版は常に NULL で書く (下の4つの型も NULL). 項目を足す版は, 古い行のこの列から新しい列へ1度だけ値を移す.
  -- WITHOUT ROWID: 行が小さいので, テーブル自体を主キーの順に格納する.
  name           TEXT NOT NULL CHECK (name GLOB '[a-z]*' AND name NOT GLOB '*[^a-z0-9_]*' AND length(CAST(name AS BLOB)) <= 64),
  major          INTEGER NOT NULL CHECK (major >= 0),
  minor          INTEGER NOT NULL CHECK (minor >= 0),
  patch          INTEGER NOT NULL CHECK (patch >= 0),
  keeps_target   INTEGER NOT NULL CHECK (keeps_target IN (0, 1)),
  copyable       INTEGER NOT NULL CHECK (copyable IN (0, 1)),
  unknown_fields BLOB,
  PRIMARY KEY (name, major)
) STRICT, WITHOUT ROWID;

-- この版の Lyell が知っている4つの型.
--   parent: 履歴上の1つ前の revision. 親は別の item の revision でもよい (item の統合では, 両方の head を親に持つ revision ができる).
--   required: リンクがある間はリンク先を削除せず, 一緒に転送する.
--   derived_from: item の作成元で, 拡張機能が出力を作る時に読む. どこから来たかを記録するだけで, 一緒に転送する必要はなく, 削除からも守らない.
--   belongs_to: リンク先を削除するとリンク元も削除する. 子の側に記録し, リンク先と型のインデックスで探す.
INSERT INTO link_types (name, major, minor, patch, keeps_target, copyable) VALUES
  ('parent', 1, 0, 0, 1, 0),
  ('required', 1, 0, 0, 1, 1),
  ('derived_from', 1, 0, 0, 0, 1),
  ('belongs_to', 1, 0, 0, 0, 1);

CREATE TABLE revision_links (
  -- revision からの型付きのリンク. 親も parent 型のリンクとして記録する. body には持たず, このテーブルが一次記録である.
  --
  -- リンク元は必ず revision. リンク先は, 特定の revision (target_revision) か, item (target_item. その時点の head, つまり "その item の最新版") のどちらか一方 (CHECK). parent 型は必ず revision を指す (CHECK).
  -- 同じリンク元とリンク先の組に, 型ごとに1行ずつ, 複数の型を持てる (レンディションは作成元の revision に derived_from と belongs_to を持つ).
  -- リンク元は revisions への外部キーを持つ. 型は link_types への外部キーを持ち, 定義の無い型の行は書けない.
  -- リンク先は外部キーを持たない. derived_from のリンク先は一緒に転送されないので, このマシンに無いことがある. ID は UUIDv7 でマシン間で衝突しないので, 無いリンク先も, sync する相手にその ID で問い合わせられる.
  -- 親の順序の列は持たない. 親は2つまでで, 1つは同じ item の expected_head, もう1つは統合される側の item の revision なので, 読む時に同じ item の親を先にする (その中は ID 順). 3つ以上を許す時に順序の列を足し, 既存の行はこの規則で埋める.
  -- unknown_fields: この版の Lyell が知らないフィールド. sync ではリンクを Protobuf のメッセージとして運び, 知っているフィールドはそれぞれの列に, それ以外のバイト列はこの列に入れ, 送る時に組み立て直す. 他の列に入るものは持たないので, 同じデータを二重に持たない. この版は常に NULL で書く. フィールドを足す版は, 古い行のこの列から新しい列へ1度だけ値を移す. これが無いと, 後から足したフィールドが古い Lyell を通った時に失われる. リンクの扱いを変えるものは, 行のフィールドではなく型のフラグか新しい major 番号で表すので, 後から足すフィールドは古い Lyell が無視してよいものに限られる.
  -- revision を間引く時は, parent, belongs_to, 未知の型のリンクを残し, required と derived_from のリンクを削除する. belongs_to が無いと, 間引いた後のレンディションを元の item の削除時に見つけられない. required と derived_from は開けなくなった revision では役割を持たない. 未知の型は要るかどうか判断できず, 行は数十バイトしかない.
  -- リンク元の revision は必ず先に記録するので, 外部キーの検査は遅延させない.
  source_revision BLOB NOT NULL REFERENCES revisions (id) CHECK (length(source_revision) = 16),
  target_revision BLOB CHECK (length(target_revision) = 16),
  target_item     BLOB CHECK (length(target_item) = 16),
  type_name       TEXT NOT NULL,
  type_major      INTEGER NOT NULL,
  unknown_fields  BLOB,
  FOREIGN KEY (type_name, type_major) REFERENCES link_types (name, major),
  CHECK ((target_revision IS NULL) <> (target_item IS NULL)),
  CHECK (type_name <> 'parent' OR target_revision IS NOT NULL)
) STRICT;

CREATE UNIQUE INDEX revision_links_target_revision
  -- 同じリンク元とリンク先の revision に, 型ごとに1行だけにする. "この revision の子はどれか" を辿る時や, revision を削除する前にリンク先になっていないかを確かめる時に, リンク元を探す.
  ON revision_links (target_revision, type_name, type_major, source_revision) WHERE target_revision IS NOT NULL;

CREATE UNIQUE INDEX revision_links_target_item
  -- 同じリンク元とリンク先の item に, 型ごとに1行だけにする. item を削除する前にリンク先になっていないかを確かめる時に, リンク元を探す.
  ON revision_links (target_item, type_name, type_major, source_revision) WHERE target_item IS NOT NULL;

CREATE INDEX revision_links_source_revision
  -- revision のリンク (親を含む) を読むため.
  ON revision_links (source_revision);
