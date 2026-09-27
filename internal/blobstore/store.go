// Package blobstore は, バイト列の実体 (blob) が実際にどこにあるかを, 呼び出し側から隠す. 今はローカルディスクだけだが, HTTP 越しのファイルサーバや, 複数台への複製にも差し替えられるようにしてある. 中身が何のバイト列かは扱わず, 中身の SHA-256 (Sum) を名前にした読み書きだけを知っている.
package blobstore

import (
	"context"
	"io"
)

// Store は, Sum で指定した blob の読み書きを抽象化する.
type Store interface {
	// Put は sum の blob を書き込む. 途中でクラッシュしても, 中途半端な内容が見えることは無い (実装が保証する).
	Put(ctx context.Context, sum Sum, data io.Reader) error
	// Open は sum の blob を読み取る ReadCloser を返す.
	Open(ctx context.Context, sum Sum) (io.ReadCloser, error)
	// Exists は sum の blob が既にあるかを報告する.
	Exists(ctx context.Context, sum Sum) (bool, error)
}
