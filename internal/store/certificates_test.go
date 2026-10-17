package store

import (
	"github.com/teddashh/AI-Intune/internal/certificates"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateCRUDRevisionAndFailedProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := certificates.Config{Endpoint: "example.test:443", Expected: []string{"example.test"}, Owner: "Example team"}
	id, err := s.SaveCertificate("", 0, c)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := s.Certificates()
	if len(items) != 1 || items[0].ID != id || len(items[0].Days) != 3 {
		t.Fatal(items)
	}
	m := items[0]
	now := time.Now().UTC()
	r := certificates.Result{AttemptedAt: now, ObservedAt: now, NotAfter: now.Add(24 * time.Hour), SANs: []string{"example.test"}}
	if err = s.RecordCertificateProbe(m, r); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Certificates()
	m = items[0]
	if err = s.RecordCertificateProbe(m, certificates.Result{AttemptedAt: now.Add(time.Hour), ProbeError: "TLS connection or handshake failed"}); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Certificates()
	if !items[0].Result.NotAfter.Equal(r.NotAfter) || items[0].Result.ProbeError == "" {
		t.Fatal(items)
	}
	if _, err = s.SaveCertificate(id, m.Revision, c); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCertificateProbe(m, r); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Certificates()
	if !items[0].Result.ObservedAt.IsZero() {
		t.Fatal("old probe overwrote edit")
	}
	if s.DeleteCertificate(id, m.Revision) == nil {
		t.Fatal("stale delete accepted")
	}
	if err = s.RequestCertificateProbe(id, items[0].Revision); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Certificates()
	if err = s.DeleteCertificate(id, items[0].Revision); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Certificates()
	if len(items) != 0 {
		t.Fatal(items)
	}
}
