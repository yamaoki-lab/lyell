package itemstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// ErrNotFastForward は, 前提にした head が今の head と違う時のエラー. 他の書き手が先に head を進めたことを表す. 呼び出し側は, 今の head を読み直し, 新しい revision の親をそれに合わせて作り直すか, 分岐として扱う.
var ErrNotFastForward = errors.New("itemstore: 早送りでない")

// NewRevision は, CommitRevision で足す revision の説明. 書かなかった項目は空の値になる.
type NewRevision struct {
	ItemID ID
	// From は, 呼び出し側が前提にしている今の head.
	From ID
	// Parents は新しい revision の親. 空なら From だけを親にする (一番よくある早送り). 書く時は From を必ず含める. item の統合なら, 別の item の head も入れてよい.
	Parents []ID
	Content Content
	Data    io.Reader // 中身のバイト列 (blob)
	// PutOptions は, blob を書く時の選択肢 (置き場の指定等).
	PutOptions []blobstore.PutOption
}

// CommitRevision は, item に revision を足し, item の head をそれへ進める. 足した revision の ID を返す.
//
// 早送りだけを受け付ける. 新しい revision は From (今の head) の子孫でなければならない. head が From から動いていたら, 何も残さずに ErrNotFastForward を返す. 読んでから書くまでの間に他の書き手が head を進めても, 片方だけが通る.
//
// 中身のバイト列 (blob) は, CreateItem と同じく, 先に書いてから記録する. 断った時や記録に失敗した時は, 書いた blob が孤立して残る. 誰からも指されない blob は無害で, 後の GC で回収できる.
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
	if err := s.putBlob(ctx, content.Blob, r.Data, r.PutOptions); err != nil {
		return ID{}, err
	}
	revID, err := NewID()
	if err != nil {
		return ID{}, err
	}

	// 書き込み用の接続は1本で, トランザクションは最初から書き込みのロックを取る (BEGIN IMMEDIATE). 同時に来た CommitRevision は1つずつ順に入る.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return ID{}, err
	}
	defer tx.Rollback() // Commit の後は何もしない

	if err := insertRevision(ctx, tx, revID, itemID, content); err != nil {
		return ID{}, err
	}
	for _, p := range parents {
		if _, err := tx.ExecContext(ctx, "INSERT INTO revision_parents (revision_id, parent_id) VALUES (?, ?)", revID[:], p[:]); err != nil {
			return ID{}, fmt.Errorf("親 %s を記録できない: %w", p, err)
		}
	}

	// 比較交換: head が from のままの時だけ進める. 読んで比べてから書くと, その間に他が進めた head を上書きしうるので, 比べることと書くことを1つの文で行う.
	res, err := tx.ExecContext(ctx, "UPDATE items SET head = ? WHERE id = ? AND head = ?", revID[:], itemID[:], from[:])
	if err != nil {
		return ID{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ID{}, err
	}
	if n == 0 {
		// 足した revision は Rollback で消える.
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM items WHERE id = ?)", itemID[:]).Scan(&exists); err != nil {
			return ID{}, err
		}
		if !exists {
			return ID{}, fmt.Errorf("item %s: %w", itemID, ErrNotFound)
		}
		return ID{}, fmt.Errorf("item %s の head が %s ではない: %w", itemID, from, ErrNotFastForward)
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
			return fmt.Errorf("親 %s が重なっている", p)
		}
		seen[p] = true
	}
	if !seen[from] {
		return fmt.Errorf("親に前提の head %s が無い. 早送りにならない", from)
	}
	return nil
}
