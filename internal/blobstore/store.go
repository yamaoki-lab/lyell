// Package blobstore は, バイト列の実体 (blob) が実際にどこにあるかを, 呼び出し側から隠す. 今はローカルディスクだけだが, HTTP 越しのファイルサーバや, 複数台への複製にも差し替えられるようにしてある. 中身が何のバイト列かは扱わず, 中身の SHA-256 (Sum) を名前にした読み書きだけを知っている.
//
// 置き場の実装は Backend を満たし, 使う側は New で Backend を包んだ Store を使う. 書いた中身が名前の SHA-256 と合うかは Store が必ず確かめるので, 実装を足しても確かめ忘れることが無い.
package blobstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
)

// Backend は, 置き場の実装が満たすもの. 使う側は直接呼ばずに, New で包んだ Store を使う.
type Backend interface {
	// Put は sum の blob を書き込む. data は最後 (io.EOF) まで読んでから確定し, 読み取りがエラーになったら確定しない. 途中でクラッシュしても, 中途半端な内容が見えることは無い. Store は, 中身が sum と合わない時に data の読み取りをエラーにするので, この約束を守れば合わない blob は置かれない.
	Put(ctx context.Context, sum Sum, data io.Reader) error
	// Open は sum の blob を読み取る ReadCloser を返す.
	Open(ctx context.Context, sum Sum) (io.ReadCloser, error)
	// Exists は sum の blob が既にあるかを報告する.
	Exists(ctx context.Context, sum Sum) (bool, error)
}

// Store は, Backend を包み, 書く中身が名前の SHA-256 と合うかを確かめる.
type Store struct {
	backend Backend
}

// New は, backend を包んだ Store を返す.
func New(backend Backend) *Store {
	return &Store{backend: backend}
}

// MismatchError は, 渡された中身の SHA-256 が, 名前として渡された Sum と合わない時のエラー.
type MismatchError struct {
	Want Sum // 名前として渡された値
	Got  Sum // 中身から計算した値
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("中身の SHA-256 が %s で, 名前の %s と合わない", e.Got, e.Want)
}

// Put は, data を sum の blob として書き込む. data の SHA-256 が sum と合わなければ, 何も置かずに *MismatchError を返す.
//
// sum の blob が既にある時も, 渡した側の計算を信じずに data を最後まで読んで確かめる. 確かめずに成功を返すと, 名前を取り違えた中身が保存されないまま, 保存できたと思われてしまうため. この時はディスクに書かない. 転送そのものを省きたい時は, 先に Exists で確かめる.
func (s *Store) Put(ctx context.Context, sum Sum, data io.Reader) error {
	ok, err := s.backend.Exists(ctx, sum)
	if err != nil {
		return err
	}
	v := &verifyingReader{r: data, h: sha256.New(), want: sum}
	if ok {
		_, err := io.Copy(io.Discard, v)
		return err
	}
	return s.backend.Put(ctx, sum, v)
}

// Open は, sum の blob を読み取る ReadCloser を返す.
func (s *Store) Open(ctx context.Context, sum Sum) (io.ReadCloser, error) {
	return s.backend.Open(ctx, sum)
}

// Exists は, sum の blob が既にあるかを報告する.
func (s *Store) Exists(ctx context.Context, sum Sum) (bool, error) {
	return s.backend.Exists(ctx, sum)
}

// verifyingReader は, 読んだ中身の SHA-256 を計算しながら r を読み, 最後まで読んだ時に want と合わなければ io.EOF の代わりに *MismatchError を返す.
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
