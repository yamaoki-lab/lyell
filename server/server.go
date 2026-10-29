// Package server は, Lyell の API (lyell.api.v1alpha1) を http.Handler として提供する.
//
// Lyell は単独のサーバではなく, 製品のバックエンドに組み込む部品である. 待ち受け (http.Server, TLS) は組み込む側が持ち, Lyell は Register で ServeMux に API のパスを登録することのみを担う. 登録するパスは全て /lyell で始まるので, 製品自身のパスとは重ならない.
//
//	srv, err := server.Open(ctx, server.Config{DBPath: "lyell.db", BlobDir: "blobs"})
//	if err != nil {
//		return err
//	}
//	defer srv.Close()
//	mux := http.NewServeMux()
//	srv.Register(mux)
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1/apiv1alpha1connect"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

// Config は, Open に渡す設定. フィールドを追加した時に呼び出し側のコードの変更を不要にするため, 引数を並べずに構造体で受け取る. 指定しなかったフィールドはゼロ値になる.
type Config struct {
	// DBPath は, item と revision を記録する SQLite の DB のファイル. 存在しなければ作成する.
	DBPath string
	// BlobDir は, blob を保存するディレクトリ.
	BlobDir string
	// Logger は, 後から調べられるよう重要な出来事 (schema version を上げた, 内部エラー等) を出力する先. nil なら出力しない.
	Logger *slog.Logger
}

// Server は, 開いた DB と blob の保存先を持ち, API を提供する.
type Server struct {
	items *itemstore.Store
	blobs *blobstore.Store
	log   *slog.Logger
}

// Open は, DB と blob の保存先を開く. DB の schema version が古ければ, 最新の schema version まで上げる.
func Open(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.BlobDir == "" {
		return nil, errors.New("blob の保存先 (BlobDir) が指定されていない")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	blobs := blobstore.New(blobstore.NewLocalStore(cfg.BlobDir))
	items, err := itemstore.Open(ctx, itemstore.Config{Path: cfg.DBPath, Blobs: blobs, Logger: log})
	if err != nil {
		return nil, err
	}
	return &Server{items: items, blobs: blobs, log: log}, nil
}

// Register は, API のパスを mux に登録する.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle(apiv1alpha1connect.NewItemServiceHandler(&itemService{items: s.items, log: s.log}))
}

// Close は, DB を閉じる. 処理中のリクエストが全て終了してから (http.Server.Shutdown の後で) 呼ぶ.
func (s *Server) Close() error {
	return s.items.Close()
}

// anonymous は, 認証を経ない経路からの記録の author.
const anonymous = "anonymous"

// author は, リクエストの context から, 記録する利用者とマシンを決める. author を決める処理はここだけにし, 認証を実装する時はここを差し替える. 現在は認証が無いので, 常に anonymous を返す.
func author(context.Context) (user, machine string) {
	return anonymous, anonymous
}
