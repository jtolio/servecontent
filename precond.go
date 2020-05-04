// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"net/http"
	"time"
)

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
