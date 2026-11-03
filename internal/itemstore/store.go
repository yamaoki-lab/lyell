// Package itemstore は, item と revision を SQLite に記録する (nk-a-79/hozonnyou#119). SQLite が item と revision の正本であり, 中身のバイト列 (blob) は blobstore に格納する.
package itemstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// Store は, 1つの DB ファイルを開いた状態を保持する構造体.
//
// SQLite は, DB ファイル全体で単一の書き込みしか同時に実行できない. 書き込み用の接続が複数存在すると, 競合時に "database is locked" で失敗する可能性がある. 高負荷時のみ発生するため, テストや単一ユーザの操作では検知しにくい. そこで, 書き込み用の接続は1本に制限し, 読み取り用と分離する. WAL モードのため, 書き込み処理中であっても読み取りが可能である.
type Store struct {
	write *sql.DB // 書き込み用. 接続は1本のみ
	read  *sql.DB // 読み取り用. 書き込み処理はエラーとなる
	blobs *blobstore.Store
	log   *slog.Logger
}

// Config は, Open に渡す設定. 設定項目の追加時に呼び出し側のコード変更を不要にするため, 引数を個別に並べず構造体として受け取る. 未指定の項目はゼロ値となる.
type Config struct {
	// Path は DB ファイルのパス. 存在しない場合は新規作成する.
	Path string
	// Blobs は, revision の中身のバイト列 (blob) の格納先.
	Blobs *blobstore.Store
	// Logger は, 後から追跡できるよう重要なイベント (schema version の更新等) を出力する先. 出力先は呼び出し側が決定する. nil の場合は出力しない.
	Logger *slog.Logger
}

// minSQLiteVersion は, 必要な機能が揃う SQLite のバージョン. STRICT は 3.37.0 以降, VACUUM INTO は 3.27.0 以降で利用可能.
var minSQLiteVersion = [3]int{3, 37, 0}

// readConnMaxIdleTime は, 読み取り用の接続をアイドル状態で保持する時間の上限. 接続はそれぞれページキャッシュ (デフォルトで最大約 2 MB) を持つため, 読み取りが停止した後は閉じて解放する. 接続を新たに開くコストは1回あたり約 0.09 ms なので, 短い時間でよい.
const readConnMaxIdleTime = 10 * time.Second

// Open は, DB を開く. schema version が古い場合は, 最新の schema version まで更新する.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	return open(ctx, cfg, migrations)
}

func open(ctx context.Context, cfg Config, ms []migration) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("DB ファイル (Path) が指定されていません")
	}
	if cfg.Blobs == nil {
		return nil, errors.New("blob の格納先 (Blobs) が指定されていません")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	path := cfg.Path

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
		_ = write.Close()
		return nil, err
	}
	if err := migrate(ctx, write, path, ms, log); err != nil {
		_ = write.Close()
		return nil, err
	}

	// 読み取り用の接続は, schema version の更新が完了した後に開く. 更新途中の DB に対する読み取りを防ぐため.
	readDSN, err := dsn(path, readConn)
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	read, err := sql.Open(driverName, readDSN)
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	// database/sql は, デフォルトではアイドル状態の接続を2つまでしか保持しない. 3つ以上を並列に読み取ると, 返却された接続が閉じられ, 次の読み取りで新たに開かれるため, 上限まで保持する.
	readConns := runtime.GOMAXPROCS(0)
	read.SetMaxOpenConns(readConns)
	read.SetMaxIdleConns(readConns)
	read.SetConnMaxIdleTime(readConnMaxIdleTime)
	if err := read.PingContext(ctx); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}

	return &Store{write: write, read: read, blobs: cfg.Blobs, log: log}, nil
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
		return fmt.Errorf("SQLite のバージョン %q を解析できません: %w", v, err)
	}
	for i := range got {
		if got[i] != minSQLiteVersion[i] {
			if got[i] < minSQLiteVersion[i] {
				return fmt.Errorf("SQLite のバージョン %s は古すぎます. %d.%d.%d 以上が必要です", v, minSQLiteVersion[0], minSQLiteVersion[1], minSQLiteVersion[2])
			}
			break
		}
	}
	return nil
}
