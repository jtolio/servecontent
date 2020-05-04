// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
)

// ErrNoOverlap is returned by ParseRange if first-byte-pos of
// all of the byte-range-spec values is greater than the content size.
var ErrNoOverlap = errors.New("invalid range: failed to overlap")

// HTTPRange specifies the byte range to be sent to the client.
type HTTPRange struct {
	Start, Length int64
}

func (r HTTPRange) ContentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, r.Start+r.Length-1, size)
}

func (r HTTPRange) MIMEHeader(contentType string, size int64) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Range": {r.ContentRange(size)},
		"Content-Type":  {contentType},
	}
}

// ParseRange parses a Range header string as per RFC 7233.
// ErrNoOverlap is returned if none of the ranges overlap.
func ParseRange(s string, size int64) ([]HTTPRange, error) {
	if s == "" {
		return nil, nil // header not present
	}
	const b = "bytes="
	if !strings.HasPrefix(s, b) {
		return nil, errors.New("invalid range")
	}
	var ranges []HTTPRange
	noOverlap := false
	for _, ra := range strings.Split(s[len(b):], ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}
		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])
		var r HTTPRange
		if start == "" {
			// If no start is specified, end specifies the
			// range start relative to the end of the file.
			i, err := strconv.ParseInt(end, 10, 64)
			if err != nil {
				return nil, errors.New("invalid range")
			}
			if i > size {
				i = size
			}
			r.Start = size - i
			r.Length = size - r.Start
		} else {
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid range")
			}
			if i >= size {
				// If the range begins after the size of the content,
				// then it does not overlap.
				noOverlap = true
				continue
			}
			r.Start = i
			if end == "" {
				// If no end is specified, range extends to end of the file.
				r.Length = size - r.Start
			} else {
				i, err := strconv.ParseInt(end, 10, 64)
				if err != nil || r.Start > i {
					return nil, errors.New("invalid range")
				}
				if i >= size {
					i = size - 1
				}
				r.Length = i - r.Start + 1
			}
		}
		ranges = append(ranges, r)
	}
	if noOverlap && len(ranges) == 0 {
		// The specified ranges did not overlap with the content.
		return nil, ErrNoOverlap
	}
	return ranges, nil
}

// countingWriter counts how many bytes have been written to it.
type countingWriter int64

func (w *countingWriter) Write(p []byte) (n int, err error) {
	*w += countingWriter(len(p))
	return len(p), nil
}

// RangesMIMESize returns the number of bytes it takes to encode the
// provided ranges as a multipart response.
func RangesMIMESize(ranges []HTTPRange, contentType string, contentSize int64) (encSize int64) {
	var w countingWriter
	mw := multipart.NewWriter(&w)
	for _, ra := range ranges {
		mw.CreatePart(ra.MIMEHeader(contentType, contentSize))
		encSize += ra.Length
	}
	mw.Close()
	encSize += int64(w)
	return
}

func SumRangesSize(ranges []HTTPRange) (size int64) {
	for _, ra := range ranges {
		size += ra.Length
	}
	return
}
