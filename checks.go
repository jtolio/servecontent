// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"net/http"
	"net/textproto"
	"time"
)

var unixEpochTime = time.Unix(0, 0)

// isZeroTime reports whether t is obviously unspecified (either zero or Unix()=0).
func isZeroTime(t time.Time) bool {
	return t.IsZero() || t.Equal(unixEpochTime)
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
