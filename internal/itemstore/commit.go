package itemstore

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFastForward は, 前提にした head が今の head と違う時のエラー. 他の書き手が先に head を進めたことを表す. 呼び出し側は, 今の head を読み直し, 新しい revision の親をそれに合わせて作り直すか, 分岐として扱う.
var ErrNotFastForward = errors.New("itemstore: 早送りでない")

// CommitRevision は, item に content を中身に持つ revision を足し, item の head をそれへ進める. 足した revision の ID を返す.
//
// 早送りだけを受け付ける. from は呼び出し側が前提にしている今の head で, parents に必ず含める (新しい revision が今の head の子孫であるため). parents には, item の統合なら別の item の head も入れてよい. head が from から動いていたら, 何も残さずに ErrNotFastForward を返す. 読んでから書くまでの間に他の書き手が head を進めても, 片方だけが通る.
//
// content.Blob の blob は, 先に blobstore へ書いておく.
func (s *Store) CommitRevision(ctx context.Context, itemID, from ID, parents []ID, content Content) (ID, error) {
	if err := content.validate(); err != nil {
		return ID{}, err
	}
	if err := validateParents(from, parents); err != nil {
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
