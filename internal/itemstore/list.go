package itemstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Order は, ListItems における item の並び順.
type Order int

const (
	// OrderIDAscending は, ID の昇順 (作成した順). ゼロ値.
	OrderIDAscending Order = iota
	// OrderIDDescending は, ID の降順 (新しい順).
	OrderIDDescending
)

// ListOptions は, ListItems の検索条件. 条件を追加した際に呼び出し側のコード変更を不要にするため, 引数を並べずに構造体で受け取る. ゼロ値のフィールドは検索条件に含めない.
type ListOptions struct {
	// Kind は, 返す item の種類. 空なら全ての種類を返す.
	Kind Kind
	// UpdatedSince と UpdatedBefore は, head の revision の記録時刻 (最後に head を進めた時刻) の範囲. UpdatedSince は境界値を含み, UpdatedBefore は含まない. DB はミリ秒単位で保存するので, ミリ秒より細かい部分は切り上げて比較する.
	UpdatedSince, UpdatedBefore time.Time
	Order                       Order
	// StartAfter は, 前のページの最後の item の ID. Order の並び順に従ってその次の item から返す. ゼロ値なら先頭から返す.
	StartAfter ID
	// Limit は, 返す件数の上限. 1以上.
	Limit int
}

// ItemEntry は, ListItems が返す1件. item と, その head の revision (親の一覧を含む).
type ItemEntry struct {
	Item Item
	Head Revision
}

// ItemList は, ListItems の結果.
type ItemList struct {
	Entries []ItemEntry
	// TotalSize は, StartAfter と Limit を除いた条件に合う item の数.
	TotalSize int
	// More は, Entries の後続にも条件に合致する item が存在するかどうか.
	More bool
}

// ListItems は, 条件に合う item を, head の revision と一緒に Order の順で返す. 条件が不正なら ErrInvalid を返す.
//
// 1ページ分の行, 全体の件数, 親の一覧は, 同じ時点のスナップショットから取得するため, 1つの読み取り専用のトランザクションで読み取る. ページ間では異なるスナップショットになる.
func (s *Store) ListItems(ctx context.Context, o ListOptions) (ItemList, error) {
	if err := o.validate(); err != nil {
		return ItemList{}, err
	}
	where, args := o.conditions()

	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ItemList{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var list ItemList
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM items JOIN revisions ON revisions.id = items.head"+whereClause(where), args...).Scan(&list.TotalSize); err != nil {
		return ItemList{}, err
	}

	order := "ASC"
	cmp := ">"
	if o.Order == OrderIDDescending {
		order, cmp = "DESC", "<"
	}
	if o.StartAfter != (ID{}) {
		where = append(where, "items.id "+cmp+" ?")
		args = append(args, o.StartAfter[:])
	}
	// 1件多く読み, 続きがあるかを判定する.
	args = append(args, o.Limit+1)
	list.Entries, err = scanEntries(ctx, tx, "SELECT items.id, items.kind, items.head, "+contentColumnList+
		" FROM items JOIN revisions ON revisions.id = items.head"+whereClause(where)+
		" ORDER BY items.id "+order+" LIMIT ?", args)
	if err != nil {
		return ItemList{}, err
	}
	if len(list.Entries) > o.Limit {
		list.Entries, list.More = list.Entries[:o.Limit], true
	}

	// 行を全て読み終えてから親を読む. 同じトランザクションで, 結果を読み終えていないクエリと並行して別のクエリを実行しないため.
	for i := range list.Entries {
		head := &list.Entries[i].Head
		if head.Parents, err = readParents(ctx, tx, head.ID); err != nil {
			return ItemList{}, err
		}
	}
	return list, nil
}

func (o ListOptions) validate() error {
	if o.Limit < 1 {
		return fmt.Errorf("%w: Limit が %d です (1以上である必要があります)", ErrInvalid, o.Limit)
	}
	if o.Kind != "" {
		if err := o.Kind.validate(); err != nil {
			return err
		}
	}
	switch o.Order {
	case OrderIDAscending, OrderIDDescending:
		return nil
	}
	return fmt.Errorf("%w: 並び順 %d は不明です", ErrInvalid, o.Order)
}

// conditions は, StartAfter を除いた条件を, WHERE 句の条件と引数で返す.
func (o ListOptions) conditions() ([]string, []any) {
	var (
		where []string
		args  []any
	)
	if o.Kind != "" {
		where = append(where, "items.kind = ?")
		args = append(args, string(o.Kind))
	}
	if !o.UpdatedSince.IsZero() {
		where = append(where, "revisions.created_at >= ?")
		args = append(args, ceilUnixMilli(o.UpdatedSince))
	}
	if !o.UpdatedBefore.IsZero() {
		where = append(where, "revisions.created_at < ?")
		args = append(args, ceilUnixMilli(o.UpdatedBefore))
	}
	return where, args
}

func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

// ceilUnixMilli は, t を Unix ミリ秒に切り上げる. created_at はミリ秒の整数なので, "created_at >= t" は "created_at >= 切り上げた t" と同じになる ("<" も同様).
func ceilUnixMilli(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.After(time.UnixMilli(ms)) {
		ms++
	}
	return ms
}

func scanEntries(ctx context.Context, tx *sql.Tx, query string, args []any) ([]ItemEntry, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []ItemEntry
	for rows.Next() {
		var (
			id, head []byte
			kind     string
			cols     contentColumns
		)
		if err := rows.Scan(append([]any{&id, &kind, &head}, cols.dest()...)...); err != nil {
			return nil, err
		}
		var e ItemEntry
		e.Item.Kind = Kind(kind)
		if err := scanID(id, &e.Item.ID); err != nil {
			return nil, err
		}
		if err := scanID(head, &e.Item.Head); err != nil {
			return nil, err
		}
		// head は自身の item の revision だけを指せることを, items の外部キーが保証している.
		e.Head = Revision{ID: e.Item.Head, ItemID: e.Item.ID}
		if e.Head.Content, err = cols.content(e.Head.ID); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
