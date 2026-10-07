package operator

import (
	"errors"

	"github.com/teddashh/AI-Intune/internal/store"
)

// KeyedInstallerDownload records delivery metadata only. The enrollment secret
// never enters the audit request or persisted evidence.
type KeyedInstallerDownload struct {
	MachineID   string
	DisplayName string
	Arch        string
	Actor       Actor
}

func (s *Service) RecordKeyedInstallerDownload(req KeyedInstallerDownload) error {
	if req.MachineID == "" || (req.Arch != "amd64" && req.Arch != "arm64") {
		return errors.New("operator: invalid keyed installer download")
	}
	entry := store.AuditEntry{Action: store.AuditKeyedInstaller, MachineID: req.MachineID,
		Subject: req.DisplayName, OK: true, Detail: "keyed installer downloaded; arch=" + req.Arch}
	ApplyActor(&entry, req.Actor)
	return s.store.RecordAudit(entry)
}
