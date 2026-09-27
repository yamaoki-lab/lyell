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
func (s *LocalStore) Put(_ context.Context, sum Sum, data io.Reader) error {
	finalPath := s.path(sum)
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

// Open は, sum の blob を読み取る ReadCloser を返す.
func (s *LocalStore) Open(_ context.Context, sum Sum) (io.ReadCloser, error) {
	return os.Open(s.path(sum))
}

// Exists は, sum の blob が既にあるかを報告する.
func (s *LocalStore) Exists(_ context.Context, sum Sum) (bool, error) {
	_, err := os.Stat(s.path(sum))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// path は, sum の blob のパスを root/sha256/<1〜2文字目>/<3〜4文字目>/<64文字全体> にする (Git LFS と同じ形). 2文字ずつ2段に分けるので, blob が約6700万件になるまで, 1つのディレクトリの中身は1024件以下に収まる. 人がファイラやターミナルで覗いた時に, 一度に並ぶ数が多くなりすぎないようにするため. ファイル名をハッシュ全体にしているのは, 見えている名前をそのまま blob の名前として使えるようにするため. "sha256" の段は, 将来ハッシュの方式を変えた時に並べて置けるようにするため.
func (s *LocalStore) path(sum Sum) string {
	h := sum.String()
	return filepath.Join(s.root, "sha256", h[0:2], h[2:4], h)
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
