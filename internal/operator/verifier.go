package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

type VerifierPreviewRequest struct {
	Kind          string
	DisplayName   string
	FailureDomain string
	// HubHost is this Hub process's --hub-host value. It is required to
	// register a hub_prober: without it the Hub's own registry row cannot be
	// resolved, and a prober whose domain is not the Hub's machine_id would be
	// allowed to verify the very host it runs on.
	HubHost string
}

type VerifierPreviewResult = store.OperatorVerifierPreviewResult

type VerifierCreateRequest struct {
	Kind           string
	DisplayName    string
	FailureDomain  string
	HubHost        string
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

type VerifierCreateResult = store.OperatorVerifierCreateResult

type VerifierRevocationPreviewRequest struct {
	VerifierID string
}

type VerifierRevocationPreviewResult = store.OperatorVerifierRevocationPreviewResult

type VerifierRevocationRequest struct {
	VerifierID         string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	Actor              Actor
}

type VerifierRevocationResult = store.OperatorVerifierRevocationResult

// VerifierItem is the operator read projection of one registry row. It has no
// credential field by construction, and State is derived so a reader never has
// to interpret a timestamp's presence.
type VerifierItem struct {
	VerifierID    string     `json:"verifier_id"`
	Kind          string     `json:"kind"`
	DisplayName   string     `json:"display_name"`
	FailureDomain string     `json:"failure_domain"`
	State         string     `json:"state"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	Revision      int64      `json:"revision"`
	EvidenceRows  int64      `json:"evidence_rows"`
}

type VerifierListResult struct {
	Verifiers      []VerifierItem `json:"verifiers"`
	Active         int            `json:"active"`
	Revoked        int            `json:"revoked"`
	SeparationRule string         `json:"separation_rule"`
}

type VerifierDetailResult struct {
	Item                 VerifierItem `json:"item"`
	JobsWithEvidence     int64        `json:"jobs_with_evidence"`
	SeparationRule       string       `json:"separation_rule"`
	GrantsDeploymentGate bool         `json:"grants_deployment_gate"`
}

// resolveVerifierHubDomain turns this process's --hub-host into the machine_id
// the Hub's own host occupies. Registering a hub_prober without a hub host is
// refused rather than defaulted: an empty value would make the store accept the
// literal "hub" domain, which on an enrolled Hub host is exactly the
// co-located-endpoint hole.
func (s *Service) resolveVerifierHubDomain(kind, hubHost string) (string, error) {
	if kind != store.VerifierKindHubProber {
		return "", nil
	}
	if strings.TrimSpace(hubHost) == "" {
		return "", &store.OperatorRequestError{
			Code:   store.OperatorCodeVerifierDomainInvalid,
			Detail: "這個 Hub 沒有設定 --hub-host，無法確定 hub_prober 的 failure_domain",
		}
	}
	return s.store.ResolveHubMachineID(hubHost)
}

func (s *Service) PreviewVerifier(req VerifierPreviewRequest) (VerifierPreviewResult, error) {
	hubMachineID, err := s.resolveVerifierHubDomain(req.Kind, req.HubHost)
	if err != nil {
		return VerifierPreviewResult{}, err
	}
	return s.store.PreviewOperatorVerifier(req.Kind, req.DisplayName, req.FailureDomain, hubMachineID)
}

// RegisterVerifier does not re-validate intent before entering Store: a
// matching idempotency key must replay its historical result after policy
// evolves. Store validates only on a cache miss, inside the writer transaction.
func (s *Service) RegisterVerifier(req VerifierCreateRequest) (VerifierCreateResult, error) {
	hubMachineID, err := s.resolveVerifierHubDomain(req.Kind, req.HubHost)
	if err != nil {
		return VerifierCreateResult{}, err
	}
	digest := VerifierSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorVerifier(store.OperatorVerifierCreateRequest{
		Kind: req.Kind, DisplayName: req.DisplayName, FailureDomain: req.FailureDomain,
		HubMachineID: hubMachineID, PreviewDigest: req.PreviewDigest, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditVerifierRegister
	entry.Subject = req.DisplayName
	if entry.Subject == "" {
		entry.Subject = "(沒有名字)"
	}
	entry.Reason = req.Reason
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if err != nil {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			entry.Detail = "idempotency conflict：" + entry.Detail
		}
	}
	recordOperatorFallbackAudit(s.store, entry, result.Audited, err)
	return result, err
}

func (s *Service) PreviewVerifierRevocation(req VerifierRevocationPreviewRequest) (VerifierRevocationPreviewResult, error) {
	return s.store.PreviewOperatorVerifierRevocation(req.VerifierID)
}

func (s *Service) RevokeVerifier(req VerifierRevocationRequest) (VerifierRevocationResult, error) {
	digest := VerifierRevocationSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorVerifierRevocation(store.OperatorVerifierRevocationRequest{
		VerifierID: req.VerifierID, ExpectedRevision: req.ExpectedRevision,
		ConfirmDisplayName: req.ConfirmDisplayName, PreviewDigest: req.PreviewDigest,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditVerifierRevoke
	entry.Subject = req.VerifierID
	entry.Reason = req.Reason
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if result.DisplayName != "" {
		entry.Subject = result.DisplayName
	}
	if err != nil {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			entry.Detail = "idempotency conflict：" + entry.Detail
		}
	} else if result.Replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次撤銷；既有證據與名冊列都還在"
	}
	recordOperatorFallbackAudit(s.store, entry, result.Audited, err)
	return result, err
}

// recordOperatorFallbackAudit writes the per-attempt row only when the store
// did not already commit one atomically.
func recordOperatorFallbackAudit(st *store.Store, entry store.AuditEntry, audited bool, err error) {
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		audited = rejection.Audited
	}
	if audited {
		return
	}
	if auditErr := st.RecordAudit(entry); auditErr != nil {
		log.Printf("operator audit write failed action=%s subject=%s: %v",
			entry.Action, entry.Subject, auditErr)
	}
}

func (s *Service) Verifiers() (VerifierListResult, error) {
	verifiers, err := s.store.ListVerifiers()
	if err != nil {
		return VerifierListResult{}, err
	}
	counts, err := s.store.VerifierEvidenceCounts()
	if err != nil {
		return VerifierListResult{}, err
	}
	result := VerifierListResult{
		Verifiers:      make([]VerifierItem, 0, len(verifiers)),
		SeparationRule: store.OperatorVerifierSeparationRule,
	}
	for _, verifier := range verifiers {
		item := verifierItem(verifier, counts[verifier.VerifierID])
		if item.State == store.VerifierStateRevoked {
			result.Revoked++
		} else {
			result.Active++
		}
		result.Verifiers = append(result.Verifiers, item)
	}
	// Active rows first, then by name: an operator looking for a credential to
	// revoke should not have to scan past retired ones.
	sort.SliceStable(result.Verifiers, func(i, j int) bool {
		if (result.Verifiers[i].State == store.VerifierStateActive) !=
			(result.Verifiers[j].State == store.VerifierStateActive) {
			return result.Verifiers[i].State == store.VerifierStateActive
		}
		return result.Verifiers[i].DisplayName < result.Verifiers[j].DisplayName
	})
	return result, nil
}

func (s *Service) VerifierDetail(verifierID string) (VerifierDetailResult, error) {
	verifier, err := s.store.GetVerifier(verifierID)
	if err != nil {
		if errors.Is(err, store.ErrVerifierNotFound) {
			return VerifierDetailResult{}, &store.OperatorRequestError{
				Code: store.OperatorCodeVerifierNotFound, Detail: "找不到這個 verifier"}
		}
		return VerifierDetailResult{}, err
	}
	counts, err := s.store.VerifierEvidenceCounts()
	if err != nil {
		return VerifierDetailResult{}, err
	}
	jobs, err := s.store.VerifierJobCount(verifierID)
	if err != nil {
		return VerifierDetailResult{}, err
	}
	return VerifierDetailResult{
		Item:                 verifierItem(verifier, counts[verifierID]),
		JobsWithEvidence:     jobs,
		SeparationRule:       store.OperatorVerifierSeparationRule,
		GrantsDeploymentGate: store.VerifierKindGrantsDeploymentGate(verifier.Kind),
	}, nil
}

func verifierItem(verifier store.Verifier, evidenceRows int64) VerifierItem {
	return VerifierItem{
		VerifierID: verifier.VerifierID, Kind: verifier.Kind,
		DisplayName: verifier.DisplayName, FailureDomain: verifier.FailureDomain,
		State: verifier.State(), CreatedAt: verifier.CreatedAt,
		RevokedAt: verifier.RevokedAt, LastSeenAt: verifier.LastSeenAt,
		Revision: verifier.Revision, EvidenceRows: evidenceRows,
	}
}

// VerifierSemanticDigest binds every domain-significant create field, Reason
// included: changing it while reusing a key must conflict rather than replay an
// audit statement the caller did not send. HubHost is deliberately absent — it
// is resolved to a machine_id which is itself bound through FailureDomain.
func VerifierSemanticDigest(req VerifierCreateRequest) string {
	body := struct {
		Kind          string `json:"kind"`
		DisplayName   string `json:"display_name"`
		FailureDomain string `json:"failure_domain"`
		PreviewDigest string `json:"preview_digest"`
		Reason        string `json:"reason"`
	}{req.Kind, req.DisplayName, req.FailureDomain, req.PreviewDigest, req.Reason}
	return semanticDigestOf(body)
}

func VerifierRevocationSemanticDigest(req VerifierRevocationRequest) string {
	body := struct {
		VerifierID         string `json:"verifier_id"`
		ExpectedRevision   *int64 `json:"expected_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
	}{req.VerifierID, req.ExpectedRevision, req.ConfirmDisplayName, req.PreviewDigest, req.Reason}
	return semanticDigestOf(body)
}

func semanticDigestOf(body any) string {
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
