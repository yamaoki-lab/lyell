// Package blobstore は, バイト列の実体 (blob) が実際にどこにあるかを, 呼び出し側から隠す. 今はローカルディスクだけだが, HTTP 越しのファイルサーバや, 複数台への複製にも差し替えられるようにしてある. 中身が何のバイト列かは扱わず, key という不透明な文字列に対する読み書きだけを知っている.
package blobstore

import (
	"context"
	"io"
)

// Store は, key で指定した内容の読み書きを抽象化する.
type Store interface {
	// Put は key の内容を書き込む. 途中でクラッシュしても, key の内容が中途半端な状態で見えることは無い (実装が保証する).
	Put(ctx context.Context, key string, data io.Reader) error
	// Open は key の内容を読み取る ReadCloser を返す.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists は key の内容が既に存在するかを報告する.
	Exists(ctx context.Context, key string) (bool, error)
}
