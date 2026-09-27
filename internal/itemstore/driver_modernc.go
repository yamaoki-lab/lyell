package itemstore

// ドライバに依存する部分 (ドライバの名前と接続文字列の組み立て) は, このファイルに閉じ込める. 別のドライバ (cgo を使う github.com/mattn/go-sqlite3 等) を足す時は, 同じ名前の定数と関数を持つファイルを足し, ビルドタグで選ぶ. 今はドライバが1つだけなので, ビルドタグは付けていない (//go:build !cgo を付けると, cgo が有効な環境でドライバの無いビルドになる).

import (
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

// connMode は, 接続を書き込みに使うか, 読み取りだけに使うか.
type connMode int

const (
	writeConn connMode = iota
	readConn
)

// dsn は, path の DB への接続文字列を返す. PRAGMA は接続ごとの設定なので, 接続文字列に入れて, database/sql が裏で作る全ての接続に効かせる. 最初の接続で1回だけ PRAGMA を実行すると, 2本目以降の接続では設定が抜ける.
func dsn(path string, mode connMode) (string, error) {
	// このドライバは, 最初の "?" より後ろを接続の設定として読む.
	if strings.Contains(path, "?") {
		return "", fmt.Errorf("DB のパスに \"?\" は使えない: %s", path)
	}

	q := url.Values{}
	// 他の接続が書いている間, すぐに失敗せず, この時間 (ミリ秒) まで待つ.
	q.Set("_busy_timeout", "5000")
	// 外部キーの検査は, 既定で無効で, 接続ごとの設定.
	q.Set("_foreign_keys", "1")
	switch mode {
	case writeConn:
		// WAL はファイルに残る設定で, 書き込みの最中でも他の接続が読める.
		q.Set("_journal_mode", "WAL")
		// WAL で NORMAL にすると, 停電の時に直前のコミットが消えうる.
		q.Set("_synchronous", "FULL")
		// トランザクションを, 最初から書き込みのロックを取って始める (BEGIN IMMEDIATE). 読んでから書く途中でロックを取りに行くと, 他の接続と取り合いになった時に, 待たずに失敗する.
		q.Set("_txlock", "immediate")
	case readConn:
		// 誤って書き込んだらエラーにする.
		q.Set("_query_only", "1")
	}
	return path + "?" + q.Encode(), nil
}
