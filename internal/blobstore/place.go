//go:build !windows

package blobstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// renameLocks は, ハードリンクを使えない時に, 存在の確認と rename の間だけ持つ sum ごとのロック. 同じ保存先を別の LocalStore で開いても効くよう, プロセスで1つにする. 保存先が異なっても sum が同じなら待つが, 持つのは置く一瞬だけなので, 実害は無い.
var renameLocks sumLocks

// linkFile は os.Link. テストで, ハードリンクを使えないファイルシステムを再現するために差し替える.
var linkFile = os.Link

// placeTemp は, 書き終えた一時ファイル tmp を final に置き, 新規に置いたかを返す. final が既にあれば, 置き換えずに false を返す. tmp の名前は残るので, 呼び出し側が消す (ハードリンクで置いた時に消すのは名前だけで, 置いた blob は残る).
//
// ハードリンクの作成は, final が既にあれば EEXIST で失敗するので, 存在の確認と配置が1つの操作になる. そのため, 保存先を共有する別のプロセスとの間でも, true を返すのは1つだけになる.
//
// 保存先がハードリンクを使えないファイルシステム (exFAT, FAT32 等) にある時は, エラーにせず, 存在を確認してから rename で置く. リンクを使えないことは, link(2) が EPERM, ENOTSUP (EOPNOTSUPP), ENOSYS を返すことで見分ける. 確認と rename の間 (置く一瞬) だけ sum のロックを持つので, 同じプロセスの Put 同士では true を返すのは1つだけになる. ただし, 別のプロセスとの間では保証が無く, 両方が true を返し, 後の rename が先の blob を (同じ中身で) 置き換えることがある. そのため, こうしたファイルシステムに保存先を置くことは推奨しない.
func (s *LocalStore) placeTemp(ctx context.Context, sum Sum, tmp, final string) (bool, error) {
	err := linkFile(tmp, final)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	case !linkUnsupported(err):
		return false, err
	}

	unlock, err := renameLocks.lock(ctx, sum)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Lstat(final); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return false, err
	}
	return true, nil
}

// linkUnsupported は, link(2) のエラーが, ファイルシステムがハードリンクを使えないことを表すかを返す.
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// removeTemp は, 一時ファイルを削除する. POSIX では読み取り専用のファイルも削除できる. ハードリンクで置いた blob と同じファイルなので, パーミッションは変えない.
func removeTemp(path string) error {
	return os.Remove(path)
}
