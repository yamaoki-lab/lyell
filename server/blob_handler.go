package server

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// blobPath は, blob のバイト列を転送するパス. {hex} は, 中身の SHA-256 を小文字の16進数64文字で表記したもの.
const blobPath = "/lyell/api/v1alpha1/blobs/sha256/{hex}"

// blobCacheControl は, GET と HEAD の応答の Cache-Control. 中身は名前の SHA-256 から変わらないので immutable を付与する. private は, 認証を実装した後に, 共有のキャッシュ (プロキシ等) に保存されるのを防ぐため.
const blobCacheControl = "private, immutable, max-age=2592000"

// blobHandler は, blob のバイト列を HTTP で転送する. blob はサイズが大きくなる可能性があるため, RPC のメッセージには含めず, この HTTP のエンドポイントで転送する.
type blobHandler struct {
	blobs *blobstore.Store
	log   *slog.Logger
}

// get は, GET と HEAD に応答する. Content-Type は application/octet-stream に固定し, nosniff を付与する. アップロードされた HTML 等が, 組み込む製品と同じオリジンで実行されるのを防ぐため.
func (h *blobHandler) get(w http.ResponseWriter, r *http.Request) {
	sum, err := parseSum(r.PathValue("hex"))
	if err != nil {
		// blob の名前として存在しないパスなので, 400 ではなく 404 を返す.
		http.NotFound(w, r)
		return
	}
	f, err := h.blobs.Open(r.Context(), sum)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()

	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", blobCacheControl)
	hdr.Set("ETag", `"`+sum.String()+`"`)

	if rs, ok := f.(io.ReadSeeker); ok {
		// ServeContent は, 設定済みの ETag を使って If-None-Match と If-Range を処理し, Range に対応する.
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	// Seek できない保存先では, Range をサポートせず, 全体を返す.
	size, err := h.blobs.Size(r.Context(), sum)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	if r.Method == http.MethodHead {
		return
	}
	// 送信の途中のエラーは, 応答のステータスを送信済みなので, 呼び出し側に伝える方法が無い.
	_, _ = io.Copy(w, f)
}

// put は, リクエストの本文を blob として保存する. 新規作成時は 201, 既存の場合は 204 を返す. 本文の SHA-256 がパスと一致しなければ, 何も保存せずに 400 を返す.
func (h *blobHandler) put(w http.ResponseWriter, r *http.Request) {
	sum, err := parseSum(r.PathValue("hex"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := &bodyReader{r: r.Body}
	created, err := h.blobs.Put(r.Context(), sum, body)
	var mismatch *blobstore.MismatchError
	switch {
	case errors.As(err, &mismatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case body.err != nil:
		// 接続の切断等で, 本文を最後まで受信できなかった. 保存先の故障ではないので, 内部エラーにしない.
		http.Error(w, "リクエストの本文を読み取れない", http.StatusBadRequest)
		return
	case err != nil && r.Context().Err() != nil:
		// 呼び出し側が切断したので, 応答を受け取る相手がいない.
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// internalError は, 詳細 (保存先のパス等) をログにだけ出力し, 呼び出し側には返さない.
func (h *blobHandler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "blob の HTTP の処理中の内部エラー", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "内部エラー", http.StatusInternalServerError)
}

// bodyReader は, リクエストの本文の読み取りのエラーを記録する. 保存先の書き込みのエラーと区別するため.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}
