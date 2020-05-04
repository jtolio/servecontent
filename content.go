package servecontent

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"
)

type Content struct {
	Name        string
	ContentType string
	ETag        string
	ModTime     time.Time
	Size        func(ctx context.Context) (size int64, err error)
	Range       func(ctx context.Context, offset, length int64) (io.ReadCloser, error)
}

func ServeReadSeeker(ctx context.Context, w http.ResponseWriter, r *http.Request, name string, modtime time.Time, fh io.ReadSeeker) {
	ServeContent(ctx, w, r, Content{
		Name:        name,
		ModTime:     modtime,
		ETag:        w.Header().Get("Etag"),
		ContentType: w.Header().Get("Content-Type"),
		Size: func(ctx context.Context) (size int64, err error) {
			size, err = fh.Seek(0, io.SeekEnd)
			if err != nil {
				return 0, fmt.Errorf("seeker can't seek: %w", err)
			}
			if _, err = fh.Seek(0, io.SeekStart); err != nil {
				return 0, fmt.Errorf("seeker can't seek: %w", err)
			}
			return size, nil
		},
		Range: func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			_, err := fh.Seek(offset, io.SeekStart)
			if err != nil {
				return nil, fmt.Errorf("seeker can't seek: %w", err)
			}
			return ioutil.NopCloser(io.LimitReader(fh, length)), nil
		},
	})
}
