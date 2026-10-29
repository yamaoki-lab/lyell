//go:build !windows

package blobstore

import "os"

// syncDir は, ディレクトリを fsync して, その中のエントリの追加や rename をディスクに永続化する. ファイルを fsync しても, そのファイルを指すディレクトリのエントリは永続化されない.
//
// macOS の fsync は, ディスク装置内のキャッシュまではフラッシュしない (フラッシュには F_FULLFSYNC が必要). SQLite も既定では同じ扱いなので, それに合わせている.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
