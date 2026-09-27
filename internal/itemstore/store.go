// Package itemstore は, item と revision を SQLite に記録する (nk-a-79/hozonnyou#119). SQLite が item と revision の正式な記録で, 中身のバイト列 (blob) は blobstore に置く.
package itemstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// Store は, 1つの DB のファイルを開いたもの.
//
// SQLite は, DB のファイル全体で, 書き込みが同時に1つしかできない. 書き込み用の接続が複数あると, 取り合った時に "database is locked" で失敗しうる. 負荷がかかった時にだけ出るので, テストや1人での操作では気付けない. そこで, 書き込み用の接続は1本に絞り, 読み取り用と分ける. WAL なので, 書いている最中でも読める.
type Store struct {
	write *sql.DB // 書き込み用. 接続は1本だけ
	read  *sql.DB // 読み取り用. 書き込むとエラーになる
	blobs *blobstore.Store
}

// minSQLiteVersion は, 使う機能が揃う SQLite のバージョン. STRICT が 3.37.0 から, VACUUM INTO が 3.27.0 から.
var minSQLiteVersion = [3]int{3, 37, 0}

// Open は, path の DB を開く. 無ければ作る. schema version が古ければ, 最新の schema version まで上げる. revision の中身のバイト列 (blob) は blobs に置く.
func Open(ctx context.Context, path string, blobs *blobstore.Store) (*Store, error) {
	return open(ctx, path, blobs, migrations)
}

func open(ctx context.Context, path string, blobs *blobstore.Store, ms []migration) (*Store, error) {
	writeDSN, err := dsn(path, writeConn)
	if err != nil {
		return nil, err
	}
	write, err := sql.Open(driverName, writeDSN)
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)

	if err := checkSQLiteVersion(ctx, write); err != nil {
		write.Close()
		return nil, err
	}
	if err := migrate(ctx, write, path, ms); err != nil {
		write.Close()
		return nil, err
	}

	// 読み取り用は, schema version を上げ終えてから開く. 上げている途中の DB を読ませないため.
	readDSN, err := dsn(path, readConn)
	if err != nil {
		write.Close()
		return nil, err
	}
	read, err := sql.Open(driverName, readDSN)
	if err != nil {
		write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(runtime.GOMAXPROCS(0))
	if err := read.PingContext(ctx); err != nil {
		read.Close()
		write.Close()
		return nil, err
	}

	return &Store{write: write, read: read, blobs: blobs}, nil
}

// Close は, DB を閉じる.
func (s *Store) Close() error {
	return errors.Join(s.read.Close(), s.write.Close())
}

func checkSQLiteVersion(ctx context.Context, db *sql.DB) error {
	var v string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil { // yamaoki-lint:ignore
		return err
	}
	var got [3]int
	if _, err := fmt.Sscanf(v, "%d.%d.%d", &got[0], &got[1], &got[2]); err != nil {
		return fmt.Errorf("SQLite のバージョン %q を読めない: %w", v, err)
	}
	for i := range got {
		if got[i] != minSQLiteVersion[i] {
			if got[i] < minSQLiteVersion[i] {
				return fmt.Errorf("SQLite のバージョン %s は古い. %d.%d.%d 以上が要る", v, minSQLiteVersion[0], minSQLiteVersion[1], minSQLiteVersion[2])
			}
			break
		}
	}
	return nil
}
