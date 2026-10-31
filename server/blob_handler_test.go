package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yamaoki-lab/lyell/internal/blobstore"
)

// newBlobServer は, backend を保存先とする Server を Register した mux を, httptest のサーバで待ち受ける. blob の HTTP だけを試すので, DB は開かない.
func newBlobServer(t *testing.T, backend blobstore.Backend, log *slog.Logger) string {
	t.Helper()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{blobs: blobstore.New(backend), log: log}
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return hs.URL
}

func blobURL(base, data string) string {
	return base + "/lyell/api/v1alpha1/blobs/sha256/" + blobstore.Sum(sha256.Sum256([]byte(data))).String()
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, method, url, body string, header map[string]string) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest() error = %v, want nil", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v, want nil", method, url, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("応答の本文を読み取れない: %v", err)
	}
	return response{status: res.StatusCode, header: res.Header, body: string(b)}
}

func TestBlob_PutThenGet(t *testing.T) {
	base := newBlobServer(t, blobstore.NewLocalStore(t.TempDir()), nil)
	url := blobURL(base, "hello")

	if res := do(t, http.MethodPut, url, "hello", nil); res.status != http.StatusCreated {
		t.Fatalf("PUT の status = %d, want 201", res.status)
	}
	if res := do(t, http.MethodPut, url, "hello", nil); res.status != http.StatusNoContent {
		t.Errorf("2回目の PUT の status = %d, want 204", res.status)
	}

	res := do(t, http.MethodGet, url, "", nil)
	if res.status != http.StatusOK || res.body != "hello" {
		t.Fatalf("GET = %d %q, want 200 %q", res.status, res.body, "hello")
	}
	want := map[string]string{
		"Content-Type":           "application/octet-stream",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, immutable, max-age=2592000",
		"ETag":                   `"` + url[strings.LastIndex(url, "/")+1:] + `"`,
	}
	for k, v := range want {
		if got := res.header.Get(k); got != v {
			t.Errorf("GET の %s = %q, want %q", k, got, v)
		}
	}

	head := do(t, http.MethodHead, url, "", nil)
	if head.status != http.StatusOK || head.body != "" || head.header.Get("Content-Length") != "5" {
		t.Errorf("HEAD = %d %q (Content-Length %q), want 200, 本文なし, Content-Length 5", head.status, head.body, head.header.Get("Content-Length"))
	}
}

func TestBlob_PutMismatchStoresNothing(t *testing.T) {
	base := newBlobServer(t, blobstore.NewLocalStore(t.TempDir()), nil)
	url := blobURL(base, "hello")

	if res := do(t, http.MethodPut, url, "goodbye", nil); res.status != http.StatusBadRequest {
		t.Errorf("一致しない中身の PUT の status = %d, want 400", res.status)
	}
	if res := do(t, http.MethodGet, url, "", nil); res.status != http.StatusNotFound {
		t.Errorf("拒否した PUT の後の GET の status = %d, want 404", res.status)
	}
}

func TestBlob_MalformedNameIsNotFound(t *testing.T) {
	base := newBlobServer(t, blobstore.NewLocalStore(t.TempDir()), nil)
	lower := blobstore.Sum(sha256.Sum256([]byte("hello"))).String()
	prefix := base + "/lyell/api/v1alpha1/blobs/sha256/"

	// 同じ blob に対して複数の表記を許可しないので, 大文字の表記も存在しないパスとして扱う.
	for _, name := range []string{strings.ToUpper(lower), lower[:63], strings.Repeat("g", 64)} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			if res := do(t, method, prefix+name, "hello", nil); res.status != http.StatusNotFound {
				t.Errorf("%s %s の status = %d, want 404", method, name, res.status)
			}
		}
	}
	if res := do(t, http.MethodGet, prefix+lower, "", nil); res.status != http.StatusNotFound {
		t.Errorf("大文字の表記への PUT の後の GET の status = %d, want 404 (保存されていない)", res.status)
	}
}

func TestBlob_RangeAndConditionalRequests(t *testing.T) {
	base := newBlobServer(t, blobstore.NewLocalStore(t.TempDir()), nil)
	url := blobURL(base, "hello world")
	if res := do(t, http.MethodPut, url, "hello world", nil); res.status != http.StatusCreated {
		t.Fatalf("PUT の status = %d, want 201", res.status)
	}

	if res := do(t, http.MethodGet, url, "", map[string]string{"Range": "bytes=6-10"}); res.status != http.StatusPartialContent || res.body != "world" {
		t.Errorf("Range 付きの GET = %d %q, want 206 %q", res.status, res.body, "world")
	}
	etag := do(t, http.MethodHead, url, "", nil).header.Get("ETag")
	if res := do(t, http.MethodGet, url, "", map[string]string{"If-None-Match": etag}); res.status != http.StatusNotModified {
		t.Errorf("If-None-Match 付きの GET の status = %d, want 304", res.status)
	}
}

// nonSeekingBackend は, Open が io.Seeker を満たさない値を返す保存先.
type nonSeekingBackend struct {
	blobstore.Backend
}

func (b nonSeekingBackend) Open(ctx context.Context, sum blobstore.Sum) (io.ReadCloser, error) {
	f, err := b.Backend.Open(ctx, sum)
	if err != nil {
		return nil, err
	}
	return struct{ io.ReadCloser }{f}, nil
}

func TestBlob_NonSeekableBackendReturnsWholeContent(t *testing.T) {
	base := newBlobServer(t, nonSeekingBackend{blobstore.NewLocalStore(t.TempDir())}, nil)
	url := blobURL(base, "hello world")
	if res := do(t, http.MethodPut, url, "hello world", nil); res.status != http.StatusCreated {
		t.Fatalf("PUT の status = %d, want 201", res.status)
	}

	res := do(t, http.MethodGet, url, "", map[string]string{"Range": "bytes=6-10"})
	if res.status != http.StatusOK || res.body != "hello world" {
		t.Errorf("Range 付きの GET = %d %q, want 200 %q (全体)", res.status, res.body, "hello world")
	}
	if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("GET の X-Content-Type-Options = %q, want nosniff", got)
	}
	head := do(t, http.MethodHead, url, "", nil)
	if head.status != http.StatusOK || head.header.Get("Content-Length") != "11" {
		t.Errorf("HEAD = %d (Content-Length %q), want 200, Content-Length 11", head.status, head.header.Get("Content-Length"))
	}
}

func TestBlob_OtherMethodsAreNotAllowed(t *testing.T) {
	base := newBlobServer(t, blobstore.NewLocalStore(t.TempDir()), nil)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if res := do(t, method, blobURL(base, "hello"), "", nil); res.status != http.StatusMethodNotAllowed {
			t.Errorf("%s の status = %d, want 405", method, res.status)
		}
	}
}

// failingBody は, 途中まで読み取れた後にエラーを返すリクエストの本文. 接続の切断を再現する.
type failingBody struct {
	remaining []byte
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.remaining) == 0 {
		return 0, errors.New("接続が切断された")
	}
	n := copy(p, b.remaining)
	b.remaining = b.remaining[n:]
	return n, nil
}

func TestBlob_PutBodyReadErrorIsBadRequest(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{blobs: blobstore.New(blobstore.NewLocalStore(t.TempDir())), log: slog.New(slog.NewTextHandler(&logs, nil))}
	mux := http.NewServeMux()
	s.Register(mux)

	// httptest のクライアントでは本文の途中で切断できないので, mux を直接呼ぶ.
	url := blobURL("", "hello")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, &failingBody{remaining: []byte("hel")}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("本文の途中で切断された PUT の status = %d, want 400", rec.Code)
	}
	if logs.Len() != 0 {
		t.Errorf("呼び出し側の問題が内部エラーとして記録された: %s", logs.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("切断された PUT の後の GET の status = %d, want 404", rec.Code)
	}
}

// failingOpenBackend は, Open が詳細 (保存先のパス) を含むエラーを返す保存先.
type failingOpenBackend struct {
	blobstore.Backend
}

func (failingOpenBackend) Open(context.Context, blobstore.Sum) (io.ReadCloser, error) {
	return nil, &fs.PathError{Op: "open", Path: "/srv/secret/blobs", Err: fs.ErrPermission}
}

func TestBlob_InternalErrorHidesDetails(t *testing.T) {
	var logs bytes.Buffer
	base := newBlobServer(t, failingOpenBackend{blobstore.NewLocalStore(t.TempDir())}, slog.New(slog.NewTextHandler(&logs, nil)))

	res := do(t, http.MethodGet, blobURL(base, "hello"), "", nil)
	if res.status != http.StatusInternalServerError {
		t.Errorf("GET の status = %d, want 500", res.status)
	}
	if strings.Contains(res.body, "/srv/secret") {
		t.Errorf("応答の本文 %q に内部エラーの詳細が含まれる", res.body)
	}
	if !strings.Contains(logs.String(), "/srv/secret") {
		t.Errorf("ログ %q に内部エラーの詳細が記録されていない", logs.String())
	}
}
