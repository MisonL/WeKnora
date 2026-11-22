package client

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
)

// countingWriter measures a body without keeping it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// uploadBody frames a multipart upload whose content is streamed as the request
// is sent, instead of being assembled in memory first.
//
// The body used to be built in a bytes.Buffer before the request existed, so a
// 50 MiB document cost 50 MiB of heap on top of whatever the filesystem had
// cached, and nothing reached the network until the last byte had been read.
// Framing it into a pipe keeps that copy out of the heap: the source is read
// once, while the request is in flight.
//
// Content-Length is still sent, and that is not cosmetic. The server sizes its
// upload admission from the declared body length, so a chunked body would read
// there as "length unknown" and be charged as a maximum-size upload on every
// request.
//
// The length is measured rather than guessed: a pre-pass writes the framing to
// a counting writer with the content part left empty, and adds the source's own
// length. Both passes share one boundary, so the advertised length is the exact
// one the sending pass produces. A source that grows or shrinks between the two
// passes desynchronises that header, and net/http rejects such a body rather
// than sending it short.
type uploadBody struct {
	boundary string
	partName string
	fileName string
	fields   [][2]string
	size     int64
	// newSource yields the content for one pass. It is called once per send and
	// again for every replay, so a retried request re-reads from the start.
	newSource func() (io.ReadCloser, error)
}

// newFileUploadBody measures the framed body of a file on disk. A blank
// fileName uses the file's own base name. fields are written after the file
// part, in the order given, and must be the same fields the request is meant to
// carry -- each one contributes bytes to the length.
func newFileUploadBody(source, partName, fileName string, fields [][2]string) (*uploadBody, error) {
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	_ = file.Close()
	if fileName == "" {
		fileName = info.Name()
	}
	return newUploadBody(info.Size(), partName, fileName, fields,
		func() (io.ReadCloser, error) { return os.Open(source) })
}

// newBytesUploadBody frames content the caller already holds. The pipe still
// avoids a second copy of the archive; it does not make the caller's slice
// disappear.
func newBytesUploadBody(content []byte, partName, fileName string, fields [][2]string) (*uploadBody, error) {
	return newUploadBody(int64(len(content)), partName, fileName, fields,
		func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(content)), nil
		})
}

func newUploadBody(contentSize int64, partName, fileName string, fields [][2]string,
	newSource func() (io.ReadCloser, error),
) (*uploadBody, error) {
	// One boundary for the measuring and the sending pass. A second random
	// boundary would be a different length and shift the declared total.
	boundary := multipart.NewWriter(io.Discard).Boundary()
	b := &uploadBody{
		boundary:  boundary,
		partName:  partName,
		fileName:  fileName,
		fields:    fields,
		newSource: newSource,
	}
	counted := &countingWriter{}
	if err := b.writeForm(counted, nil); err != nil {
		return nil, err
	}
	b.size = counted.n + contentSize
	return b, nil
}

// writeForm writes the whole multipart form to w. A nil content writes the
// content part empty, which is how the measuring pass avoids reading the source.
func (b *uploadBody) writeForm(w io.Writer, content io.Reader) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(b.boundary); err != nil {
		return err
	}
	part, err := mw.CreateFormFile(b.partName, b.fileName)
	if err != nil {
		return err
	}
	if content != nil {
		if _, err := io.Copy(part, content); err != nil {
			return err
		}
	}
	for _, field := range b.fields {
		if err := mw.WriteField(field[0], field[1]); err != nil {
			return err
		}
	}
	return mw.Close()
}

// open returns a reader that streams the form. The source is opened here rather
// than inside the goroutine so an unreadable source is reported to the caller
// before a request is built; the goroutine owns the handle and closes it once
// the body has been consumed, or once the transport closes the reader, which is
// what keeps an aborted request from leaking either the descriptor or the
// goroutine.
func (b *uploadBody) open() (io.ReadCloser, error) {
	source, err := b.newSource()
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = source.Close() }()
		_ = writer.CloseWithError(b.writeForm(writer, source))
	}()
	return reader, nil
}

// contentType is the header the boundary in this body requires.
func (b *uploadBody) contentType() string {
	return "multipart/form-data; boundary=" + b.boundary
}
