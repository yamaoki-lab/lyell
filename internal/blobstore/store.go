// Package blobstore は, バイト列の実体 (blob) の保存先を, 呼び出し側から隠蔽する. 現在の保存先はローカルディスクだけだが, HTTP 経由のファイルサーバや, 複数台への複製にも差し替えられるようにしている. 中身が何のバイト列かは扱わず, 中身の SHA-256 (Sum) を名前とした読み書きだけを提供する.
//
// 保存先の実装は Backend を満たし, 呼び出し側は New で Backend をラップした Store を使用する. 書き込んだ中身が名前の SHA-256 と一致するかは Store が必ず検証するので, 実装を追加しても検証が漏れることは無い.
package blobstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
)

// Backend は, 保存先の実装が満たすインタフェース. 呼び出し側は直接呼ばずに, New でラップした Store を使用する.
type Backend interface {
	// Put は, sum の blob を書き込む. data を最後 (io.EOF) まで読んでから確定し, 読み取りがエラーになった場合は確定しない. 途中でクラッシュしても, 不完全な内容が見えることは無い. Store は, 中身が sum と一致しない時に data の読み取りをエラーにするので, この条件を満たせば一致しない blob は保存されない.
	//
	// opts は, Store が先に CheckPutOptions で検証してから渡す.
	Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) error
	// CheckPutOptions は, 書き込まずにオプションだけを検証する. サポートしないオプションがあれば *UnsupportedOptionError を返す. 未知の種類は, 必須 (Required が true) なら拒否し, 任意なら無視する.
	CheckPutOptions(opts ...PutOption) error
	// Open は, sum の blob を読み取る ReadCloser を返す.
	Open(ctx context.Context, sum Sum) (io.ReadCloser, error)
	// Size は, sum の blob のバイト数を返す. 存在しなければ fs.ErrNotExist をラップしたエラーを返す. 存在するかだけを知りたい呼び出し側のために, Store が Exists を提供する.
	Size(ctx context.Context, sum Sum) (int64, error)
}

// Store は, Backend をラップし, 書き込む中身が名前の SHA-256 と一致するかを検証する.
type Store struct {
	backend Backend
	locks   sumLocks
}

// New は, backend をラップした Store を返す.
func New(backend Backend) *Store {
	return &Store{backend: backend}
}

// MismatchError は, 渡された中身の SHA-256 が, 名前として渡された Sum と一致しない時のエラー.
type MismatchError struct {
	Want Sum // 名前として渡された値
	Got  Sum // 中身から計算した値
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("中身の SHA-256 %s が, 名前の %s と一致しない", e.Got, e.Want)
}

// Put は, data を sum の blob として書き込み, この呼び出しで blob を新規に作成したかを返す. data の SHA-256 が sum と一致しなければ, 何も保存せずに *MismatchError を返す.
//
// sum の blob が既に存在する時も, 呼び出し側の計算を信頼せずに data を最後まで読んで検証する. 検証せずに成功を返すと, 名前と異なる中身が保存されていないのに, 保存できたと誤認されるため. この時はディスクに書き込まず, false を返す. 転送そのものを省略したい時は, 先に Exists で確認する.
//
// 同じ sum への Put は, 同時には書き込まない. 後から来た Put は先の Put の完了を待ち, その後は既に存在する時と同じく検証だけを行う. 異なる sum への Put は互いに待たない. 存在の確認をロックの中で行うので, 同じ sum への Put がほぼ同時に来ても, true を返すのは1つだけになる. 呼び出し側が Exists で確認してから Put すると, 確認がロックの外になり, この保証が無くなる.
//
// opts は, data を読む前に CheckPutOptions で検証する. サポートしないオプションがあれば, 何も読まず何も保存せずに *UnsupportedOptionError を返す. 既に存在する blob は保存し直さないので, その時 opts は使用しない (既に存在する blob の保存先を変更するのは, 別の操作として扱う).
func (s *Store) Put(ctx context.Context, sum Sum, data io.Reader, opts ...PutOption) (created bool, err error) {
	if err := s.backend.CheckPutOptions(opts...); err != nil {
		return false, err
	}
	unlock, err := s.locks.lock(ctx, sum)
	if err != nil {
		return false, err
	}
	ok, err := s.Exists(ctx, sum)
	if err != nil {
		unlock()
		return false, err
	}
	v := &verifyingReader{r: data, h: sha256.New(), want: sum}
	if ok {
		// 書き込まないので, 検証の間は他の Put を待たせない.
		unlock()
		_, err := io.Copy(io.Discard, v)
		return false, err
	}
	defer unlock()
	if err := s.backend.Put(ctx, sum, v, opts...); err != nil {
		return false, err
	}
	return true, nil
}

// CheckPutOptions は, 書き込まずにオプションだけを検証する. 設定を読み込んだ時や画面から値を受け取った時に呼べば, 最初の書き込みより前に誤りを検出できる.
func (s *Store) CheckPutOptions(opts ...PutOption) error {
	return s.backend.CheckPutOptions(opts...)
}

// Open は, sum の blob を読み取る ReadCloser を返す.
func (s *Store) Open(ctx context.Context, sum Sum) (io.ReadCloser, error) {
	return s.backend.Open(ctx, sum)
}

// Size は, sum の blob のバイト数を返す. 存在しなければ fs.ErrNotExist をラップしたエラーを返す.
func (s *Store) Size(ctx context.Context, sum Sum) (int64, error) {
	return s.backend.Size(ctx, sum)
}

// Exists は, sum の blob が既に存在するかを返す.
func (s *Store) Exists(ctx context.Context, sum Sum) (bool, error) {
	_, err := s.backend.Size(ctx, sum)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// verifyingReader は, 読み取った中身の SHA-256 を計算しながら r を読み, 最後まで読んだ時に want と一致しなければ, io.EOF の代わりに *MismatchError を返す.
type verifyingReader struct {
	r    io.Reader
	h    hash.Hash
	want Sum
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	if err == io.EOF {
		var got Sum
		copy(got[:], v.h.Sum(nil))
		if got != v.want {
			return n, &MismatchError{Want: v.want, Got: got}
		}
	}
	return n, err
}
