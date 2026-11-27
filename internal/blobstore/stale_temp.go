package blobstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleTempAge は, RemoveStaleTemp が消す一時ファイルの古さ (更新時刻からの経過時間). 書いている途中の一時ファイルは書くたびに更新時刻が進むので, 転送が止まったまま待つ時間より十分に長くする. 設定にはしない.
const staleTempAge = 24 * time.Hour

// RemoveStaleTemp は, 落ちた Put が残した書きかけの一時ファイル (root/.tmp/put-*) のうち, 更新時刻が staleTempAge より古いものを消す. 起動時に1回呼ぶ. 動き続ける間の定期的な掃除はしない.
//
// 全部でなく古いものだけを消すのは, 同じ保存先を使う別のプロセスが書いている途中の一時ファイルを消さないため. それでも staleTempAge より長く止まったままの Put の一時ファイルは消すので, その Put は置く時に失敗する. 一時ファイルの他に置いたもの (put- で始まらないもの, ディレクトリ) は消さない. .tmp が無ければ何もしない. 消せないものがあっても残りは消し続け, 最初のエラーを返す.
//
// POSIX では, 置いた後に名前を消す前に落ちた一時ファイルは, 置いた blob とハードリンクで同じファイルになっているが, 消すのは名前だけなので blob は残る.
func (s *LocalStore) RemoveStaleTemp() error {
	return removeStaleTemp(filepath.Join(s.root, ".tmp"), time.Now().Add(-staleTempAge))
}

// removeStaleTemp は, dir の下の put-* のうち, 更新時刻が before より前のものを消す.
func removeStaleTemp(dir string, before time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "put-") {
			continue
		}
		info, err := e.Info()
		if err == nil {
			if !info.ModTime().Before(before) {
				continue
			}
			err = removeTemp(filepath.Join(dir, e.Name()))
		}
		// 読む間に別のプロセスが置き終えて消したものは, 消せなくてよい.
		if err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}
