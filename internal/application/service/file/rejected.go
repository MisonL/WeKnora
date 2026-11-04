package file

import (
	"context"
	"io"
	"mime/multipart"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// rejectedFileService fails every operation with the reason it was created
// for. Resolution paths use it instead of returning nil or silently falling
// back to the process-global service when a storage configuration must not be
// honoured (for example RAM role credentials injected into tenant config).
// Failing loudly keeps the rejection visible in logs and API responses instead
// of serving data through a different identity.
type rejectedFileService struct {
	err error
}

// NewRejectedFileService returns a FileService whose every method fails with err.
func NewRejectedFileService(err error) interfaces.FileService {
	return &rejectedFileService{err: err}
}

func (s *rejectedFileService) CheckConnectivity(context.Context) error { return s.err }

func (s *rejectedFileService) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	return "", s.err
}

func (s *rejectedFileService) SaveBytes(context.Context, []byte, uint64, string, bool) (string, error) {
	return "", s.err
}

func (s *rejectedFileService) GetFile(context.Context, string) (io.ReadCloser, error) {
	return nil, s.err
}

func (s *rejectedFileService) GetFileURL(context.Context, string) (string, error) {
	return "", s.err
}

func (s *rejectedFileService) DeleteFile(context.Context, string) error { return s.err }

func (s *rejectedFileService) CopyFile(context.Context, string, uint64, string) (string, error) {
	return "", s.err
}
