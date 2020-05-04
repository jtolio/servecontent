// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
)

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

	ctype, err := content.ContentType(ctx)
	if err != nil {
		http.Error(w, "failed getting content type", http.StatusInternalServerError)
		return
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}

	if size < 0 {
		w.WriteHeader(http.StatusOK)
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

	switch {
	case len(ranges) == 1:
		oneRange(ctx, w, r, content, ranges[0], size)
		return
	case len(ranges) > 1:
		multiRange(ctx, w, r, content, ranges, size, ctype)
		return
	}
	noRanges(ctx, w, r, content, size)
}

func noRanges(ctx context.Context, w http.ResponseWriter, r *http.Request, content Content, size int64) {
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rc, err := content.Range(ctx, 0, size)
	if err != nil {
		// TODO: this maybe should be http.StatusRequestedRangeNotSatisfiable sometimes
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.WriteHeader(http.StatusOK)
	io.CopyN(w, rc, size)
}

func oneRange(ctx context.Context, w http.ResponseWriter, r *http.Request, content Content, ra HTTPRange, size int64) {
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
	w.Header().Set("Content-Range", ra.ContentRange(size))

	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusPartialContent)
		return
	}

	rc, err := content.Range(ctx, ra.Start, ra.Length)
	if err != nil {
		// TODO: should we return http.StatusRequestedRangeNotSatisfiable in certain
		// Range error conditions?
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.WriteHeader(http.StatusPartialContent)
	io.CopyN(w, rc, ra.Length)
}

func multiRange(ctx context.Context, w http.ResponseWriter, r *http.Request, content Content, ranges []HTTPRange, size int64, ctype string) {
	if SumRangesSize(ranges) > size {
		// The total number of bytes in all the ranges
		// is larger than the size of the file by
		// itself, so this is probably an attack, or a
		// dumb client. Ignore the range request.
		noRanges(ctx, w, r, content, size)
		return
	}

	sendSize := RangesMIMESize(ranges, ctype, size)

	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())
	w.Header().Set("Accept-Ranges", "bytes")
	if w.Header().Get("Content-Encoding") == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(sendSize, 10))
	}

	w.WriteHeader(http.StatusPartialContent)

	if r.Method == "HEAD" {
		return
	}

	for _, ra := range ranges {
		part, err := mw.CreatePart(ra.MIMEHeader(ctype, size))
		if err != nil {
			return
		}
		contentRange, err := content.Range(ctx, ra.Start, ra.Length)
		if err != nil {
			return
		}
		_, err = io.CopyN(part, contentRange, ra.Length)
		contentRange.Close()
		if err != nil {
			return
		}
	}
	mw.Close()
}
