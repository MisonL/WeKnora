package client

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUploadBodyStreamsBeforeTheSourceIsRead is the property the streaming body
// exists for: bytes reach the wire while the source is still being read. The
// source here blocks until the test releases it, so a body that was assembled
// first could not deliver the multipart header at all -- it would wait for the
// content, which never arrives.
func TestUploadBodyStreamsBeforeTheSourceIsRead(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	body, err := newUploadBody(1024, "file", "a.bin", nil, func() (io.ReadCloser, error) {
		return io.NopCloser(&blockingReader{release: release}), nil
	})
	if err != nil {
		t.Fatalf("frame body: %v", err)
	}
	stream, err := body.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// The multipart preamble and the part headers precede the content, so they
	// must arrive while the source is still blocked.
	header := make([]byte, 64)
	done := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(stream, header)
		done <- readErr
	}()
	select {
	case readErr := <-done:
		if readErr != nil {
			t.Fatalf("read multipart header: %v", readErr)
		}
	case <-time.After(5 * time.Second):
		once.Do(func() { close(release) })
		t.Fatal("the multipart header did not arrive while the source was blocked; " +
			"the body is being buffered before it is sent")
	}
	once.Do(func() { close(release) })
	_, _ = io.Copy(io.Discard, stream)
}

// blockingReader yields the content only after release is closed.
type blockingReader struct {
	release <-chan struct{}
	served  bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.release
	if r.served {
		return 0, io.EOF
	}
	r.served = true
	return copy(p, make([]byte, len(p))), nil
}

// TestUploadBodySendsExactlyTheDeclaredLength is the invariant the declared
// Content-Length rests on: the measuring pass and the sending pass frame the
// same bytes. A file that misses a byte would desynchronise the header, and the
// server sizes its upload admission on that header.
func TestUploadBodySendsExactlyTheDeclaredLength(t *testing.T) {
	const content = "hello, streaming world"
	path := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	fields := [][2]string{{"channel", "cli"}, {"fileName", "中文名.txt"}}
	body, err := newFileUploadBody(path, "file", "", fields)
	if err != nil {
		t.Fatalf("frame body: %v", err)
	}
	stream, err := body.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	sent, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if int64(len(sent)) != body.size {
		t.Fatalf("sent %d bytes, declared %d", len(sent), body.size)
	}

	// The bytes have to be a valid multipart form carrying the file and every
	// field -- a length that matches but a form that is malformed would be a
	// worse failure, because the server would reject it after the transfer.
	_, params, err := mime.ParseMediaType(body.contentType())
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(sent), params["boundary"])
	form, err := reader.ReadForm(int64(len(sent)))
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	defer func() { _ = form.RemoveAll() }()

	files := form.File["file"]
	if len(files) != 1 {
		t.Fatalf("form carries %d file parts, want 1", len(files))
	}
	if got := files[0].Filename; got != "doc.txt" {
		t.Fatalf("file part name = %q, want the source basename", got)
	}
	opened, err := files[0].Open()
	if err != nil {
		t.Fatalf("open file part: %v", err)
	}
	defer func() { _ = opened.Close() }()
	got, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("read file part: %v", err)
	}
	if string(got) != content {
		t.Fatalf("file part = %q, want %q", got, content)
	}
	for _, field := range fields {
		if values := form.Value[field[0]]; len(values) != 1 || values[0] != field[1] {
			t.Fatalf("field %q = %v, want %q", field[0], values, field[1])
		}
	}
}

// TestUploadBodyReplaysIdentically covers the 401 path: the CLI's retry
// transport calls GetBody to resend, and a replay that differs from the first
// send would either be rejected or silently upload a different document.
func TestUploadBodyReplaysIdentically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	body, err := newFileUploadBody(path, "file", "", [][2]string{{"channel", "api"}})
	if err != nil {
		t.Fatalf("frame body: %v", err)
	}

	read := func() []byte {
		stream, openErr := body.open()
		if openErr != nil {
			t.Fatalf("open stream: %v", openErr)
		}
		defer func() { _ = stream.Close() }()
		got, readErr := io.ReadAll(stream)
		if readErr != nil {
			t.Fatalf("read body: %v", readErr)
		}
		return got
	}

	first, second := read(), read()
	if !bytes.Equal(first, second) {
		t.Fatal("the replay produced different bytes than the first send")
	}
	if int64(len(second)) != body.size {
		t.Fatalf("replay sent %d bytes, declared %d", len(second), body.size)
	}
}

// TestFileUploadBodyReportsAMissingSource keeps the failure at the call site
// instead of a failed transfer: a caller that mistyped a path should not learn
// about it from the server.
func TestFileUploadBodyReportsAMissingSource(t *testing.T) {
	_, err := newFileUploadBody(filepath.Join(t.TempDir(), "absent.pdf"), "file", "", nil)
	if err == nil {
		t.Fatal("framing a missing file succeeded")
	}
}

// TestUploadBodyKeepsTheCustomFileName pins the folder-upload case, where the
// caller supplies a name carrying a relative path that is not the file's own.
func TestUploadBodyKeepsTheCustomFileName(t *testing.T) {
	source := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(source, []byte("body"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	body, err := newFileUploadBody(source, "file", "docs/nested/doc.md", nil)
	if err != nil {
		t.Fatalf("frame body: %v", err)
	}
	stream, err := body.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	all, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(all), "filename=\"docs/nested/doc.md\"") {
		t.Fatalf("the custom file name did not reach the part header:\n%s", all)
	}
}

// TestCreateKnowledgeFromFileDeclaresTheBodyLength is the contract the
// streaming body must not break. net/http cannot infer a length from a pipe, so
// without an explicit ContentLength the request goes out chunked -- and the
// server reads a missing length as "unknown", charging every upload against its
// byte budget as though it were the largest file it accepts.
func TestCreateKnowledgeFromFileDeclaresTheBodyLength(t *testing.T) {
	srv := newMultipartEchoServer(t)
	c := NewClient(srv.URL)

	path := filepath.Join(t.TempDir(), "doc.txt")
	content := strings.Repeat("abc", 4096)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", path, nil, nil, "docs/doc.txt", "cli", nil); err != nil {
		t.Fatalf("upload: %v", err)
	}

	got := lastRecordedUpload(t, srv)
	if got.contentLength <= 0 {
		t.Fatalf("ContentLength = %d; the body went out without a declared length", got.contentLength)
	}
	if got.transferEncoding != "" {
		t.Fatalf("Transfer-Encoding = %q, want none: a chunked upload hides its size from admission control",
			got.transferEncoding)
	}
	if int64(len(got.body)) != got.contentLength {
		t.Fatalf("server received %d bytes but the request declared %d", len(got.body), got.contentLength)
	}
	if got.field["channel"] != "cli" || got.field["fileName"] != "docs/doc.txt" {
		t.Fatalf("fields did not survive the stream: %v", got.field)
	}
	if got.fileContent != content {
		t.Fatalf("file content changed in transit: %d bytes, want %d", len(got.fileContent), len(content))
	}
}

// recordedUpload is what the echo server saw for one request.
type recordedUpload struct {
	contentLength    int64
	transferEncoding string
	body             []byte
	fileContent      string
	field            map[string]string
}

type multipartEchoServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []recordedUpload
}

func newMultipartEchoServer(t *testing.T) *multipartEchoServer {
	t.Helper()
	echo := &multipartEchoServer{}
	echo.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec := recordedUpload{
			contentLength:    r.ContentLength,
			transferEncoding: strings.Join(r.TransferEncoding, ","),
			body:             body,
			field:            map[string]string{},
		}
		if _, params, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type")); parseErr == nil {
			if mr := multipart.NewReader(bytes.NewReader(body), params["boundary"]); mr != nil {
				if form, formErr := mr.ReadForm(int64(len(body))); formErr == nil {
					for key, values := range form.Value {
						if len(values) > 0 {
							rec.field[key] = values[0]
						}
					}
					if files := form.File["file"]; len(files) == 1 {
						if f, openErr := files[0].Open(); openErr == nil {
							if content, readErr := io.ReadAll(f); readErr == nil {
								rec.fileContent = string(content)
							}
							_ = f.Close()
						}
					}
					_ = form.RemoveAll()
				}
			}
		}
		echo.mu.Lock()
		echo.seen = append(echo.seen, rec)
		echo.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"doc-1","knowledge_base_id":"kb-1",` +
			`"type":"file","title":"doc.txt","parse_status":"pending"}}`))
	}))
	t.Cleanup(echo.Close)
	return echo
}

func lastRecordedUpload(t *testing.T, srv *multipartEchoServer) recordedUpload {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.seen) == 0 {
		t.Fatal("the server was never reached")
	}
	return srv.seen[len(srv.seen)-1]
}

// TestUploadAbandonedByTheServerDoesNotLeak is the risk streaming introduces: a
// server that answers before the body is finished (413, 401, a refused
// admission slot) leaves the framing goroutine blocked on a pipe nobody reads.
// net/http closes the request body when the exchange ends, and closing the pipe
// reader is what unblocks the writer -- assert the goroutine actually goes away
// instead of trusting that.
func TestUploadAbandonedByTheServerDoesNotLeak(t *testing.T) {
	const fileSize = 8 << 20
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), fileSize), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Answer immediately without reading the body, then hold the
		// connection open just long enough that a goroutine still writing
		// would be visibly stuck.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":"file too large"}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(done)
	}))
	defer server.Close()

	c := NewClient(server.URL)
	_, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", path, nil, nil, "", "", nil)
	if err == nil {
		t.Fatal("an oversized upload was reported as successful")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never answered")
	}

	// The framing goroutine must exit. Poll briefly: closing the request body
	// unblocks it, but that happens on net/http's own schedule.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if stableGoroutineCount() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a framing goroutine is still blocked after the server refused the upload "+
				"(goroutines: %d)", runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stableGoroutineCount reports whether the count has settled: two consecutive
// samples taken far enough apart to let a finishing goroutine deregister.
func stableGoroutineCount() bool {
	first := runtime.NumGoroutine()
	time.Sleep(60 * time.Millisecond)
	return runtime.NumGoroutine() <= first
}

// TestClosingTheBodyReleasesTheSource is the direct form of the leak guard
// above: closing what open returned must reach the framing goroutine, which
// then releases the file handle. Without that, an abandoned upload keeps a
// descriptor open and a goroutine parked on a pipe nobody will ever read.
func TestClosingTheBodyReleasesTheSource(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	body, err := newUploadBody(1<<20, "file", "big.bin", nil, func() (io.ReadCloser, error) {
		return &closeSignal{
			Reader:  bytes.NewReader(bytes.Repeat([]byte("z"), 1<<20)),
			onClose: func() { once.Do(func() { close(closed) }) },
		}, nil
	})
	if err != nil {
		t.Fatalf("frame body: %v", err)
	}
	stream, err := body.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// Reading a little guarantees the writer is inside the copy and therefore
	// blocked on the pipe rather than still starting up.
	if _, err := io.ReadFull(stream, make([]byte, 256)); err != nil {
		t.Fatalf("read preamble: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("closing the request body did not reach the framing goroutine; " +
			"the source stays open and the goroutine stays parked")
	}
}

// closeSignal reports when it is closed, so a test can observe release without
// inspecting goroutines.
type closeSignal struct {
	io.Reader
	onClose func()
}

func (c *closeSignal) Close() error {
	c.onClose()
	return nil
}
