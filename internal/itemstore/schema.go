package itemstore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"time"
)

// migration は, schema version を1つ上げる. 渡されたトランザクションの中だけで変更する.
type migration func(ctx context.Context, tx *sql.Tx) error

// schemaFiles は, schema version ごとの SQL. schema/0001.sql が schema version 1 で, 番号の順に適用する. 表の決まりの理由は, 各ファイルの中にコメントで書いてある.
//
//go:embed schema/*.sql
var schemaFiles embed.FS

// goMigrations は, SQL だけでは書けない変更 (データの移し替え等) のための Go の関数. 番号は schema version で, その番号の SQL のファイルの後に, 同じトランザクションの中で実行する. Go だけで済む変更でも, 番号を取るために SQL のファイル (コメントだけでよい) を置く. 今は無い.
var goMigrations = map[int]migration{}

// migrations の i 番目 (0 始まり) は, schema version i を i+1 へ上げる. schema version は DB のファイルの user_version に入る. // yamaoki-lint:ignore
//
// 一度公開したファイルや関数は書き換えない. 既に上げ終えた DB には二度と適用されないので, 書き換えると, 新しく作った DB と上げてきた DB で形が食い違う. 変更は, 次の番号で足して行う.
var migrations = mustLoadMigrations(schemaFiles, goMigrations)

func mustLoadMigrations(files fs.FS, goFuncs map[int]migration) []migration {
	ms, err := loadMigrations(files, goFuncs)
	if err != nil {
		panic(err)
	}
	return ms
}

// loadMigrations は, schema/NNNN.sql を番号の順に並べ, 1つずつの migration にする. 番号は1からの連番でなければならない. 飛びや重なりがあると, ある schema version が黙って抜けるので, エラーにする.
func loadMigrations(files fs.FS, goFuncs map[int]migration) ([]migration, error) {
	names, err := fs.Glob(files, "schema/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var ms []migration
	for i, name := range names {
		schemaVersion := i + 1
		if want := fmt.Sprintf("schema/%04d.sql", schemaVersion); name != want {
			return nil, fmt.Errorf("schema version %d のファイルは %s のはずだが, %s がある. 番号は1からの連番にする", schemaVersion, want, name)
		}
		sqlText, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		goFunc := goFuncs[schemaVersion]
		ms = append(ms, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if goFunc != nil {
				return goFunc(ctx, tx)
			}
			return nil
		})
	}
	for schemaVersion := range goFuncs {
		if schemaVersion < 1 || schemaVersion > len(ms) {
			return nil, fmt.Errorf("schema version %d の Go の関数に, 対応する SQL のファイルが無い", schemaVersion)
		}
	}
	return ms, nil
}

// migrate は, DB を ms の最新の schema version まで上げる. schema version ごとに1つのトランザクションで適用するので, 途中で失敗しても, 失敗した schema version の変更だけが取り消される.
//
// 既に中身がある DB を上げる時は, 先に VACUUM INTO で DB を丸ごと複製する. 失敗した適用はトランザクションが取り消すが, "成功したが中身を壊した" 適用は取り消せないので, その時は複製から戻す.
func migrate(ctx context.Context, db *sql.DB, path string, ms []migration) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil { // yamaoki-lint:ignore
		return err
	}
	switch {
	case current > len(ms):
		// 古いプログラムで新しい DB を触ると, 知らない列や表を壊しうる.
		return fmt.Errorf("DB の schema version %d は, このプログラムが知る最新の schema version %d より新しい. 新しいプログラムで開くこと", current, len(ms))
	case current == len(ms):
		return nil
	case current == 0:
		// schema version が 0 なのに表があるなら, Lyell が作った DB ではない.
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%s は schema version が 0 だが, 既に %d 個の表などがある. Lyell の DB ではないかもしれない", path, n)
		}
	default:
		backup := fmt.Sprintf("%s.v%d-%s.bak", path, current, time.Now().UTC().Format("20060102T150405Z"))
		if _, err := db.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("schema version を上げる前の複製 (%s) を作れない: %w", backup, err)
		}
	}

	for v := current; v < len(ms); v++ {
		if err := applyMigration(ctx, db, ms[v], v+1); err != nil {
			return fmt.Errorf("schema version %d へ上げられない: %w", v+1, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, to int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit の後は何もしない
	if err := m(ctx, tx); err != nil {
		return err
	}
	// user_version もトランザクションの中で変わるので, 適用と schema version がずれることは無い. // yamaoki-lint:ignore
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", to)); err != nil { // yamaoki-lint:ignore
		return err
	}
	return tx.Commit()
}
