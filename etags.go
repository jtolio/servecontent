// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"net/textproto"
	"strings"
)

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
