package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// LocalStore は, Store をローカルディスク上のファイルとして実装する.
type LocalStore struct {
	root string
}

// NewLocalStore は, root の下に内容を置く LocalStore を返す.
func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

// Put は, 同じファイルシステムの上の一時的な置き場 (root/.tmp/) に data を書き, 書き終えてから rename で最終的なパスへ移す. rename は同じファイルシステムの中でだけ不可分なので, システムの一時ディレクトリ (os.TempDir) は使わない.
//
// Put が nil を返した時には, 内容とパスがディスクに届いている. 呼び出し側は, この後に DB へ記録してよい. そのために, rename の前にファイルを fsync し, rename の後に置き先のディレクトリを fsync する. 新しく作ったディレクトリも, その親を fsync する.
func (s *LocalStore) Put(_ context.Context, key string, data io.Reader) error {
	finalPath := s.path(key)
	if err := mkdirAllDurable(filepath.Dir(finalPath)); err != nil {
		return err
	}

	stagingDir := filepath.Join(s.root, ".tmp")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(stagingDir, "put-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// rename に成功していれば tmpPath は既に無いので, ここの Remove はエラーを返すだけになる (無視してよい). 途中で失敗した時は, これが後片付けになる.
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(finalPath))
}

// Open は, keyの内容を読み取るReadCloserを返す.
func (s *LocalStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(s.path(key))
}

// Exists は, keyの内容が既に存在するかを報告する.
func (s *LocalStore) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(s.path(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (s *LocalStore) path(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}

// mkdirAllDurable は, os.MkdirAll と同じく dir までのディレクトリを作る. 違いは, 新しく作ったディレクトリごとに親を fsync すること. これをしないと, 停電の後にディレクトリごと消え, その下に fsync したファイルも一緒に失われうる.
func mkdirAllDurable(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: errors.New("ディレクトリではないファイルがある")}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAllDurable(parent); err != nil {
			return err
		}
	}
	// 他の Put が同じディレクトリを同時に作っていることがあるので, 既にあるのはエラーにしない.
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDir(parent)
}
