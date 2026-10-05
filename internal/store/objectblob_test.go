package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObjectBlobUpsertKeepsDigestAndRefusesSizeChange(t *testing.T) {
	st := newTestStore(t)
	when := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return when }
	digest := strings.Repeat("ab", 32)
	row := ObjectBlob{
		Digest: digest, SizeBytes: 12, ObjectKey: "blobs/" + digest,
		Backend: "memory", Kind: "artifact", MediaType: "application/gzip",
	}
	if err := st.UpsertObjectBlob(row); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetObjectBlob(digest)
	if err != nil || !ok || !got.CreatedAt.Equal(when) || got.Backend != "memory" || got.Kind != "artifact" {
		t.Fatalf("got %+v ok %v err %v", got, ok, err)
	}
	row.Backend = "r2"
	row.Kind = "evidence"
	row.MediaType = "text/plain"
	if err := st.UpsertObjectBlob(row); err != nil {
		t.Fatal(err)
	}
	got, _, err = st.GetObjectBlob(digest)
	if err != nil || got.Backend != "r2" || got.Kind != "evidence" || !got.CreatedAt.Equal(when) {
		t.Fatalf("refreshed %+v err %v", got, err)
	}
	row.SizeBytes = 13
	if err := st.UpsertObjectBlob(row); err == nil {
		t.Fatal("size change was accepted")
	}
	row.SizeBytes = 12
	row.ObjectKey = "other/" + digest
	if err := st.UpsertObjectBlob(row); err == nil {
		t.Fatal("foreign object key was accepted")
	}
	if _, ok, err := st.GetObjectBlob(strings.Repeat("c", 64)); err != nil || ok {
		t.Fatalf("missing ok=%v err=%v", ok, err)
	}
}

func TestObjectBlobTableIsOptionalLedgerIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatal(err)
	}
}
