package main

import (
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/teddashh/AI-Intune/internal/blobstore"
)

// serveRemoteArtifact streams a Hub-measured artifact when the local tarball
// is missing and SQLite names the configured backend. The object key in the
// row must equal blobs/<digest>; a mismatched row is treated as absent.
// The bool is true only after headers are written.
func (h *hub) serveRemoteArtifact(w http.ResponseWriter, r *http.Request, digest string) bool {
	if h == nil || h.blobs == nil || h.store == nil {
		return false
	}
	row, ok, err := h.store.GetObjectBlob(digest)
	if err != nil || !ok {
		return false
	}
	if row.Kind != blobstore.KindArtifact || row.Backend != h.blobs.Name() ||
		row.ObjectKey != blobstore.ObjectKey(digest) || row.SizeBytes <= 0 {
		return false
	}
	body, obj, err := h.blobs.Open(r.Context(), digest)
	if err != nil {
		log.Printf("遠端 artifact %s 讀取失敗", digest)
		return false
	}
	defer body.Close()
	if obj.Digest != digest || obj.Key != row.ObjectKey || obj.Size != row.SizeBytes {
		log.Printf("遠端 artifact %s 與帳本不一致", digest)
		return false
	}
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return true
	}
	if _, err := io.Copy(w, io.LimitReader(body, obj.Size)); err != nil {
		log.Printf("串流遠端 artifact %s 失敗", digest)
	}
	return true
}
