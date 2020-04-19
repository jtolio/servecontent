// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package servecontent

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testFile    = "testdata/file"
	testFileLen = 11
)

func ServeFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := os.Open(name)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	d, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	ServeContent(w, r, name, d.ModTime(), f)
}

type wantRange struct {
	start, end int64 // range [start,end)
}

var ServeFileRangeTests = []struct {
	r      string
	code   int
	ranges []wantRange
}{
	{r: "", code: http.StatusOK},
	{r: "bytes=0-4", code: http.StatusPartialContent, ranges: []wantRange{{0, 5}}},
	{r: "bytes=2-", code: http.StatusPartialContent, ranges: []wantRange{{2, testFileLen}}},
	{r: "bytes=-5", code: http.StatusPartialContent, ranges: []wantRange{{testFileLen - 5, testFileLen}}},
	{r: "bytes=3-7", code: http.StatusPartialContent, ranges: []wantRange{{3, 8}}},
	{r: "bytes=0-0,-2", code: http.StatusPartialContent, ranges: []wantRange{{0, 1}, {testFileLen - 2, testFileLen}}},
	{r: "bytes=0-1,5-8", code: http.StatusPartialContent, ranges: []wantRange{{0, 2}, {5, 9}}},
	{r: "bytes=0-1,5-", code: http.StatusPartialContent, ranges: []wantRange{{0, 2}, {5, testFileLen}}},
	{r: "bytes=5-1000", code: http.StatusPartialContent, ranges: []wantRange{{5, testFileLen}}},
	{r: "bytes=0-,1-,2-,3-,4-", code: http.StatusOK}, // ignore wasteful range request
	{r: "bytes=0-9", code: http.StatusPartialContent, ranges: []wantRange{{0, testFileLen - 1}}},
	{r: "bytes=0-10", code: http.StatusPartialContent, ranges: []wantRange{{0, testFileLen}}},
	{r: "bytes=0-11", code: http.StatusPartialContent, ranges: []wantRange{{0, testFileLen}}},
	{r: "bytes=10-11", code: http.StatusPartialContent, ranges: []wantRange{{testFileLen - 1, testFileLen}}},
	{r: "bytes=10-", code: http.StatusPartialContent, ranges: []wantRange{{testFileLen - 1, testFileLen}}},
	{r: "bytes=11-", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=11-12", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=12-12", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=11-100", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=12-100", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=100-", code: http.StatusRequestedRangeNotSatisfiable},
	{r: "bytes=100-1000", code: http.StatusRequestedRangeNotSatisfiable},
}

func TestServeFile(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, testFile)
	}))
	defer ts.Close()
	c := ts.Client()

	var err error

	file, err := ioutil.ReadFile(testFile)
	if err != nil {
		t.Fatal("reading file:", err)
	}

	// set up the Request (re-used for all tests)
	var req http.Request
	req.Header = make(http.Header)
	if req.URL, err = url.Parse(ts.URL); err != nil {
		t.Fatal("ParseURL:", err)
	}
	req.Method = "GET"

	// straight GET
	_, body := getBody(t, "straight get", req, c)
	if !bytes.Equal(body, file) {
		t.Fatalf("body mismatch: got %q, want %q", body, file)
	}

	// Range tests
Cases:
	for _, rt := range ServeFileRangeTests {
		if rt.r != "" {
			req.Header.Set("Range", rt.r)
		}
		resp, body := getBody(t, fmt.Sprintf("range test %q", rt.r), req, c)
		if resp.StatusCode != rt.code {
			t.Errorf("range=%q: StatusCode=%d, want %d", rt.r, resp.StatusCode, rt.code)
		}
		if rt.code == http.StatusRequestedRangeNotSatisfiable {
			continue
		}
		wantContentRange := ""
		if len(rt.ranges) == 1 {
			rng := rt.ranges[0]
			wantContentRange = fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end-1, testFileLen)
		}
		cr := resp.Header.Get("Content-Range")
		if cr != wantContentRange {
			t.Errorf("range=%q: Content-Range = %q, want %q", rt.r, cr, wantContentRange)
		}
		ct := resp.Header.Get("Content-Type")
		if len(rt.ranges) == 1 {
			rng := rt.ranges[0]
			wantBody := file[rng.start:rng.end]
			if !bytes.Equal(body, wantBody) {
				t.Errorf("range=%q: body = %q, want %q", rt.r, body, wantBody)
			}
			if strings.HasPrefix(ct, "multipart/byteranges") {
				t.Errorf("range=%q content-type = %q; unexpected multipart/byteranges", rt.r, ct)
			}
		}
		if len(rt.ranges) > 1 {
			typ, params, err := mime.ParseMediaType(ct)
			if err != nil {
				t.Errorf("range=%q content-type = %q; %v", rt.r, ct, err)
				continue
			}
			if typ != "multipart/byteranges" {
				t.Errorf("range=%q content-type = %q; want multipart/byteranges", rt.r, typ)
				continue
			}
			if params["boundary"] == "" {
				t.Errorf("range=%q content-type = %q; lacks boundary", rt.r, ct)
				continue
			}
			if g, w := resp.ContentLength, int64(len(body)); g != w {
				t.Errorf("range=%q Content-Length = %d; want %d", rt.r, g, w)
				continue
			}
			mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
			for ri, rng := range rt.ranges {
				part, err := mr.NextPart()
				if err != nil {
					t.Errorf("range=%q, reading part index %d: %v", rt.r, ri, err)
					continue Cases
				}
				wantContentRange = fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end-1, testFileLen)
				if g, w := part.Header.Get("Content-Range"), wantContentRange; g != w {
					t.Errorf("range=%q: part Content-Range = %q; want %q", rt.r, g, w)
				}
				body, err := ioutil.ReadAll(part)
				if err != nil {
					t.Errorf("range=%q, reading part index %d body: %v", rt.r, ri, err)
					continue Cases
				}
				wantBody := file[rng.start:rng.end]
				if !bytes.Equal(body, wantBody) {
					t.Errorf("range=%q: body = %q, want %q", rt.r, body, wantBody)
				}
			}
			_, err = mr.NextPart()
			if err != io.EOF {
				t.Errorf("range=%q; expected final error io.EOF; got %v", rt.r, err)
			}
		}
	}
}

func TestServeFileContentType(t *testing.T) {
	const ctype = "icecream/chocolate"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("override") {
		case "1":
			w.Header().Set("Content-Type", ctype)
		case "2":
			// Explicitly inhibit sniffing.
			w.Header()["Content-Type"] = []string{}
		}
		ServeFile(w, r, "testdata/file")
	}))
	defer ts.Close()
	get := func(override string, want []string) {
		resp, err := http.Get(ts.URL + "?override=" + override)
		if err != nil {
			t.Fatal(err)
		}
		if h := resp.Header["Content-Type"]; !reflect.DeepEqual(h, want) {
			t.Errorf("Content-Type mismatch: got %v, want %v", h, want)
		}
		resp.Body.Close()
	}
	get("0", []string{"text/plain; charset=utf-8"})
	get("1", []string{ctype})
	get("2", nil)
}

func TestServeFileMimeType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeFile(w, r, "testdata/style.css")
	}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := "text/css; charset=utf-8"
	if h := resp.Header.Get("Content-Type"); h != want {
		t.Errorf("Content-Type mismatch: got %q, want %q", h, want)
	}
}

func TestServeFileFromCWD(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeFile(w, r, "stdlib_test.go")
	}))
	defer ts.Close()
	r, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("expected 200 OK, got %s", r.Status)
	}
}

func mustStat(t *testing.T, fileName string) os.FileInfo {
	fi, err := os.Stat(fileName)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestServeContent(t *testing.T) {
	type serveParam struct {
		name        string
		modtime     time.Time
		content     io.ReadSeeker
		contentType string
		etag        string
	}
	servec := make(chan serveParam, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := <-servec
		if p.etag != "" {
			w.Header().Set("ETag", p.etag)
		}
		if p.contentType != "" {
			w.Header().Set("Content-Type", p.contentType)
		}
		ServeContent(w, r, p.name, p.modtime, p.content)
	}))
	defer ts.Close()

	type testCase struct {
		// One of file or content must be set:
		file    string
		content io.ReadSeeker

		modtime          time.Time
		serveETag        string // optional
		serveContentType string // optional
		reqHeader        map[string]string
		wantLastMod      string
		wantContentType  string
		wantContentRange string
		wantStatus       int
	}
	htmlModTime := mustStat(t, "testdata/index.html").ModTime()
	tests := map[string]testCase{
		"no_last_modified": {
			file:            "testdata/style.css",
			wantContentType: "text/css; charset=utf-8",
			wantStatus:      200,
		},
		"with_last_modified": {
			file:            "testdata/index.html",
			wantContentType: "text/html; charset=utf-8",
			modtime:         htmlModTime,
			wantLastMod:     htmlModTime.UTC().Format(http.TimeFormat),
			wantStatus:      200,
		},
		"not_modified_modtime": {
			file:      "testdata/style.css",
			serveETag: `"foo"`, // Last-Modified sent only when no ETag
			modtime:   htmlModTime,
			reqHeader: map[string]string{
				"If-Modified-Since": htmlModTime.UTC().Format(http.TimeFormat),
			},
			wantStatus: 304,
		},
		"not_modified_modtime_with_contenttype": {
			file:             "testdata/style.css",
			serveContentType: "text/css", // explicit content type
			serveETag:        `"foo"`,    // Last-Modified sent only when no ETag
			modtime:          htmlModTime,
			reqHeader: map[string]string{
				"If-Modified-Since": htmlModTime.UTC().Format(http.TimeFormat),
			},
			wantStatus: 304,
		},
		"not_modified_etag": {
			file:      "testdata/style.css",
			serveETag: `"foo"`,
			reqHeader: map[string]string{
				"If-None-Match": `"foo"`,
			},
			wantStatus: 304,
		},
		"not_modified_etag_no_seek": {
			content:   panicOnSeek{nil}, // should never be called
			serveETag: `W/"foo"`,        // If-None-Match uses weak ETag comparison
			reqHeader: map[string]string{
				"If-None-Match": `"baz", W/"foo"`,
			},
			wantStatus: 304,
		},
		"if_none_match_mismatch": {
			file:      "testdata/style.css",
			serveETag: `"foo"`,
			reqHeader: map[string]string{
				"If-None-Match": `"Foo"`,
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
		},
		"range_good": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"Range": "bytes=0-4",
			},
			wantStatus:       http.StatusPartialContent,
			wantContentType:  "text/css; charset=utf-8",
			wantContentRange: "bytes 0-4/8",
		},
		"range_match": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": `"A"`,
			},
			wantStatus:       http.StatusPartialContent,
			wantContentType:  "text/css; charset=utf-8",
			wantContentRange: "bytes 0-4/8",
		},
		"range_match_weak_etag": {
			file:      "testdata/style.css",
			serveETag: `W/"A"`,
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": `W/"A"`,
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
		},
		"range_no_overlap": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"Range": "bytes=10-20",
			},
			wantStatus:       http.StatusRequestedRangeNotSatisfiable,
			wantContentType:  "text/plain; charset=utf-8",
			wantContentRange: "bytes */8",
		},
		// An If-Range resource for entity "A", but entity "B" is now current.
		// The Range request should be ignored.
		"range_no_match": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": `"B"`,
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
		},
		"range_with_modtime": {
			file:    "testdata/style.css",
			modtime: time.Date(2014, 6, 25, 17, 12, 18, 0 /* nanos */, time.UTC),
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": "Wed, 25 Jun 2014 17:12:18 GMT",
			},
			wantStatus:       http.StatusPartialContent,
			wantContentType:  "text/css; charset=utf-8",
			wantContentRange: "bytes 0-4/8",
			wantLastMod:      "Wed, 25 Jun 2014 17:12:18 GMT",
		},
		"range_with_modtime_mismatch": {
			file:    "testdata/style.css",
			modtime: time.Date(2014, 6, 25, 17, 12, 18, 0 /* nanos */, time.UTC),
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": "Wed, 25 Jun 2014 17:12:19 GMT",
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/css; charset=utf-8",
			wantLastMod:     "Wed, 25 Jun 2014 17:12:18 GMT",
		},
		"range_with_modtime_nanos": {
			file:    "testdata/style.css",
			modtime: time.Date(2014, 6, 25, 17, 12, 18, 123 /* nanos */, time.UTC),
			reqHeader: map[string]string{
				"Range":    "bytes=0-4",
				"If-Range": "Wed, 25 Jun 2014 17:12:18 GMT",
			},
			wantStatus:       http.StatusPartialContent,
			wantContentType:  "text/css; charset=utf-8",
			wantContentRange: "bytes 0-4/8",
			wantLastMod:      "Wed, 25 Jun 2014 17:12:18 GMT",
		},
		"unix_zero_modtime": {
			content:         strings.NewReader("<html>foo"),
			modtime:         time.Unix(0, 0),
			wantStatus:      http.StatusOK,
			wantContentType: "text/html; charset=utf-8",
		},
		"ifmatch_matches": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"If-Match": `"Z", "A"`,
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
		},
		"ifmatch_star": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"If-Match": `*`,
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
		},
		"ifmatch_failed": {
			file:      "testdata/style.css",
			serveETag: `"A"`,
			reqHeader: map[string]string{
				"If-Match": `"B"`,
			},
			wantStatus: 412,
		},
		"ifmatch_fails_on_weak_etag": {
			file:      "testdata/style.css",
			serveETag: `W/"A"`,
			reqHeader: map[string]string{
				"If-Match": `W/"A"`,
			},
			wantStatus: 412,
		},
		"if_unmodified_since_true": {
			file:    "testdata/style.css",
			modtime: htmlModTime,
			reqHeader: map[string]string{
				"If-Unmodified-Since": htmlModTime.UTC().Format(http.TimeFormat),
			},
			wantStatus:      200,
			wantContentType: "text/css; charset=utf-8",
			wantLastMod:     htmlModTime.UTC().Format(http.TimeFormat),
		},
		"if_unmodified_since_false": {
			file:    "testdata/style.css",
			modtime: htmlModTime,
			reqHeader: map[string]string{
				"If-Unmodified-Since": htmlModTime.Add(-2 * time.Second).UTC().Format(http.TimeFormat),
			},
			wantStatus:  412,
			wantLastMod: htmlModTime.UTC().Format(http.TimeFormat),
		},
	}
	for testName, tt := range tests {
		var content io.ReadSeeker
		if tt.file != "" {
			f, err := os.Open(tt.file)
			if err != nil {
				t.Fatalf("test %q: %v", testName, err)
			}
			defer f.Close()
			content = f
		} else {
			content = tt.content
		}
		for _, method := range []string{"GET", "HEAD"} {
			//restore content in case it is consumed by previous method
			if content, ok := content.(*strings.Reader); ok {
				content.Seek(0, io.SeekStart)
			}

			servec <- serveParam{
				name:        filepath.Base(tt.file),
				content:     content,
				modtime:     tt.modtime,
				etag:        tt.serveETag,
				contentType: tt.serveContentType,
			}
			req, err := http.NewRequest(method, ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.reqHeader {
				req.Header.Set(k, v)
			}

			c := ts.Client()
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != tt.wantStatus {
				t.Errorf("test %q using %q: got status = %d; want %d", testName, method, res.StatusCode, tt.wantStatus)
			}
			if g, e := res.Header.Get("Content-Type"), tt.wantContentType; g != e {
				t.Errorf("test %q using %q: got content-type = %q, want %q", testName, method, g, e)
			}
			if g, e := res.Header.Get("Content-Range"), tt.wantContentRange; g != e {
				t.Errorf("test %q using %q: got content-range = %q, want %q", testName, method, g, e)
			}
			if g, e := res.Header.Get("Last-Modified"), tt.wantLastMod; g != e {
				t.Errorf("test %q using %q: got last-modified = %q, want %q", testName, method, g, e)
			}
		}
	}
}

func getBody(t *testing.T, testName string, req http.Request, client *http.Client) (*http.Response, []byte) {
	r, err := client.Do(&req)
	if err != nil {
		t.Fatalf("%s: for URL %q, send error: %v", testName, req.URL.String(), err)
	}
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("%s: for URL %q, reading body: %v", testName, req.URL.String(), err)
	}
	return r, b
}

type panicOnSeek struct{ io.ReadSeeker }

func Test_scanETag(t *testing.T) {
	tests := []struct {
		in         string
		wantETag   string
		wantRemain string
	}{
		{`W/"etag-1"`, `W/"etag-1"`, ""},
		{`"etag-2"`, `"etag-2"`, ""},
		{`"etag-1", "etag-2"`, `"etag-1"`, `, "etag-2"`},
		{"", "", ""},
		{"W/", "", ""},
		{`W/"truc`, "", ""},
		{`w/"case-sensitive"`, "", ""},
		{`"spaced etag"`, "", ""},
	}
	for _, test := range tests {
		etag, remain := scanETag(test.in)
		if etag != test.wantETag || remain != test.wantRemain {
			t.Errorf("scanETag(%q)=%q %q, want %q %q", test.in, etag, remain, test.wantETag, test.wantRemain)
		}
	}
}
