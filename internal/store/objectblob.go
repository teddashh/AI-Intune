package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrObjectBlobConflict means a digest is already stored with a different size
// or object key. Same bytes may be republished; a different size is not.
var ErrObjectBlobConflict = errors.New("store: object blob does not match the stored digest")

// ObjectBlob is the SQLite reference for bytes a backend already stored.
// The digest is the Hub's own SHA-256. This row is not a machine fact.
type ObjectBlob struct {
	Digest    string
	SizeBytes int64
	ObjectKey string
	Backend   string
	Kind      string
	MediaType string
	CreatedAt time.Time
}

// UpsertObjectBlob inserts a content-addressed reference. An existing row with
// the same digest and size is kept; backend, kind, and media type refresh to
// the latest successful publish. A size or key mismatch is refused.
func (s *Store) UpsertObjectBlob(row ObjectBlob) error {
	if err := validateObjectBlob(row); err != nil {
		return err
	}
	var size int64
	var key, backend, kind, media string
	err := s.db.QueryRow(`SELECT size_bytes, object_key, backend, kind, media_type
		FROM object_blobs WHERE digest=?`, row.Digest).Scan(&size, &key, &backend, &kind, &media)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created := fmtTime(s.now().UTC().Truncate(time.Second))
		_, err = s.db.Exec(`INSERT INTO object_blobs(
			digest, size_bytes, object_key, backend, kind, media_type, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			row.Digest, row.SizeBytes, row.ObjectKey, row.Backend, row.Kind, row.MediaType, created)
		return err
	case err != nil:
		return err
	}
	if size != row.SizeBytes || key != row.ObjectKey {
		return ErrObjectBlobConflict
	}
	if backend == row.Backend && kind == row.Kind && media == row.MediaType {
		return nil
	}
	_, err = s.db.Exec(`UPDATE object_blobs SET backend=?, kind=?, media_type=?
		WHERE digest=? AND size_bytes=? AND object_key=?`,
		row.Backend, row.Kind, row.MediaType, row.Digest, row.SizeBytes, row.ObjectKey)
	return err
}

// GetObjectBlob returns the reference for digest. ok is false when it is absent.
func (s *Store) GetObjectBlob(digest string) (ObjectBlob, bool, error) {
	if !objectDigest(digest) {
		return ObjectBlob{}, false, errors.New("store: object blob digest is invalid")
	}
	var row ObjectBlob
	var created string
	err := s.db.QueryRow(`SELECT digest, size_bytes, object_key, backend, kind, media_type, created_at
		FROM object_blobs WHERE digest=?`, digest).Scan(
		&row.Digest, &row.SizeBytes, &row.ObjectKey, &row.Backend, &row.Kind, &row.MediaType, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ObjectBlob{}, false, nil
	}
	if err != nil {
		return ObjectBlob{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return ObjectBlob{}, false, err
	}
	row.CreatedAt = parsed.UTC()
	return row, true, nil
}

func validateObjectBlob(row ObjectBlob) error {
	if !objectDigest(row.Digest) || row.SizeBytes <= 0 || row.SizeBytes > 1<<30 {
		return errors.New("store: object blob is invalid")
	}
	if row.ObjectKey != "blobs/"+row.Digest {
		return errors.New("store: object blob key is invalid")
	}
	switch row.Backend {
	case "r2", "s3", "dir", "memory":
	default:
		return errors.New("store: object blob backend is invalid")
	}
	switch row.Kind {
	case "artifact", "evidence":
	default:
		return errors.New("store: object blob kind is invalid")
	}
	if row.MediaType == "" || len(row.MediaType) > 128 || strings.TrimSpace(row.MediaType) != row.MediaType {
		return errors.New("store: object blob media type is invalid")
	}
	return nil
}

func objectDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
