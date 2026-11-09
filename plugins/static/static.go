// Package static mounts an explicit fs.FS without directory listings.
package static

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"trest.local/trest"
)

// Options controls the mounted file collection. The supplied filesystem is
// trusted: an os.DirFS containing symlinks can expose their targets. Prefer an
// embedded filesystem or one that enforces its own filesystem-root boundary.
type Options struct {
	MaxBytes      int64
	AllowDotfiles bool
	CacheControl  string
}

// New mounts regular files below prefix. It never renders directory listings or
// treats URL-cleaning as permission to read a different file. Default max 32 MiB.
func New(prefix string, files fs.FS, options Options) (trest.Component, error) {
	if files == nil {
		return nil, errors.New("static: a filesystem is required")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 32 << 20
	}
	if options.MaxBytes < 1 || options.MaxBytes > 1<<40 {
		return nil, errors.New("static: invalid maximum file size")
	}
	if strings.ContainsAny(options.CacheControl, "\r\n") {
		return nil, errors.New("static: invalid cache control")
	}
	if prefix == "" || prefix[0] != '/' || strings.ContainsAny(prefix, "{}?\\#") || path.Clean(prefix) != prefix || prefix == "/" {
		return nil, errors.New("static: use a clean, non-root URL prefix without a trailing slash")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if !validName(name, options.AllowDotfiles) {
			http.NotFound(w, r)
			return
		}
		f, err := files.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if info.Size() < 0 || info.Size() > options.MaxBytes {
			http.Error(w, "file exceeds serving limit", http.StatusRequestEntityTooLarge)
			return
		}
		var reader io.ReadSeeker
		if seekable, ok := f.(io.ReadSeeker); ok {
			reader = seekable
		} else {
			data, readErr := io.ReadAll(io.LimitReader(f, options.MaxBytes+1))
			if readErr != nil {
				http.Error(w, "file unavailable", http.StatusInternalServerError)
				return
			}
			if int64(len(data)) > options.MaxBytes {
				http.Error(w, "file exceeds serving limit", http.StatusRequestEntityTooLarge)
				return
			}
			reader = bytes.NewReader(data)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if options.CacheControl != "" {
			w.Header().Set("Cache-Control", options.CacheControl)
		}
		http.ServeContent(w, r, path.Base(name), info.ModTime(), reader)
	})
	return trest.Raw(http.MethodGet, prefix+"/{file...}", handler).Summary("Serve configured static files"), nil
}
func validName(name string, dotfiles bool) bool {
	if name == "" || name == "." || !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return false
	}
	if !dotfiles {
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") {
				return false
			}
		}
	}
	return true
}
