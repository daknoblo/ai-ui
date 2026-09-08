package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

type staticHandler struct {
	assets map[string]staticAsset
}

// newStaticHandler serves the embedded assets from memory.
//
// http.FileServer cannot emit validators for an embed.FS because its entries
// have no modification time, so every page load re-downloaded the bundled
// JavaScript. Hashing the content once at start-up turns those requests into
// cheap 304 responses. Content-versioned URLs also let newly fetched dialogs
// replace a stylesheet that an already open page loaded before an image update.
func newStaticHandler(fsys fs.FS) (*staticHandler, error) {
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

	return &staticHandler{assets: assets}, nil
}

func (h *staticHandler) URL(name string) (string, error) {
	asset, ok := h.assets[name]
	if !ok {
		return "", fmt.Errorf("embedded asset %q does not exist", name)
	}
	return "/static/" + name + "?v=" + strings.Trim(asset.etag, `"`), nil
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/static/"))
	asset, ok := h.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("Cache-Control", "no-cache")
	// A zero modtime makes ServeContent rely on the ETag alone.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(asset.data))
}
