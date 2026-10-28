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
var ErrNotFastForward = errors.New("itemstore: 早送りでない")

// NotFastForwardError は, CommitRevision が早送りでないために拒否した時のエラー. 拒否した時点の head を持つので, 呼び出し側は読み直さずに統合の処理へ進める.
type NotFastForwardError struct {
	ItemID ID
	From   ID // 呼び出し側が前提とした head
	Head   ID // 拒否した時点の head
}

func (e *NotFastForwardError) Error() string {
	return fmt.Sprintf("item %s の head が %s ではなく %s: %v", e.ItemID, e.From, e.Head, ErrNotFastForward)
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
	// Data は, 中身のバイト列 (blob). nil の場合は blob が保存済みであるとみなし, 存在しなければ ErrBlobNotFound を返す.
	Data io.Reader
	// PutOptions は, blob を書き込む時のオプション (保存先の指定等).
	PutOptions []blobstore.PutOption
}

// CommitRevision は, item に revision を追加し, item の head をその revision へ進める. 追加した revision の ID を返す.
//
// 早送りだけを受け付ける. 新しい revision は From (現在の head) の子孫でなければならない. head が From から動いていたら, 何も記録せずに *NotFastForwardError (ErrNotFastForward) を返す. 読み取ってから書き込むまでの間に他の書き込みが head を進めても, 一方だけが成功する.
//
// 引数が不正なら ErrInvalid を, item や親の revision が存在しなければ ErrNotFound を返す.
//
// 中身のバイト列 (blob) は, CreateItem と同じく, 先に書き込んでから記録する. 拒否した時や記録に失敗した時は, 書き込んだ blob が孤立して残る. どこからも参照されない blob は無害で, 後の GC で回収できる.
func (s *Store) CommitRevision(ctx context.Context, r NewRevision) (ID, error) {
	itemID, from, parents, content := r.ItemID, r.From, r.Parents, r.Content
	if len(parents) == 0 {
		parents = []ID{from}
	}
	if err := content.validate(); err != nil {
		return ID{}, err
	}
	if err := validateParents(from, parents); err != nil {
		return ID{}, err
	}
	size, err := s.placeBlob(ctx, content.Blob, r.Data, r.PutOptions)
	if err != nil {
		return ID{}, err
	}
	content.BlobSize = size
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

	if err := insertRevision(ctx, tx, revID, itemID, content); err != nil {
		return ID{}, err
	}
	for _, p := range parents {
		// 存在しない親は外部キーの制約でも拒否されるが, そのエラーは DB の故障と区別できないので, 先に確認して ErrNotFound を返す.
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM revisions WHERE id = ?)", p[:]).Scan(&exists); err != nil {
			return ID{}, err
		}
		if !exists {
			return ID{}, fmt.Errorf("親 revision %s: %w", p, ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO revision_parents (revision_id, parent_id) VALUES (?, ?)", revID[:], p[:]); err != nil {
			return ID{}, fmt.Errorf("親 %s を記録できない: %w", p, err)
		}
	}

	// compare-and-swap: head が from のままの時だけ進める. 読み取って比較してから書き込むと, その間に他の書き込みによって進められた head を上書きしうるので, 比較と書き込みを1つの SQL 文で行う.
	res, err := tx.ExecContext(ctx, "UPDATE items SET head = ? WHERE id = ? AND head = ?", revID[:], itemID[:], from[:])
	if err != nil {
		return ID{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ID{}, err
	}
	if n == 0 {
		// 追加した revision は Rollback で取り消される. 書き込みロックを保持したまま読み取るので, 読み取った head は拒否した時点のものである.
		var head []byte
		err := tx.QueryRowContext(ctx, "SELECT head FROM items WHERE id = ?", itemID[:]).Scan(&head)
		if errors.Is(err, sql.ErrNoRows) {
			return ID{}, fmt.Errorf("item %s: %w", itemID, ErrNotFound)
		}
		if err != nil {
			return ID{}, err
		}
		e := &NotFastForwardError{ItemID: itemID, From: from}
		if err := scanID(head, &e.Head); err != nil {
			return ID{}, err
		}
		return ID{}, e
	}

	if err := tx.Commit(); err != nil {
		return ID{}, err
	}
	return revID, nil
}

func validateParents(from ID, parents []ID) error {
	seen := make(map[ID]bool, len(parents))
	for _, p := range parents {
		if seen[p] {
			return fmt.Errorf("%w: 親 %s が重複している", ErrInvalid, p)
		}
		seen[p] = true
	}
	if !seen[from] {
		return fmt.Errorf("%w: 親に前提の head %s が含まれていない. 早送りにならない", ErrInvalid, from)
	}
	return nil
}
