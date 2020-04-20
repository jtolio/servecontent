package servecontent

import (
	"context"
	"io"
	"time"
)

type Content struct {
	Name     string
	MimeType string
	Etag     string
	ModTime  time.Time
	Size     int64
	Range    func(ctx context.Context, offset, length int64) (io.Reader, error)
}
