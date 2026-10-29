package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// LocalStore は, Backend をローカルディスク上のファイルとして実装する.
type LocalStore struct {
	root string
}

// LocalStore が Backend を満たさなくなったら, ここでコンパイルエラーになる.
var _ Backend = (*LocalStore)(nil)

// NewLocalStore は, root の下に blob を保存する LocalStore を返す.
func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

// Put は, 同じファイルシステム上の一時ディレクトリ (root/.tmp/) に data を書き込み, 書き込みを終えてから rename で最終的なパスへ移動する. rename は同じファイルシステム内でのみアトミックなので, システムの一時ディレクトリ (os.TempDir) は使用しない.
//
// Put が nil を返した時点で, データの内容とパスはディスクに永続化されている. 呼び出し側は, その後に DB へ記録してよい. そのために, rename の前にファイルを fsync し, rename の後に配置先のディレクトリを fsync する. 新しく作成したディレクトリも, その親ディレクトリを fsync する.
//
// blob は読み取り専用 (0444) で作成する. 名前は中身の SHA-256 なので, 作成後に中身が書き換えられると, 名前との不一致を検知できなくなる. 書き込みを終えたら fsync の前に, 一時ファイルの段階でパーミッションを 0444 に変更するので, 書き込み可能な blob が最終的なパスに存在することは無い. Windows では読み取り専用の属性が付く.
//
// LocalStore は保存先が1つであり, 書き込みのオプションを一切サポートしないため, opts は使用しない.
func (s *LocalStore) Put(_ context.Context, sum Sum, data io.Reader, _ ...PutOption) error {
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
	// rename に成功していれば tmpPath は既に存在しないので, ここの Remove は単にエラーを返すのみとなる (無視して差し支えない). 途中で失敗した場合は, ここで一時ファイルを削除する.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o444); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

// CheckPutOptions は, 必須のオプションがあれば拒否し, 任意のオプションは無視する. LocalStore は書き込みのオプションを一切サポートしないため.
func (s *LocalStore) CheckPutOptions(opts ...PutOption) error {
	for _, opt := range opts {
		if opt.Required() {
			return &UnsupportedOptionError{Option: opt, Reason: "LocalStore は書き込みのオプションをサポートしていない"}
		}
	}
	return nil
}

// Open は, sum の blob を読み取る ReadCloser を返す.
func (s *LocalStore) Open(_ context.Context, sum Sum) (io.ReadCloser, error) {
	return os.Open(s.path(sum))
}

// Size は, sum の blob のバイト数を返す. 存在しなければ fs.ErrNotExist をラップしたエラーを返す.
func (s *LocalStore) Size(_ context.Context, sum Sum) (int64, error) {
	fi, err := os.Stat(s.path(sum))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// path は, sum の blob のパスを root/sha256/<1〜2文字目>/<3〜4文字目>/<64文字全体> にする (Git LFS と同じ構成). 2文字ずつ2階層に分けるので, blob が約6700万件になるまで, 1つのディレクトリのエントリは1024件以下に収まる. ファイラやターミナルでの閲覧時に, 1つのディレクトリ内の表示件数が過大になるのを防ぐため. ファイル名をハッシュ全体とすることで, 表示されたファイル名をそのまま blob 名として利用可能にしている. "sha256" の階層は, 将来ハッシュアルゴリズムを変更した時に共存させられるようにするため.
func (s *LocalStore) path(sum Sum) string {
	h := sum.String()
	return filepath.Join(s.root, "sha256", h[0:2], h[2:4], h)
}

// mkdirAllDurable は, os.MkdirAll と同じく dir までのディレクトリを作成する. 違いは, 新しく作成したディレクトリごとに親ディレクトリを fsync すること. これを行わないと, 停電発生時にディレクトリごと消失し, その配下で fsync したファイルも失われる可能性がある.
func mkdirAllDurable(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: errors.New("ディレクトリではないファイルが存在する")}
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
	// 他の Put が同じディレクトリを同時に作成していることがあるので, 既に存在する場合はエラーにしない.
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDir(parent)
}
