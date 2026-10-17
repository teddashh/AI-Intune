package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/certificates"
)

func (s *Store) Certificates() ([]certificates.Monitor, error) {
	rows, err := s.rdb.Query(`SELECT id,revision,config,result FROM certificate_monitors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []certificates.Monitor{}
	for rows.Next() {
		var m certificates.Monitor
		var c, r string
		if err = rows.Scan(&m.ID, &m.Revision, &c, &r); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(c), &m.Config); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(r), &m.Result); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// certificateChange commits configuration and its optional operator audit together.
func (s *Store) certificateChange(id, action string, audits []AuditEntry, query string, args ...any) error {
	tx, err := s.beginWrite(context.Background(), "certificate_"+action)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	for _, a := range audits {
		a.Action = AuditAction("certificate_" + action)
		a.Subject = id
		a.OK = true
		if err = s.recordAudit(tx, a); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) SaveCertificate(id string, revision int64, c certificates.Config, audits ...AuditEntry) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(c)
	if id == "" {
		id = newID()
		err := s.certificateChange(id, "create", audits, `INSERT INTO certificate_monitors(id,revision,config) SELECT ?,1,? WHERE (SELECT COUNT(*) FROM certificate_monitors)<200`, id, string(raw))
		return id, err
	}
	err := s.certificateChange(id, "update", audits, `UPDATE certificate_monitors SET config=?,revision=revision+1,result='{}' WHERE id=? AND revision=?`, string(raw), id, revision)
	return id, err
}
func (s *Store) DeleteCertificate(id string, revision int64, audits ...AuditEntry) error {
	return s.certificateChange(id, "delete", audits, `DELETE FROM certificate_monitors WHERE id=? AND revision=?`, id, revision)
}
func (s *Store) RecordCertificateProbe(m certificates.Monitor, r certificates.Result) error {
	if r.ObservedAt.IsZero() {
		old := m.Result
		old.AttemptedAt = r.AttemptedAt
		old.ProbeError = r.ProbeError
		r = old
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.execWrite(context.Background(), "certificate_probe_result", `UPDATE certificate_monitors SET result=? WHERE id=? AND revision=?`, string(raw), m.ID, m.Revision)
	return err
}
func (s *Store) RequestCertificateProbe(id string, revision int64, audits ...AuditEntry) error {
	return s.certificateChange(id, "probe", audits, `UPDATE certificate_monitors SET result=json_set(result,'$.attempted_at','0001-01-01T00:00:00Z'),revision=revision+1 WHERE id=? AND revision=?`, id, revision)
}
