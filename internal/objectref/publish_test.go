package objectref

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/blobstore"
	"github.com/teddashh/AI-Intune/internal/store"
)

type recordingLedger struct {
	rows []store.ObjectBlob
	err  error
}

func (l *recordingLedger) UpsertObjectBlob(row store.ObjectBlob) error {
	if l.err != nil {
		return l.err
	}
	l.rows = append(l.rows, row)
	return nil
}

func TestPublishArtifactRehashesAndRefusesMismatch(t *testing.T) {
	body := []byte("tarball-bytes")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	mem := blobstore.NewMemory()
	ledger := &recordingLedger{}
	pub := Publisher{Backend: mem, Store: ledger}
	if err := pub.PublishArtifact(context.Background(), dir, digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if len(ledger.rows) != 1 || ledger.rows[0].Digest != digest || ledger.rows[0].Kind != blobstore.KindArtifact ||
		ledger.rows[0].ObjectKey != "blobs/"+digest || ledger.rows[0].SizeBytes != int64(len(body)) {
		t.Fatalf("row: %+v", ledger.rows)
	}
	rc, _, err := mem.Open(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := bytes.NewBuffer(nil), error(nil)
	_, _ = got.ReadFrom(rc)
	_ = rc.Close()
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("stored %q", got.Bytes())
	}

	bad := &recordingLedger{}
	if err := (Publisher{Backend: mem, Store: bad}).PublishArtifact(context.Background(), dir, digest, int64(len(body)+1)); !errors.Is(err, blobstore.ErrDigestMismatch) || len(bad.rows) != 0 {
		t.Fatalf("size mismatch err=%v rows=%d", err, len(bad.rows))
	}
	other := stringsRepeat("b", 64)
	if err := os.WriteFile(filepath.Join(dir, other+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Publisher{Backend: mem, Store: bad}).PublishArtifact(context.Background(), dir, other, int64(len(body))); !errors.Is(err, blobstore.ErrDigestMismatch) || len(bad.rows) != 0 {
		t.Fatalf("digest mismatch err=%v rows=%d", err, len(bad.rows))
	}
}

func TestPublishEvidenceHashesItself(t *testing.T) {
	body := []byte("evidence excerpt bytes")
	mem := blobstore.NewMemory()
	ledger := &recordingLedger{}
	obj, err := (Publisher{Backend: mem, Store: ledger}).PublishEvidence(context.Background(), bytes.NewReader(body), int64(len(body)), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if obj.Digest != digest || len(ledger.rows) != 1 || ledger.rows[0].Kind != blobstore.KindEvidence || ledger.rows[0].Digest != digest {
		t.Fatalf("evidence obj=%+v row=%+v", obj, ledger.rows)
	}
	ledger.err = errors.New("ledger down")
	if _, err := (Publisher{Backend: mem, Store: ledger}).PublishEvidence(context.Background(), bytes.NewReader(body), int64(len(body)), "text/plain"); err == nil {
		t.Fatal("expected ledger error")
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
