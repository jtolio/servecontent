package servecontent

import (
	"context"
	"io"
	"net/http"
	"os"
	"time"
)

func serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := os.Open(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	d, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	serveContent(w, r, name, d.ModTime(), f)
}

func serveContent(w http.ResponseWriter, r *http.Request, name string, modtime time.Time, f io.ReadSeeker) {
	ServeReadSeeker(context.Background(), w, r, name, modtime, f)
}
