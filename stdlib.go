// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The DetectContentType algorithm uses at most sniffLen bytes to make its decision.
const sniffLen = 512

// ErrNoOverlap is returned by ParseRange if first-byte-pos of
// all of the byte-range-spec values is greater than the content size.
var ErrNoOverlap = errors.New("invalid range: failed to overlap")

func min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}

// TODO: update documentation here
// ServeContent replies to the request using the content in the
// provided ReadSeeker. The main benefit of ServeContent over io.Copy
// is that it handles Range requests properly, sets the MIME type, and
// handles If-Match, If-Unmodified-Since, If-None-Match, If-Modified-Since,
// and If-Range requests.
//
// If the response's Content-Type header is not set, ServeContent
// first tries to deduce the type from name's file extension and,
// if that fails, falls back to reading the first block of the content
// and passing it to DetectContentType.
// The name is otherwise unused; in particular it can be empty and is
// never sent in the response.
//
// If modtime is not the zero time or Unix epoch, ServeContent
// includes it in a Last-Modified header in the response. If the
// request includes an If-Modified-Since header, ServeContent uses
// modtime to decide whether the content needs to be sent at all.
//
// The content's Seek method must work: ServeContent uses
// a seek to the end of the content to determine its size.
//
// If the caller has set w's ETag header formatted per RFC 7232, section 2.3,
// ServeContent uses it to handle requests using If-Match, If-None-Match, or If-Range.
//
// Note that *os.File implements the io.ReadSeeker interface.
func ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request, content Content) {
	name := content.Name
	sizeFunc := content.Size

	// EvaluatePreconditions sets w.Header() Last-Modified and Etag fields for us.
	if EvaluatePreconditions(w, r, content.ModTime, content.ETag) {
		return
	}

	size, err := sizeFunc(ctx)
	if err != nil {
		// The underlying error text isn't included in the reply so it's not sent to end users.
		http.Error(w, "failed getting size", http.StatusInternalServerError)
		return
	}

	rangeReq := r.Header.Get("Range")
	if rangeReq != "" && CheckIfRange(r, content.ETag, content.ModTime) == CondFalse {
		rangeReq = ""
	}

	code := http.StatusOK

	// If Content-Type isn't set, use the file's extension to find it, but
	// if the Content-Type is unset explicitly, do not sniff the type.
	ctypes, haveType := w.Header()["Content-Type"]
	var ctype string
	if !haveType {
		ctype = mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			// read a chunk to decide between utf-8 text and binary
			// TODO: don't force reading this twice
			r, err := content.Range(ctx, 0, min(sniffLen, size))
			if err != nil {
				http.Error(w, "failed getting range", http.StatusInternalServerError)
				return
			}
			buf, err := ioutil.ReadAll(r)
			r.Close()
			if err != nil {
				http.Error(w, "failed getting range", http.StatusInternalServerError)
				return
			}
			ctype = http.DetectContentType(buf)
		}
		w.Header().Set("Content-Type", ctype)
	} else if len(ctypes) > 0 {
		ctype = ctypes[0]
	}

	sendSize := size
	sendContent := func() (io.ReadCloser, error) { return content.Range(ctx, 0, size) }

	if size < 0 {
		w.WriteHeader(code)
		return
	}

	ranges, err := ParseRange(rangeReq, size)
	if err != nil {
		if err == ErrNoOverlap {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		}
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if SumRangesSize(ranges) > size {
		// The total number of bytes in all the ranges
		// is larger than the size of the file by
		// itself, so this is probably an attack, or a
		// dumb client. Ignore the range request.
		ranges = nil
	}
	switch {
	case len(ranges) == 1:
		// RFC 7233, Section 4.1:
		// "If a single part is being transferred, the server
		// generating the 206 response MUST generate a
		// Content-Range header field, describing what range
		// of the selected representation is enclosed, and a
		// payload consisting of the range.
		// ...
		// A server MUST NOT generate a multipart response to
		// a request for a single range, since a client that
		// does not request multiple parts might not support
		// multipart responses."
		ra := ranges[0]
		sendContent = func() (io.ReadCloser, error) {
			// TODO: should we return http.StatusRequestedRangeNotSatisfiable in certain
			// Range error conditions?
			return content.Range(ctx, ra.Start, ra.Length)
		}
		sendSize = ra.Length
		code = http.StatusPartialContent
		w.Header().Set("Content-Range", ra.ContentRange(size))
	case len(ranges) > 1:
		sendSize = RangesMIMESize(ranges, ctype, size)
		code = http.StatusPartialContent

		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		w.Header().Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())
		sendContent = func() (io.ReadCloser, error) { return ioutil.NopCloser(pr), nil }
		defer pr.Close() // cause writing goroutine to fail and exit if CopyN doesn't finish.
		go func() {
			for _, ra := range ranges {
				part, err := mw.CreatePart(ra.MIMEHeader(ctype, size))
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				contentRange, err := content.Range(ctx, ra.Start, ra.Length)
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				_, err = io.CopyN(part, contentRange, ra.Length)
				contentRange.Close()
				if err != nil {
					pw.CloseWithError(err)
					return
				}
			}
			mw.Close()
			pw.Close()
		}()

		w.Header().Set("Accept-Ranges", "bytes")
		if w.Header().Get("Content-Encoding") == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(sendSize, 10))
		}
	}

	w.WriteHeader(code)

	if r.Method != "HEAD" {
		r, err := sendContent()
		if err != nil {
			// TODO: this maybe should be http.StatusRequestedRangeNotSatisfiable sometimes
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer r.Close()

		io.CopyN(w, r, sendSize)
	}
}

// ScanETag determines if a syntactically valid ETag is present at s. If so,
// the ETag and remaining text after consuming ETag is returned. Otherwise,
// it returns "", "".
func ScanETag(s string) (etag string, remain string) {
	s = textproto.TrimString(s)
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s[start:]) < 2 || s[start] != '"' {
		return "", ""
	}
	// ETag is either W/"text" or "text".
	// See RFC 7232 2.3.
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		// Character values allowed in ETags.
		case c == 0x21 || c >= 0x23 && c <= 0x7E || c >= 0x80:
		case c == '"':
			return s[:i+1], s[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

// ETagStrongMatch reports whether a and b match using strong ETag comparison.
// Assumes a and b are valid ETags.
func ETagStrongMatch(a, b string) bool {
	return a == b && a != "" && a[0] == '"'
}

// ETagWeakMatch reports whether a and b match using weak ETag comparison.
// Assumes a and b are valid ETags.
func ETagWeakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// CondResult is the result of an HTTP request precondition check.
// See https://tools.ietf.org/html/rfc7232 section 3.
type CondResult int

const (
	CondNone CondResult = iota
	CondTrue
	CondFalse
)

func CheckIfMatch(h http.Header, etag string) CondResult {
	im := h.Get("If-Match")
	if im == "" {
		return CondNone
	}
	for {
		im = textproto.TrimString(im)
		if len(im) == 0 {
			break
		}
		if im[0] == ',' {
			im = im[1:]
			continue
		}
		if im[0] == '*' {
			return CondTrue
		}
		et, remain := ScanETag(im)
		if et == "" {
			break
		}
		if ETagStrongMatch(et, etag) {
			return CondTrue
		}
		im = remain
	}

	return CondFalse
}

func CheckIfUnmodifiedSince(h http.Header, modtime time.Time) CondResult {
	ius := h.Get("If-Unmodified-Since")
	if ius == "" || isZeroTime(modtime) {
		return CondNone
	}
	t, err := http.ParseTime(ius)
	if err != nil {
		return CondNone
	}

	// The Last-Modified header truncates sub-second precision so
	// the modtime needs to be truncated too.
	modtime = modtime.Truncate(time.Second)
	if modtime.Before(t) || modtime.Equal(t) {
		return CondTrue
	}
	return CondFalse
}

func CheckIfNoneMatch(h http.Header, etag string) CondResult {
	inm := h.Get("If-None-Match")
	if inm == "" {
		return CondNone
	}
	buf := inm
	for {
		buf = textproto.TrimString(buf)
		if len(buf) == 0 {
			break
		}
		if buf[0] == ',' {
			buf = buf[1:]
		}
		if buf[0] == '*' {
			return CondFalse
		}
		et, remain := ScanETag(buf)
		if et == "" {
			break
		}
		if ETagWeakMatch(et, etag) {
			return CondFalse
		}
		buf = remain
	}
	return CondTrue
}

func CheckIfModifiedSince(r *http.Request, modtime time.Time) CondResult {
	if r.Method != "GET" && r.Method != "HEAD" {
		return CondNone
	}
	ims := r.Header.Get("If-Modified-Since")
	if ims == "" || isZeroTime(modtime) {
		return CondNone
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return CondNone
	}
	// The Last-Modified header truncates sub-second precision so
	// the modtime needs to be truncated too.
	modtime = modtime.Truncate(time.Second)
	if modtime.Before(t) || modtime.Equal(t) {
		return CondFalse
	}
	return CondTrue
}

func CheckIfRange(r *http.Request, etag string, modtime time.Time) CondResult {
	if r.Method != "GET" && r.Method != "HEAD" {
		return CondNone
	}
	ir := r.Header.Get("If-Range")
	if ir == "" {
		return CondNone
	}
	et, _ := ScanETag(ir)
	if et != "" {
		if ETagStrongMatch(et, etag) {
			return CondTrue
		} else {
			return CondFalse
		}
	}
	// The If-Range value is typically the ETag value, but it may also be
	// the modtime date. See golang.org/issue/8367.
	if modtime.IsZero() {
		return CondFalse
	}
	t, err := http.ParseTime(ir)
	if err != nil {
		return CondFalse
	}
	if t.Unix() == modtime.Unix() {
		return CondTrue
	}
	return CondFalse
}

var unixEpochTime = time.Unix(0, 0)

// isZeroTime reports whether t is obviously unspecified (either zero or Unix()=0).
func isZeroTime(t time.Time) bool {
	return t.IsZero() || t.Equal(unixEpochTime)
}

func WriteNotModified(w http.ResponseWriter) {
	// RFC 7232 section 4.1:
	// a sender SHOULD NOT generate representation metadata other than the
	// above listed fields unless said metadata exists for the purpose of
	// guiding cache updates (e.g., Last-Modified might be useful if the
	// response does not have an ETag field).
	h := w.Header()
	delete(h, "Content-Type")
	delete(h, "Content-Length")
	if h.Get("Etag") != "" {
		delete(h, "Last-Modified")
	}
	w.WriteHeader(http.StatusNotModified)
}

// EvaluatePreconditions evaluates request preconditions and reports whether a precondition
// resulted in sending StatusNotModified or StatusPreconditionFailed.
func EvaluatePreconditions(w http.ResponseWriter, r *http.Request, modtime time.Time, etag string) (done bool) {
	// This function carefully follows RFC 7232 section 6.
	if !isZeroTime(modtime) {
		w.Header().Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	}
	if etag != "" {
		w.Header().Set("Etag", etag)
	}
	ch := CheckIfMatch(r.Header, etag)
	if ch == CondNone {
		ch = CheckIfUnmodifiedSince(r.Header, modtime)
	}
	if ch == CondFalse {
		w.WriteHeader(http.StatusPreconditionFailed)
		return true
	}
	switch CheckIfNoneMatch(r.Header, etag) {
	case CondFalse:
		if r.Method == "GET" || r.Method == "HEAD" {
			WriteNotModified(w)
			return true
		} else {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
	case CondNone:
		if CheckIfModifiedSince(r, modtime) == CondFalse {
			WriteNotModified(w)
			return true
		}
	}

	return false
}

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
