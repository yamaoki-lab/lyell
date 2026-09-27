package itemstore

import (
	"context"
	"database/sql"
	"fmt"
)

// CheckReport は, Check が見つけた問題. どちらも空なら問題は無い.
type CheckReport struct {
	// Integrity は, SQLite 自身の検査 (PRAGMA integrity_check) が見つけたファイルの壊れ. ページの破損や, 表と索引の食い違い等.
	Integrity []string
	// ForeignKeys は, 外部キーの違反 (無い行を指している行). 外部キーの検査が無効な接続から書かれた時に起こる. その接続ではエラーにならないので, ここで初めて気付ける.
	ForeignKeys []ForeignKeyViolation
}

// ForeignKeyViolation は, 外部キーの違反の1件.
type ForeignKeyViolation struct {
	Table  string // 違反している行の表
	Parent string // 指している先の表
}

// OK は, 問題が無いかを返す.
func (r CheckReport) OK() bool {
	return len(r.Integrity) == 0 && len(r.ForeignKeys) == 0
}

// Check は, DB が壊れていないかを確かめる. 壊れは書いた時にはエラーにならず, 後から読んだ時に初めて表に出ることが多いので, 定期的に呼ぶ (定期的に呼ぶ仕組みは, 依存関係を持てる非同期処理のキュー nk-a-79/hozonnyou#134 ができてから).
//
// blob のバイト列が手元にあるかは見ない. 転送から外した blob は手元に無いのが正しく, 壊れではないため.
//
// 問題が見つかっても error は返さず, CheckReport に入れる. error は, 検査そのものができなかった時だけ返す. 結果はログにも残す.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	// 同じ時点の DB を検査するため, 1つの接続で続けて行う. どちらの検査も読むだけなので, 読み取り用の接続でよい.
	conn, err := s.read.Conn(ctx)
	if err != nil {
		return CheckReport{}, err
	}
	defer conn.Close()

	var r CheckReport
	if r.Integrity, err = integrityCheck(ctx, conn); err != nil {
		return CheckReport{}, err
	}
	if r.ForeignKeys, err = foreignKeyCheck(ctx, conn); err != nil {
		return CheckReport{}, err
	}

	if r.OK() {
		s.log.Info("DB の検査で問題は無かった")
	} else {
		s.log.Error("DB の検査で問題が見つかった", "integrity", r.Integrity, "foreign_keys", r.ForeignKeys)
	}
	return r, nil
}

func integrityCheck(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		// 問題が無い時は "ok" の1行だけが返る.
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	return problems, rows.Err()
}

func foreignKeyCheck(ctx context.Context, conn *sql.Conn) ([]ForeignKeyViolation, error) {
	// 1行が1件の違反で, 表, 行番号, 指している先の表, 外部キーの番号が返る. WITHOUT ROWID の表では行番号が NULL になる.
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var violations []ForeignKeyViolation
	for rows.Next() {
		var (
			v     ForeignKeyViolation
			rowid sql.NullInt64
			fkid  int
		)
		if err := rows.Scan(&v.Table, &rowid, &v.Parent, &fkid); err != nil {
			return nil, fmt.Errorf("外部キーの検査の結果を読めない: %w", err)
		}
		violations = append(violations, v)
	}
	return violations, rows.Err()
}
