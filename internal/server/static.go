package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// staticAsset is an embedded file together with its content based ETag.
type staticAsset struct {
	data []byte
	etag string
}

// newStaticHandler serves the embedded assets from memory.
//
// http.FileServer cannot emit validators for an embed.FS because its entries
// have no modification time, so every page load re-downloaded the bundled
// JavaScript. Hashing the content once at start-up turns those requests into
// cheap 304 responses while "no-cache" still guarantees that a new image is
// picked up immediately.
func newStaticHandler(fsys fs.FS) (http.Handler, error) {
	assets := map[string]staticAsset{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		assets[p] = staticAsset{
			data: data,
			etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean(strings.TrimPrefix(r.URL.Path, "/static/"))
		asset, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", asset.etag)
		w.Header().Set("Cache-Control", "no-cache")
		// A zero modtime makes ServeContent rely on the ETag alone.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(asset.data))
	}), nil
}
