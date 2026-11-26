package itemstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// ErrNotFastForward は, 前提とした head が現在の head と異なる時のエラー. 他の書き込みが先に head を進めたことを表す. 呼び出し側は, 現在の head を読み直し, 新しい revision の親をそれに合わせて作り直すか, 分岐として扱う.
//
// CommitRevision は *NotFastForwardError を返し, errors.Is(err, ErrNotFastForward) は真になる. 現在の head が必要な時は errors.As で取り出す.
var ErrNotFastForward = errors.New("itemstore: 早送り (fast-forward) ではありません")

// NotFastForwardError は, CommitRevision が早送りでないために拒否した時のエラー. 拒否した時点の head を持つため, 呼び出し側は読み直さずに統合の処理へ進める.
type NotFastForwardError struct {
	ItemID ID
	From   ID // 呼び出し側が前提とした head
	Head   ID // 拒否した時点の head
}

func (e *NotFastForwardError) Error() string {
	return fmt.Sprintf("item %s の head が %s ではなく %s です: %v", e.ItemID, e.From, e.Head, ErrNotFastForward)
}

// Is は, errors.Is(err, ErrNotFastForward) を真にする. ErrNotFastForward で判別する既存の呼び出し側との互換性を保つため.
func (e *NotFastForwardError) Is(target error) bool {
	return target == ErrNotFastForward
}

// NewRevision は, CommitRevision で追加する revision の内容. 指定しなかったフィールドはゼロ値になる.
type NewRevision struct {
	ItemID ID
	// From は, 呼び出し側が前提としている現在の head.
	From ID
	// Parents は, 新しい revision の親. 空なら From だけを親にする (最も一般的な早送り). 指定する場合は From を必ず含める. item の統合では, 別の item の head も含めてよい.
	Parents []ID
	Content Content
	// Data は, 中身のバイト列 (blob). KindExtension の item は blob を持たないので使用しない. nil の場合は blob が保存済みであるとみなし, 存在しなければ ErrBlobNotFound を返す.
	Data io.Reader
	// PutOptions は, blob を書き込む時のオプション (保存先の指定等).
	PutOptions []blobstore.PutOption
}

// CommitRevision は, item に revision を追加し, item の head をその revision へ進める. 追加した revision の ID を返す.
//
// 早送りだけを受け付ける. 新しい revision は From (現在の head) の子孫でなければならない. head が From から動いていたら, 何も記録せずに *NotFastForwardError (ErrNotFastForward) を返す. 読み取ってから書き込むまでの間に他の書き込みが head を進めても, 一方だけが成功する.
//
// 引数が不正なら ErrInvalid を, item や親の revision が存在しなければ ErrNotFound を返す. 親の指定が不正な場合のエラーは *ParentsError 型で, errors.As で取り出せる.
//
// 中身のバイト列 (blob) は, CreateItem と同じく, 先に書き込んでから記録する. 拒否した時や記録に失敗した時は, 書き込んだ blob が孤立して残る. どこからも参照されない blob は無害で, 後の GC で回収できる.
func (s *Store) CommitRevision(ctx context.Context, r NewRevision) (ID, error) {
	r, err := s.prepareRevision(ctx, r)
	if err != nil {
		return ID{}, err
	}
	revID, err := NewID()
	if err != nil {
		return ID{}, err
	}

	// 書き込み用の接続は1本で, トランザクションは開始時に書き込みロックを取得する (BEGIN IMMEDIATE). 同時に呼ばれた CommitRevision は1つずつ順に実行される.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return ID{}, err
	}
	defer func() { _ = tx.Rollback() }() // Commit の後は何もしない

	if _, err := validateRevisionTarget(ctx, tx, r); err != nil {
		return ID{}, err
	}
	if err := insertRevision(ctx, tx, revID, r.ItemID, r.Content); err != nil {
		return ID{}, err
	}
	for _, p := range r.Parents {
		if _, err := tx.ExecContext(ctx, "INSERT INTO revision_links (source_revision, target_revision, type_name, type_major) VALUES (?, ?, ?, ?)", revID[:], p[:], linkTypeParent, linkTypeParentMajor); err != nil {
			return ID{}, fmt.Errorf("親 %s を記録できませんでした: %w", p, err)
		}
	}

	// 検証から更新まで同じトランザクション内で書き込みロックを保持するため, 他の書き込みがその間に head を進めることはない.
	if _, err := tx.ExecContext(ctx, "UPDATE items SET head = ? WHERE id = ?", revID[:], r.ItemID[:]); err != nil {
		return ID{}, err
	}

	if err := tx.Commit(); err != nil {
		return ID{}, err
	}
	return revID, nil
}

// ValidateCommitRevision は, CommitRevision の入力を読み取りトランザクションで検証し, 保存される revision の中身と親を返す. blob は保存済みである必要があり, Data と PutOptions は使用しない.
func (s *Store) ValidateCommitRevision(ctx context.Context, r NewRevision) (Revision, error) {
	// prepareRevision は読み取り用接続を自分で取るので, トランザクションを始める前に呼ぶ. 接続を1本持ったまま呼ぶと, 同時に呼ばれた数が読み取り用接続の数に達した時に全てが止まる. 種類は変わらず item も消えないので, 先に読んでも結果は同じ.
	r.Data = nil
	r, err := s.prepareRevision(ctx, r)
	if err != nil {
		return Revision{}, err
	}

	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Revision{}, err
	}
	defer func() { _ = tx.Rollback() }()

	parentItems, err := validateRevisionTarget(ctx, tx, r)
	if err != nil {
		return Revision{}, err
	}
	// Revision と同じく, 同じ item の親を先に, その中は ID 順に返す. 親は全て存在することを検証済み.
	parents := parentOrder(r.ItemID, r.Parents, func(p ID) (ID, bool) {
		item, ok := parentItems[p]
		return item, ok
	})
	return Revision{ItemID: r.ItemID, Parents: parents, Content: &r.Content}, nil
}

func (s *Store) prepareRevision(ctx context.Context, r NewRevision) (NewRevision, error) {
	if len(r.Parents) == 0 {
		r.Parents = []ID{r.From}
	}
	if err := r.Content.validate(); err != nil {
		return NewRevision{}, err
	}
	if err := validateParents(r.From, r.Parents); err != nil {
		return NewRevision{}, err
	}
	// 種類は作成後に変わらないので, 記録するトランザクションの外で読んでよい. blob を書き込む前に, 種類に合わない中身を弾く.
	item, err := s.Item(ctx, r.ItemID)
	if err != nil {
		return NewRevision{}, err
	}
	if err := validateContentForKind(item.Kind, r.Content, true); err != nil {
		return NewRevision{}, err
	}
	r.Content.BlobSize = 0
	if item.Kind == KindExtension {
		return r, nil
	}
	size, err := s.placeBlob(ctx, r.Content.Blob, r.Data, r.PutOptions)
	if err != nil {
		return NewRevision{}, err
	}
	r.Content.BlobSize = size
	return r, nil
}

// validateRevisionTarget は, 同じスナップショットで親の存在と現在の head を検証する. 親ごとに, その revision が属する item を返す (ValidateCommitRevision で親を並べるため).
func validateRevisionTarget(ctx context.Context, tx *sql.Tx, r NewRevision) (map[ID]ID, error) {
	parentItems := make(map[ID]ID, len(r.Parents))
	for _, p := range r.Parents {
		var raw []byte
		err := tx.QueryRowContext(ctx, "SELECT item_id FROM revisions WHERE id = ?", p[:]).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("親 revision %s: %w", p, ErrNotFound)
		}
		if err != nil {
			return nil, err
		}
		var item ID
		if err := scanID(raw, &item); err != nil {
			return nil, err
		}
		parentItems[p] = item
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT head FROM items WHERE id = ?", r.ItemID[:]).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("item %s: %w", r.ItemID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var head ID
	if err := scanID(raw, &head); err != nil {
		return nil, err
	}
	if head != r.From {
		return nil, &NotFastForwardError{ItemID: r.ItemID, From: r.From, Head: head}
	}
	return parentItems, nil
}

// maxParents は, 1つの revision が持てる parents の数の上限. fast-forward の parents は1つ, 統合の parents は2つで, 3つ以上を一度に統合する操作は存在しない. 上限が無いと, 1回の CommitRevision で大量の parents を記録でき, その間は唯一の書き込み接続を占有し続ける.
const maxParents = 2

// ParentsProblem は, 親の指定がどの決まりに反したかを表す.
type ParentsProblem int

const (
	// ParentsTooMany は, 親の数が上限を超えていること.
	ParentsTooMany ParentsProblem = iota + 1
	// ParentsDuplicate は, 同じ親が2回以上指定されていること.
	ParentsDuplicate
	// ParentsMissingFrom は, 親に前提の head (From) が含まれていないこと.
	ParentsMissingFrom
)

// ParentsError は, CommitRevision で親の指定が不正だった時のエラー. errors.Is(err, ErrInvalid) は true になる. API で, どの親がどの決まりに反したかを呼び出し側に返すために使う.
type ParentsError struct {
	Problem ParentsProblem
	// Index は, Problem が ParentsDuplicate の時, 重複した親のうち2つ目の位置 (NewRevision.Parents の添字). それ以外は -1.
	Index  int
	detail string
}

func (e *ParentsError) Error() string {
	return fmt.Sprintf("%v: %s", ErrInvalid, e.detail)
}

// Is は, errors.Is(err, ErrInvalid) で true を返すようにする. ErrInvalid で判別する呼び出し側との互換性を保つため.
func (e *ParentsError) Is(target error) bool {
	return target == ErrInvalid
}

func validateParents(from ID, parents []ID) error {
	// 親ごとに DB を参照する前に, 件数のみで拒否する.
	if len(parents) > maxParents {
		return &ParentsError{Problem: ParentsTooMany, Index: -1, detail: fmt.Sprintf("parents が %d 個指定されています (%d 個以下である必要があります)", len(parents), maxParents)}
	}
	seen := make(map[ID]bool, len(parents))
	for i, p := range parents {
		if seen[p] {
			return &ParentsError{Problem: ParentsDuplicate, Index: i, detail: fmt.Sprintf("親 %s が重複しています", p)}
		}
		seen[p] = true
	}
	if !seen[from] {
		return &ParentsError{Problem: ParentsMissingFrom, Index: -1, detail: fmt.Sprintf("親に前提の head %s が含まれていません. 早送り (fast-forward) になりません", from)}
	}
	return nil
}
