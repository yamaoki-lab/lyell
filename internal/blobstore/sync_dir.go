//go:build !windows

package blobstore

import "os"

// syncDir は, ディレクトリを fsync して, その中の項目の追加や rename をディスクに届ける. ファイルを fsync しても, そのファイルを指すディレクトリの項目までは届かない.
//
// macOS の fsync は, ディスクの装置の中のキャッシュまでは押し出さない (押し出すには F_FULLFSYNC が要る). SQLite も既定では同じ扱いなので, ここでも合わせている.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
