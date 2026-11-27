package blobstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// placeTemp は, 書き終えた一時ファイル tmp を final に置き, 新規に置いたかを返す. final が既にあれば, 置き換えずに false を返す (tmp は残るので, 呼び出し側が消す).
//
// MoveFile は, os.Rename と異なり置き換えを許さない (MOVEFILE_REPLACE_EXISTING を付けない) ので, final が既にあれば失敗し, 存在の確認と配置が1つの操作になる. そのため, 保存先を共有する別のプロセスとの間でも, true を返すのは1つだけになり, 読み取り専用の blob を置き換えようとして失敗することも無い. ハードリンクを使わないので, FAT 系のファイルシステムでも同じ保証がある.
func (s *LocalStore) placeTemp(_ context.Context, _ Sum, tmp, final string) (bool, error) {
	from, err := syscall.UTF16PtrFromString(tmp)
	if err != nil {
		return false, err
	}
	to, err := syscall.UTF16PtrFromString(final)
	if err != nil {
		return false, err
	}
	err = syscall.MoveFile(from, to)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	return false, &os.LinkError{Op: "movefile", Old: tmp, New: final, Err: err}
}

// removeTemp は, 一時ファイルを削除する. Windows では読み取り専用の属性が付いたファイルを削除できないので, 失敗したら属性を外してから削除し直す. 一時ファイルは置いた blob と別のファイルなので, 属性を外しても blob には影響しない.
func removeTemp(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if os.Chmod(path, 0o600) != nil {
		return err
	}
	return os.Remove(path)
}
