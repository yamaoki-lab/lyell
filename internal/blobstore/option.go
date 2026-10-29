package blobstore

import "fmt"

// PutOption は, Put に渡す書き込みのオプション (どの保存先に書き込むか, 何か所に複製するか等). 種類は, 保存先の実装が自身のパッケージで型として定義する. blobstore は種類を1つも定義しない.
//
// 実装ごとのオプションは, 実装を生成する箇所 (起動時の初期化) で一緒に組み立て, 呼び出し側には組み立て済みのものを渡す. そうすれば, 実装とオプションの取り違えの多くはコンパイル時に検出できる. 利用者が画面や設定ファイルで選ぶ値のように実行時にしか分からないものは, CheckPutOptions で最初の書き込みより前に検証する.
type PutOption interface {
	// Required は, このオプションをサポートしない実装が Put を拒否すべきかを返す. false なら任意のオプションとして扱い, 未知の実装は無視してよい. true なら, サポートしない実装は *UnsupportedOptionError で拒否する.
	Required() bool
}

// UnsupportedOptionError は, 保存先の実装が書き込みのオプションをサポートしない時のエラー. 未知の種類の必須のオプションのほか, 既知の種類でも値を受け付けられない時 (存在しない保存先の名前等) にも使用する.
type UnsupportedOptionError struct {
	Option PutOption
	Reason string
}

func (e *UnsupportedOptionError) Error() string {
	return fmt.Sprintf("書き込みのオプション %T (%v) をサポートしていない: %s", e.Option, e.Option, e.Reason)
}
