package operator

import (
	"github.com/teddashh/AI-Intune/internal/certificates"
	"time"
)

// CertificateRead is shared by the HTML and operator API adapters. These fields
// include operator-entered free text and must not be presented as secret-scanned.
func (s *Service) CertificateRead() ([]certificates.Status, error) {
	items, err := s.store.Certificates()
	if err != nil {
		return nil, err
	}
	out := []certificates.Status{}
	for _, m := range items {
		out = append(out, certificates.Evaluate(m, time.Now()))
	}
	certificates.Sort(out)
	return out, nil
}
func (s *Service) CertificateSave(id string, revision int64, c certificates.Config, actor Actor) (string, error) {
	return s.store.SaveCertificate(id, revision, c, auditFromActor(actor))
}
func (s *Service) CertificateDelete(id string, revision int64, actor Actor) error {
	return s.store.DeleteCertificate(id, revision, auditFromActor(actor))
}
func (s *Service) CertificateProbe(id string, revision int64, actor Actor) error {
	return s.store.RequestCertificateProbe(id, revision, auditFromActor(actor))
}
