package servecontent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"path/filepath"
	"time"
)

var (
	ErrInvalidOffset = errors.New("invalid offset")
)

// RangeFunc represents a way of getting a range of data from a stream.
// If length == -1, the range from the offset to the end is requested. offset
// and length are *not* guaranteed to be within the stream's size.
// If the offset is not a valid offset, the RangeFunc should return an error
// where errors.Is(err, ErrInvalidOffset) is true. If the length runs off the
// end of the object, errors.Is(err, io.EOF) is expected whenever the stream
// actually ends. io.ErrUnexpectedEOF is reserved for other conditions.
type RangeFunc func(ctx context.Context, offset, length int64) (io.ReadCloser, error)

type Content struct {
	ContentType func(ctx context.Context) (string, error)
	ETag        string
	ModTime     time.Time
	Size        func(ctx context.Context) (size int64, err error)
	Range       RangeFunc
}

func ServeReadSeeker(ctx context.Context, w http.ResponseWriter, r *http.Request, name string, modtime time.Time, fh io.ReadSeeker) {
	var rangefn RangeFunc = func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		_, err := fh.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, fmt.Errorf("seeker can't seek: %w", err)
		}
		return ioutil.NopCloser(io.LimitReader(fh, length)), nil
	}
	ctypefn := func(ctx context.Context) (string, error) {
		if ctypes, haveType := w.Header()["Content-Type"]; haveType {
			if len(ctypes) > 0 {
				return ctypes[0], nil
			}
			return "", nil
		}
		return DetectContentType(ctx, name, rangefn)
	}
	ServeContent(ctx, w, r, Content{
		ModTime:     modtime,
		ETag:        w.Header().Get("Etag"),
		ContentType: ctypefn,
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
		Range: rangefn,
	})
}

func DetectContentType(ctx context.Context, name string, f RangeFunc) (string, error) {
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype != "" || f == nil {
		return ctype, nil
	}
	// read a chunk to decide between utf-8 text and binary
	// TODO: don't force reading this twice
	r, err := f(ctx, 0, sniffLen)
	if err != nil {
		return "", err
	}
	buf, err := ioutil.ReadAll(r)
	r.Close()
	if err != nil {
		return "", err
	}
	return http.DetectContentType(buf), nil
}
