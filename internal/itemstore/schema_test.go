package itemstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"testing"
	"testing/fstest"
)

// releasedSchemaFiles は, 公開した SQL のファイルの SHA-256. 公開したファイルを変更すると, マイグレーション済みの DB と新しく作成した DB でスキーマが食い違うので, 変更をこのテストで検知する. 新しいファイルを公開する時に, ここへ追加する.
//
// 公開とは, 最初のタグ (v0.1.0) を打つこと. それより前に変更する時は, 意図した変更であることを示すため, ここの値も更新する.
var releasedSchemaFiles = map[string]string{
	"schema/0001.sql": "06990940c00d058483788ae29a807bd50a643b083a66f09f862b28c6c2a71431",
}

func TestSchemaFiles_ReleasedAreUnchanged(t *testing.T) {
	for name, want := range releasedSchemaFiles {
		b, err := fs.ReadFile(schemaFiles, name)
		if err != nil {
			t.Errorf("%s を読み取れない: %v", name, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("公開した %s が変更されている (SHA-256 %s, want %s). 変更は次の番号のファイルで行う", name, got, want)
		}
	}
}

func TestLoadMigrations_FollowsFileNumbers(t *testing.T) {
	noop := func(context.Context, *sql.Tx) error { return nil }
	tests := []struct {
		name    string
		files   fstest.MapFS
		goFuncs map[int]migration
		want    int // 0 ならエラーを期待する
	}{
		{name: "連番", files: fstest.MapFS{"schema/0001.sql": {}, "schema/0002.sql": {}}, want: 2},
		{name: "番号が欠けている", files: fstest.MapFS{"schema/0001.sql": {}, "schema/0003.sql": {}}},
		{name: "1から始まらない", files: fstest.MapFS{"schema/0002.sql": {}}},
		{name: "桁数が異なる", files: fstest.MapFS{"schema/1.sql": {}}},
		{name: "Go の関数に対応する SQL のファイルが無い", files: fstest.MapFS{"schema/0001.sql": {}}, goFuncs: map[int]migration{2: noop}},
		{name: "Go の関数と SQL のファイルが対応している", files: fstest.MapFS{"schema/0001.sql": {}}, goFuncs: map[int]migration{1: noop}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := loadMigrations(tt.files, tt.goFuncs)
			if tt.want == 0 {
				if err == nil {
					t.Errorf("loadMigrations() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadMigrations() error = %v, want nil", err)
			}
			if len(ms) != tt.want {
				t.Errorf("schema version の数 = %d, want %d", len(ms), tt.want)
			}
		})
	}
}
