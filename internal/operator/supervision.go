package operator

import (
	"context"
	"regexp"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

type SupervisionFilter = store.SupervisionFilter
type JobsSummary = store.JobsSummary
type ApprovalsSummary = store.ApprovalsSummary
type HubStatus = store.HubStatus

var ErrInvalidSupervision = store.ErrInvalidSupervision

func ValidateSupervisionFilter(f SupervisionFilter) error {
	if f.Window != "" && f.Window != "24h" && f.Window != "7d" {
		return ErrInvalidSupervision
	}
	for _, v := range []string{f.Kind, f.MachineID} {
		if v != "" && !validAuditFilterText(v, 256) {
			return ErrInvalidSupervision
		}
	}
	return nil
}

func (s *Service) JobsSummary(ctx context.Context, f SupervisionFilter, now time.Time) (JobsSummary, error) {
	if err := ValidateSupervisionFilter(f); err != nil {
		return JobsSummary{}, err
	}
	r, err := s.store.JobsSummaryContext(ctx, f, now)
	if err != nil {
		return r, err
	}
	for i := range r.Items {
		a := &r.Items[i]
		a.Kind, _, _ = supervisionText(a.Kind, 256)
		if a.ScriptID != nil {
			v, _, _ := supervisionText(*a.ScriptID, 256)
			a.ScriptID = &v
		}
		if a.MachineID != nil {
			v, _, _ := supervisionText(*a.MachineID, 256)
			a.MachineID = &v
		}
	}
	r.Filters.Kind, _, _ = supervisionText(r.Filters.Kind, 256)
	r.Filters.MachineID, _, _ = supervisionText(r.Filters.MachineID, 256)
	return r, nil
}

func (s *Service) ApprovalsSummary(ctx context.Context, window string, now time.Time) (ApprovalsSummary, error) {
	if err := ValidateSupervisionFilter(SupervisionFilter{Window: window}); err != nil {
		return ApprovalsSummary{}, err
	}
	return s.store.ApprovalsSummaryContext(ctx, window, now)
}

func (s *Service) HubStatus(ctx context.Context, buildVersion string, startedAt, now time.Time) (HubStatus, error) {
	r, err := s.store.HubStatusContext(ctx, now)
	if err != nil {
		return r, err
	}
	r.Version, _, _ = supervisionText(buildVersion, 128)
	if !startedAt.IsZero() && !startedAt.After(now) {
		v := now.Sub(startedAt).Seconds()
		r.UptimeSeconds = &v
	}
	r.DBSizeBytes, r.WALSizeBytes, r.StorageUnavailableReason = s.store.SupervisionStorageSizes(ctx)
	return r, nil
}

// Labels are the only stored text disclosed by supervision. Scrub common
// credential shapes before capping, so truncation cannot conceal their prefix.
var supervisionCredentialShape = regexp.MustCompile(`(?i)(?:bearer\s+|(?:token|password|secret|api[_-]?key)\s*[:=]\s*|(?:sk-|ghp_|github_pat_|AKIA|tskey-))[^\s,;]+`)

func supervisionText(value string, limit int) (string, bool, []string) {
	return scrubBoundedText(supervisionCredentialShape.ReplaceAllString(value, "[redacted]"), limit)
}
