package blobstore

import "fmt"

// PutOption は, Put に渡す書き込みの選択肢 (どの置き場に書くか, 何か所に複製するか等). 種類は, 置き場の実装が自分のパッケージで型として定義する. blobstore は種類を1つも決めない.
//
// 実装ごとの選択肢は, 実装を作る所 (起動時の組み立て) で一緒に組み立て, 使う側にはできあがったものを渡す. そうすれば, 実装と選択肢の取り違えの多くはコンパイル時に捕まる. 利用者が画面や設定ファイルで選ぶ値のように実行時にしか分からないものは, CheckPutOptions で最初の書き込みより前に確かめる.
type PutOption interface {
	// Required は, この選択肢を扱えない実装が Put を断るべきかを返す. false なら希望として扱い, 知らない実装は無視してよい. true なら, 扱えない実装は *UnsupportedOptionError で断る.
	Required() bool
}

// UnsupportedOptionError は, 置き場の実装が書き込みの選択肢を扱えない時のエラー. 知らない種類の必須の選択肢のほか, 知っている種類でも値を受け付けられない時 (無い置き場の名前等) にも使う.
type UnsupportedOptionError struct {
	Option PutOption
	Reason string
}

func (e *UnsupportedOptionError) Error() string {
	return fmt.Sprintf("書き込みの選択肢 %T (%v) を扱えない: %s", e.Option, e.Option, e.Reason)
}
