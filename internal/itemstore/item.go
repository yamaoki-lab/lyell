package itemstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/google/uuid"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// ID は item と revision の ID で, UUIDv7 (16バイト). 先頭に作った時刻 (ミリ秒) が入るので, 作った順に並ぶ.
type ID [16]byte

// NewID は, 今の時刻から新しい ID を作る.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return ID{}, err
	}
	return ID(u), nil
}

// String は, ハイフン区切りの見慣れた形 (0190a0b0-c0d0-7000-8000-000000000001) を返す.
func (id ID) String() string {
	return uuid.UUID(id).String()
}

// Kind は item の種類. 作った後は変わらない.
type Kind string

const (
	// KindAsset は, 中身 (blob) がバイト列そのものの item.
	KindAsset Kind = "asset"
	// KindCollection は, 中身 (blob) が他の item への参照の一覧の item.
	KindCollection Kind = "collection"
)

// Item は, item の記録.
type Item struct {
	ID   ID
	Kind Kind
	Head ID // 今指している revision
}

// Content は, revision の中身. 間引くと全て消え, ID と親だけの骨格が残る.
type Content struct {
	// CreatedAt は, revision を記録した時刻. DB にはミリ秒で持つので, それより細かい部分は切り捨てる. 中身が表す本来の日時は Body の側に持つ.
	CreatedAt time.Time
	// AuthorUser と AuthorMachine は, 記録した利用者と機体. どちらも必ず入れる. 認証の無い経路からの記録は, 両方 "anonymous" にする.
	AuthorUser    string
	AuthorMachine string
	// Blob は, 中身のバイト列 (blob) の SHA-256. 記録した後は, バイト列を手元に持っているか (転送から外した等) とは別になる. 持っているかは blobstore に聞く.
	Blob blobstore.Sum
	// BlobSize は, blob のバイト数. 記録する時は itemstore が blobstore から入れるので, 渡した値は使わない.
	BlobSize int64
	// Body は, revision の情報 (名前, メモ, content profile 等) を Protobuf で表したもの. itemstore は中身を解釈せず, 受け取ったバイト列をそのまま持つ. 空の Protobuf のメッセージは0バイトになるので, 空でもよい.
	Body []byte
}

// Revision は, revision の記録.
type Revision struct {
	ID      ID
	ItemID  ID
	Parents []ID // 1つ前の revision. 最初の revision は持たない. 統合では, 別の item の revision も親になる
	// Content は中身. 間引かれていれば nil.
	Content *Content
}

// ErrNotFound は, 指定した item や revision が無い時のエラー.
var ErrNotFound = errors.New("itemstore: 見つからない")

// ErrBlobNotFound は, Data を渡さずに記録しようとした時に, Content.Blob の blob が置かれていないことを表す.
var ErrBlobNotFound = errors.New("itemstore: blob が置かれていない")

// NewItem は, CreateItem で作る item の説明. 項目を足しても呼び出し側を直さずに済むよう, 引数を並べずにこの形で受け取る. 書かなかった項目は空の値になる.
type NewItem struct {
	Kind    Kind
	Content Content // 最初の revision の中身
	// Data は, 中身のバイト列 (blob). SHA-256 が Content.Blob と合わなければ何も記録しない. nil なら, blob は先に置いてあるものとし, 無ければ ErrBlobNotFound を返す (API で blob を RPC の外で先に送った時).
	Data io.Reader
	// PutOptions は, blob を書く時の選択肢 (置き場の指定等). 無ければ置き場の既定に従う.
	PutOptions []blobstore.PutOption
}

// CreateItem は, item を最初の revision と一緒に作る.
//
// blob を先に書き (書き終えるとディスクに届いている), その後で item と revision を1つのトランザクションで記録する. 逆の順にすると, 記録した後に落ちた時に, 無い blob を指す revision が残る. この順なら, 途中で落ちても孤立した blob が残るだけで, 後の GC で回収できる.
func (s *Store) CreateItem(ctx context.Context, n NewItem) (Item, error) {
	kind, content := n.Kind, n.Content
	if err := kind.validate(); err != nil {
		return Item{}, err
	}
	if err := content.validate(); err != nil {
		return Item{}, err
	}
	size, err := s.placeBlob(ctx, content.Blob, n.Data, n.PutOptions)
	if err != nil {
		return Item{}, err
	}
	content.BlobSize = size
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
	defer func() { _ = tx.Rollback() }() // Commit の後は何もしない

	// items と revisions は互いを指すが, その検査はコミットの時なので, 入れる順は問わない.
	if _, err := tx.ExecContext(ctx, "INSERT INTO items (id, kind, head) VALUES (?, ?, ?)", itemID[:], string(kind), revID[:]); err != nil {
		return Item{}, err
	}
	if err := insertRevision(ctx, tx, revID, itemID, content); err != nil {
		return Item{}, err
	}
	// 外部キーの検査はここで行われるので, このエラーを捨てると, 書けたつもりで何も残らない.
	if err := tx.Commit(); err != nil {
		return Item{}, err
	}
	return Item{ID: itemID, Kind: kind, Head: revID}, nil
}

// placeBlob は, 記録する前に blob が置かれていることを確かめ, その大きさを返す. data があれば先に書き, nil なら置いてあるものを使う. CreateItem と CommitRevision で共通.
//
// 置いてあることを確かめてから記録するまでの間に, blob が消されることは考えない. 誰からも指されない blob を消す GC を作る時に, 置いてすぐの blob を消さない猶予を設ける.
func (s *Store) placeBlob(ctx context.Context, sum blobstore.Sum, data io.Reader, opts []blobstore.PutOption) (int64, error) {
	if data != nil {
		if err := s.blobs.Put(ctx, sum, data, opts...); err != nil {
			return 0, fmt.Errorf("blob を書けない: %w", err)
		}
	}
	size, err := s.blobs.Size(ctx, sum)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("blob %s: %w", sum, ErrBlobNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("blob %s の大きさを読めない: %w", sum, err)
	}
	return size, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, id, itemID ID, c Content) error {
	// Body が nil だと NULL (間引かれた印) として入ってしまうので, 空でも必ず長さ0のバイト列として渡す.
	body := c.Body
	if body == nil {
		body = []byte{}
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO revisions (id, item_id, created_at, author_user, author_machine, blob_sha256, blob_size, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], itemID[:], c.CreatedAt.UnixMilli(), c.AuthorUser, c.AuthorMachine, c.Blob[:], c.BlobSize, body)
	return err
}

// Item は, id の item を返す. 無ければ ErrNotFound を返す.
func (s *Store) Item(ctx context.Context, id ID) (Item, error) {
	var (
		kind string
		head []byte
	)
	err := s.read.QueryRowContext(ctx, "SELECT kind, head FROM items WHERE id = ?", id[:]).Scan(&kind, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("item %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Item{}, err
	}
	item := Item{ID: id, Kind: Kind(kind)}
	if err := scanID(head, &item.Head); err != nil {
		return Item{}, err
	}
	return item, nil
}

// Revision は, id の revision を, 親の一覧と一緒に返す. 間引かれていれば Content は nil になるが, 親は辿れる. 無ければ ErrNotFound を返す.
func (s *Store) Revision(ctx context.Context, id ID) (Revision, error) {
	// 行と親の一覧を同じ時点のものとして読むため, 1つの読み取りのトランザクションにまとめる.
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Revision{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		itemID                    []byte
		createdAt, blobSize       sql.NullInt64
		authorUser, authorMachine sql.NullString
		blob, body                []byte
		thinned                   bool
	)
	// 長さ0の BLOB は, ドライバによっては nil として読める. 間引かれたかどうかは body の nil ではなく, IS NULL で DB に聞く.
	err = tx.QueryRowContext(ctx, `
SELECT item_id, created_at, author_user, author_machine, blob_sha256, blob_size, body, body IS NULL
FROM revisions WHERE id = ?`, id[:]).Scan(&itemID, &createdAt, &authorUser, &authorMachine, &blob, &blobSize, &body, &thinned)
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
	// 間引かれていない revision は, 中身の列を全て持つことを表の CHECK が保証している.
	if !thinned {
		if body == nil {
			body = []byte{}
		}
		c := &Content{
			CreatedAt:     time.UnixMilli(createdAt.Int64).UTC(),
			AuthorUser:    authorUser.String,
			AuthorMachine: authorMachine.String,
			BlobSize:      blobSize.Int64,
			Body:          body,
		}
		if len(blob) != len(c.Blob) {
			return Revision{}, fmt.Errorf("revision %s の blob_sha256 が %d バイト", id, len(blob))
		}
		copy(c.Blob[:], blob)
		rev.Content = c
	}

	rows, err := tx.QueryContext(ctx, "SELECT parent_id FROM revision_parents WHERE revision_id = ? ORDER BY parent_id", id[:])
	if err != nil {
		return Revision{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return Revision{}, err
		}
		var parent ID
		if err := scanID(raw, &parent); err != nil {
			return Revision{}, err
		}
		rev.Parents = append(rev.Parents, parent)
	}
	if err := rows.Err(); err != nil {
		return Revision{}, err
	}
	return rev, nil
}

func scanID(raw []byte, id *ID) error {
	if len(raw) != len(id) {
		return fmt.Errorf("ID が %d バイト (16バイトのはず)", len(raw))
	}
	copy(id[:], raw)
	return nil
}

func (k Kind) validate() error {
	switch k {
	case KindAsset, KindCollection:
		return nil
	}
	return fmt.Errorf("知らない item の種類: %q", k)
}

func (c Content) validate() error {
	if c.CreatedAt.IsZero() {
		return errors.New("記録した時刻 (CreatedAt) が無い")
	}
	if c.AuthorUser == "" || c.AuthorMachine == "" {
		return errors.New("記録した利用者と機体 (AuthorUser, AuthorMachine) は両方要る. 認証の無い経路なら anonymous にする")
	}
	return nil
}
