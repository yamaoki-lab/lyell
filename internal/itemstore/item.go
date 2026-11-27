package itemstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// ID は, item と revision の識別子で, UUIDv7 (16バイト). 先頭に生成時刻 (ミリ秒) を含むので, 生成順に並ぶ.
type ID [16]byte

// NewID は, 現在時刻から新しい ID を生成する.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return ID{}, err
	}
	return ID(u), nil
}

// String は, ハイフン区切りの標準の表記 (0190a0b0-c0d0-7000-8000-000000000001) を返す.
func (id ID) String() string {
	return uuid.UUID(id).String()
}

// Kind は, item の種類. 作成後は変更不可.
type Kind string

const (
	// KindAsset は, 中身 (blob) がバイト列そのものである item.
	KindAsset Kind = "asset"
	// KindCollection は, 中身 (blob) が, 他の item への参照の一覧である item.
	KindCollection Kind = "collection"
	// KindExtension は, 開かれた時に拡張機能が中身を用意する item. 別の item のレンディションや現在時刻のように, 保存したバイト列ではなく開いた時に作られる中身を表す. item は中身を用意する拡張機能の名前 (Item.Extension) を持ち, revision は blob を持たずに, その拡張機能だけが読むデータを body の extension_data に持つ. 開いた時に作られるバイト列は, マシンごとに異なりうるので revision には保存しない.
	KindExtension Kind = "extension"
)

// Item は, item のレコード.
type Item struct {
	ID   ID
	Kind Kind
	// Extension は, 開かれた時に中身を用意する拡張機能の名前 (lyell.collection 等). Kind が KindExtension の時だけ値を持つ. Kind と同じく item に属し, 作成後は変更不可.
	Extension string
	Head      ID // 現在指している revision
}

// Content は, revision の中身. 間引くと全て削除され, ID と一部のリンク (親を含む) だけが残る.
type Content struct {
	// CreatedAt は, revision を記録した時刻. DB にはミリ秒単位で保存するので, それより細かい部分は切り捨てる. 中身が表す本来の日時は Body に持たせる.
	CreatedAt time.Time
	// AuthorUser と AuthorMachine は, 記録した利用者とマシン. どちらも必須. 認証を経ない経路からの記録では, 両方を "anonymous" にする.
	AuthorUser    string
	AuthorMachine string
	// Blob は, 中身のバイト列 (blob) の SHA-256. ゼロ値は, blob を持たないこと (開かれた時に拡張機能が中身を用意する item の revision) を表す. 記録した後は, バイト列がローカルに保持されているか (転送の対象から外した場合等) とは無関係である. ローカルに存在するかは blobstore に問い合わせる.
	Blob blobstore.Sum
	// BlobSize は, blob のバイト数. blob を持たない時は 0. 記録時は itemstore が blobstore から取得した値を入れるので, 渡された値は使用しない.
	BlobSize int64
	// Body は, revision の情報を lyell.storage.v1alpha1.RevisionBody で表したバイト列. revision の一次情報で, 上のフィールドはここから作り直せる. 記録する時は, itemstore が上のフィールドの値で Body の同じフィールドを上書きし, 他のフィールド (名前や, この Lyell が知らないもの) はそのまま残す. 空でもよい. 読み取り時には, 記録した body 全体が入る.
	Body []byte
}

// Revision は, revision のレコード.
type Revision struct {
	ID      ID
	ItemID  ID
	Parents []ID // 1つ前の revision. 初回の revision には存在しない. item の統合では, 別の item の revision も親になる.
	// Links は, 親以外のリンク. 型とリンク先の順に並ぶ. 間引かれた revision でも, 残すリンク (0001.sql の revision_links) を返す.
	Links []Link
	// Content は中身. 間引かれていれば nil.
	Content *Content
}

// ErrNotFound は, 指定した item や revision が存在しない時のエラー.
var ErrNotFound = errors.New("itemstore: 見つかりません")

// ErrBlobNotFound は, Data を渡さずに記録しようとした時に, Content.Blob の blob が存在しないことを表す.
var ErrBlobNotFound = errors.New("itemstore: blob が見つかりません")

// ErrInvalid は, 引数が不正な時のエラー (不明な種類, 親の重複等). 呼び出し側の誤りなので, 同じ引数で再試行しても成功しない. API では, サーバの故障と区別して呼び出し側に返すために使う.
var ErrInvalid = errors.New("itemstore: 引数が不正です")

// NewItem は, CreateItem で作成する item の内容. フィールドを追加した時に呼び出し側のコードの変更を不要にするため, 引数を並べずに構造体で受け取る. 指定しなかったフィールドはゼロ値になる.
type NewItem struct {
	Kind Kind
	// Extension は, 中身を用意する拡張機能の名前. Kind が KindExtension の時は必須で, それ以外では空にする.
	Extension string
	Content   Content // 最初の revision の中身
	// Data は, 中身のバイト列 (blob). KindExtension の item は blob を持たないので使用しない. SHA-256 が Content.Blob と一致しなければ, 何も記録しない. nil の場合は blob が保存済みであるとみなし, 存在しなければ ErrBlobNotFound を返す (API で, blob を RPC とは別に先に送った場合).
	Data io.Reader
	// PutOptions は, blob を書き込む時のオプション (保存先の指定等). 無ければ保存先の既定に従う.
	PutOptions []blobstore.PutOption
	// Links と LinkTypes は, 最初の revision からの親以外のリンクと, それに使う型の定義. NewRevision と同じ (レンディションは作成時から derived_from を持つ).
	Links     []Link
	LinkTypes []LinkTypeDefinition
}

// CreateItem は, item を最初の revision と一緒に作成する. 引数が不正なら ErrInvalid を返す.
//
// 先に blob を書き込み (書き込みが完了した時点でディスクに永続化されている), その後で item と revision を1つのトランザクションで記録する. 逆の順にすると, 記録した後にクラッシュした場合に, 存在しない blob を指す revision が残る. この順なら, 途中でクラッシュしても孤立した blob が残るだけで, 後の GC で回収できる.
func (s *Store) CreateItem(ctx context.Context, n NewItem) (Item, error) {
	content, err := s.prepareNewItem(ctx, n, true)
	if err != nil {
		return Item{}, err
	}
	links, err := validateLinks(n.Links, n.LinkTypes)
	if err != nil {
		return Item{}, err
	}
	itemID, err := NewID()
	if err != nil {
		return Item{}, err
	}
	revID, err := NewID()
	if err != nil {
		return Item{}, err
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 済みの場合は何も行わない

	// items と revisions は互いを参照するが, 外部キーの検査はコミット時に行われるので, 挿入の順序は問わない.
	if _, err := tx.ExecContext(ctx, "INSERT INTO items (id, kind, extension, head) VALUES (?, ?, ?, ?)", itemID[:], string(n.Kind), extensionColumn(n.Extension), revID[:]); err != nil {
		return Item{}, err
	}
	if err := registerLinkTypes(ctx, tx, n.LinkTypes, true); err != nil {
		return Item{}, err
	}
	if err := insertRevision(ctx, tx, revID, itemID, content); err != nil {
		return Item{}, err
	}
	if err := insertLinks(ctx, tx, revID, links); err != nil {
		return Item{}, err
	}
	// 外部キーの検査はここで行われるので, このエラーを無視すると, 書き込みに成功したと誤認したまま何も保存されない.
	if err := tx.Commit(); err != nil {
		return Item{}, err
	}
	return Item{ID: itemID, Kind: n.Kind, Extension: n.Extension, Head: revID}, nil
}

// ValidateCreateItem は, CreateItem の入力を検証し, 最初の revision の中身を返す. リンクは受け取らない (API はまだ親以外のリンクを渡さない. #42). hasBlob が false なら blob の検証を省略する. KindExtension の item は blob を持たないので, hasBlob によらず blob を検証しない.
func (s *Store) ValidateCreateItem(ctx context.Context, kind Kind, extension string, content Content, hasBlob bool) (Content, error) {
	return s.prepareNewItem(ctx, NewItem{Kind: kind, Extension: extension, Content: content}, hasBlob)
}

func (s *Store) prepareNewItem(ctx context.Context, n NewItem, hasBlob bool) (Content, error) {
	if err := n.Kind.validate(); err != nil {
		return Content{}, err
	}
	if err := validateItemExtension(n.Kind, n.Extension); err != nil {
		return Content{}, err
	}
	if err := n.Content.validate(); err != nil {
		return Content{}, err
	}
	if err := validateContentForKind(n.Kind, n.Content, hasBlob); err != nil {
		return Content{}, err
	}
	content := n.Content
	content.BlobSize = 0
	if hasBlob && n.Kind != KindExtension {
		size, err := s.placeBlob(ctx, content.Blob, n.Data, n.PutOptions)
		if err != nil {
			return Content{}, err
		}
		content.BlobSize = size
	}
	return content, nil
}

// placeBlob は, 記録の前に blob が存在することを確認し, そのサイズを返す. data があれば先に書き込み, nil なら保存済みの blob を使う. CreateItem と CommitRevision で共通の処理.
//
// 確認してから記録するまでの間に, blob が削除されることは考慮しない. どこからも参照されない blob を削除する GC を実装する時に, 作成直後の blob を削除しない猶予期間を設ける.
func (s *Store) placeBlob(ctx context.Context, sum blobstore.Sum, data io.Reader, opts []blobstore.PutOption) (int64, error) {
	if data != nil {
		if _, err := s.blobs.Put(ctx, sum, data, opts...); err != nil {
			return 0, fmt.Errorf("blob の書き込みに失敗しました: %w", err)
		}
	}
	size, err := s.blobs.Size(ctx, sum)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("blob %s: %w", sum, ErrBlobNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("blob %s のサイズ取得に失敗しました: %w", sum, err)
	}
	return size, nil
}

// extensionColumn は, items の extension の列に書く値を返す. 拡張機能の名前を持たない item では NULL にする.
func extensionColumn(name string) any {
	if name == "" {
		return nil
	}
	return name
}

func insertRevision(ctx context.Context, tx *sql.Tx, id, itemID ID, c Content) error {
	body, err := encodeBody(c)
	if err != nil {
		return err
	}
	// 列は c からではなく body から求める. 列と body が食い違わないようにするため.
	cols, err := columnsFromBody(body)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO revisions (id, item_id, created_at, author_user, author_machine, blob_sha256, blob_size, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], itemID[:], cols.createdAt, cols.authorUser, cols.authorMachine, cols.blob, cols.blobSizeColumn(), body)
	return err
}

// Item は, id の item を返す. 存在しなければ ErrNotFound を返す.
func (s *Store) Item(ctx context.Context, id ID) (Item, error) {
	var (
		kind      string
		extension sql.NullString
		head      []byte
	)
	err := s.read.QueryRowContext(ctx, "SELECT kind, extension, head FROM items WHERE id = ?", id[:]).Scan(&kind, &extension, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("item %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Item{}, err
	}
	item := Item{ID: id, Kind: Kind(kind), Extension: extension.String}
	if err := scanID(head, &item.Head); err != nil {
		return Item{}, err
	}
	return item, nil
}

// Revision は, id の revision を, 親の一覧と一緒に返す. 間引かれていれば Content は nil になるが, 親は辿れる. 存在しなければ ErrNotFound を返す.
func (s *Store) Revision(ctx context.Context, id ID) (Revision, error) {
	// 行と親の一覧を同じ時点のスナップショットから取得するため, 1つの読み取り専用のトランザクションで読み取る.
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Revision{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		itemID []byte
		cols   contentColumns
	)
	err = tx.QueryRowContext(ctx, "SELECT item_id, "+contentColumnList+" FROM revisions WHERE id = ?", id[:]).
		Scan(append([]any{&itemID}, cols.dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, fmt.Errorf("revision %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Revision{}, err
	}

	rev := Revision{ID: id}
	if err := scanID(itemID, &rev.ItemID); err != nil {
		return Revision{}, err
	}
	if rev.Content, err = cols.content(id); err != nil {
		return Revision{}, err
	}
	if rev.Parents, err = readParents(ctx, tx, id); err != nil {
		return Revision{}, err
	}
	if rev.Links, err = readLinks(ctx, tx, id); err != nil {
		return Revision{}, err
	}
	return rev, nil
}

// contentColumnList は, revisions から中身を取得する際のカラム一覧. contentColumns.dest と同じ順序で定義する.
//
// 長さ0の BLOB は, ドライバによっては nil として読み取られる. 間引かれたかどうかは, body が nil かではなく, IS NULL で DB に問い合わせて判定する.
const contentColumnList = "created_at, author_user, author_machine, blob_sha256, blob_size, body, body IS NULL"

// contentColumns は, contentColumnList のカラム値を読み出す構造体.
type contentColumns struct {
	createdAt, blobSize       sql.NullInt64
	authorUser, authorMachine sql.NullString
	blob, body                []byte
	thinned                   bool
}

func (c *contentColumns) dest() []any {
	return []any{&c.createdAt, &c.authorUser, &c.authorMachine, &c.blob, &c.blobSize, &c.body, &c.thinned}
}

// content は, 読み取った列から revision id の中身を組み立てる. 間引かれていれば nil を返す.
func (c *contentColumns) content(id ID) (*Content, error) {
	if c.thinned {
		return nil, nil
	}
	// 間引かれていない revision が記録時刻と作成者の列を持つこと, および blob_sha256 と blob_size が両方あるか両方無いかは, テーブルの CHECK 制約が保証している.
	body := c.body
	if body == nil {
		body = []byte{}
	}
	content := &Content{
		CreatedAt:     time.UnixMilli(c.createdAt.Int64).UTC(),
		AuthorUser:    c.authorUser.String,
		AuthorMachine: c.authorMachine.String,
		BlobSize:      c.blobSize.Int64,
		Body:          body,
	}
	// blob を持たない revision では, Blob はゼロ値のまま (blob_size も NULL なので BlobSize は 0).
	if c.blob == nil {
		return content, nil
	}
	if len(c.blob) != len(content.Blob) {
		return nil, fmt.Errorf("revision %s の blob_sha256 のバイト数が不正です (%d バイト)", id, len(c.blob))
	}
	copy(content.Blob[:], c.blob)
	return content, nil
}

// readParents は, revision id の親 (parent 型のリンクのリンク先) を返す. 最初の revision なら nil を返す.
//
// 親は, id と同じ item の revision を先に, その中は ID 順に並べる (parentOrder と同じ順序). リンク先はこのマシンに無いこともあり, 無い親は item が分からないので最後に置く.
func readParents(ctx context.Context, tx *sql.Tx, id ID) ([]ID, error) {
	// リンク先の行が無くても親を落とさないよう, LEFT JOIN にする. 同じ item の親, 別の item の親, 無い親 (t.item_id が NULL) の順に並べる.
	rows, err := tx.QueryContext(ctx, `SELECT l.target_revision FROM revision_links l
		JOIN revisions s ON s.id = l.source_revision
		LEFT JOIN revisions t ON t.id = l.target_revision
		WHERE l.source_revision = ? AND l.type_name = ? AND l.type_major = ?
		ORDER BY t.item_id IS NOT s.item_id, t.item_id IS NULL, l.target_revision`, id[:], LinkParent.Name, LinkParent.Major)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var parents []ID
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var parent ID
		if err := scanID(raw, &parent); err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	return parents, rows.Err()
}

// parentOrder は, 親の並び順を返す. 親は2つまでで, 1つは同じ item の From, もう1つは統合される側の item の revision なので, 順序の列を持たずに, 同じ item の親を先にする. 3つ以上を許す時は, 順序の列を足す (0001.sql の revision_links).
//
// 同じ item の親, 別の item の親, item が分からない (このマシンに無い) 親の順で, それぞれの中は ID 順. itemOf は親の item を返し, 分からなければ ok を false にする.
func parentOrder(itemID ID, parents []ID, itemOf func(ID) (ID, bool)) []ID {
	rank := func(p ID) int {
		item, ok := itemOf(p)
		switch {
		case !ok:
			return 2
		case item == itemID:
			return 0
		}
		return 1
	}
	sorted := slices.Clone(parents)
	slices.SortFunc(sorted, func(a, b ID) int {
		if c := rank(a) - rank(b); c != 0 {
			return c
		}
		return bytes.Compare(a[:], b[:])
	})
	return sorted
}

func scanID(raw []byte, id *ID) error {
	if len(raw) != len(id) {
		return fmt.Errorf("ID が %d バイトです (16バイトである必要があります)", len(raw))
	}
	copy(id[:], raw)
	return nil
}

func (k Kind) validate() error {
	switch k {
	case KindAsset, KindCollection, KindExtension:
		return nil
	}
	return fmt.Errorf("%w: item 種別 %q は不明です", ErrInvalid, k)
}

// hasBlob は, 中身が blob を持つかを返す.
func (c Content) hasBlob() bool {
	return c.Blob != blobstore.Sum{}
}

func (c Content) validate() error {
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: 記録時刻 (CreatedAt) が指定されていません", ErrInvalid)
	}
	if c.AuthorUser == "" || c.AuthorMachine == "" {
		return fmt.Errorf("%w: 記録した利用者とマシン (AuthorUser, AuthorMachine) は両方必須です. 認証を経ない経路では anonymous にしてください", ErrInvalid)
	}
	// blob を書き込む前に弾けるよう, Body が読めるかもここで確かめる.
	if _, err := encodeBody(c); err != nil {
		return err
	}
	return nil
}
