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
func (s *LocalStore) Put(_ context.Context, key string, data io.Reader) error {
	finalPath := s.path(key)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
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
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, finalPath)
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
