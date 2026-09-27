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

// releasedSchemaFiles は, 公開した SQL のファイルの SHA-256. 公開したファイルを書き換えると, 既に上げ終えた DB と新しく作った DB で形が食い違うので, 書き換えたらこのテストで気付けるようにする. 新しいファイルを公開する時に, ここへ足す.
var releasedSchemaFiles = map[string]string{
	"schema/0001.sql": "c8f7b5f38034441ea26bcd42a371f7df6aa6df99f9cc833159bc40a6cd4ee02d",
}

func TestSchemaFiles_ReleasedAreUnchanged(t *testing.T) {
	for name, want := range releasedSchemaFiles {
		b, err := fs.ReadFile(schemaFiles, name)
		if err != nil {
			t.Errorf("%s を読めない: %v", name, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("公開した %s が書き換えられている (SHA-256 %s, want %s). 変更は次の番号のファイルで行う", name, got, want)
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
		{name: "番号が飛んでいる", files: fstest.MapFS{"schema/0001.sql": {}, "schema/0003.sql": {}}},
		{name: "1から始まらない", files: fstest.MapFS{"schema/0002.sql": {}}},
		{name: "桁が違う", files: fstest.MapFS{"schema/1.sql": {}}},
		{name: "Go の関数に SQL のファイルが無い", files: fstest.MapFS{"schema/0001.sql": {}}, goFuncs: map[int]migration{2: noop}},
		{name: "Go の関数と SQL のファイルが揃っている", files: fstest.MapFS{"schema/0001.sql": {}}, goFuncs: map[int]migration{1: noop}, want: 1},
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
