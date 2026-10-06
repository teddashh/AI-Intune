// Package operator owns human/operator initiated control-plane use cases.
// HTTP, HTML forms, and local CLI entry points all call this service; machine
// bearer authentication deliberately remains outside this package.
package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type TailnetSource interface {
	Get(context.Context) tailnet.Status
}

type Service struct {
	store                 *store.Store
	artifactsDir          string
	artifactFetcher       artifactFetchBackend
	tailnetSource         TailnetSource
	restoreDrill          *restoredrill.Runner
	terminalSessionCloser func([]string)
	blobPublisher         ArtifactBlobPublisher
}

func (s *Service) ConfigureRestoreDrill(runner restoredrill.Runner) {
	s.restoreDrill = &runner
}

func New(st *store.Store) *Service { return &Service{store: st} }

// NewWithArtifacts adds the Hub-owned artifact catalog used by deployment
// preview/apply. New remains valid for read-only use cases and older adapters;
// a deployment preview on a Service without a catalog fails closed.
func NewWithArtifacts(st *store.Store, artifactsDir string) *Service {
	service := &Service{store: st, artifactsDir: artifactsDir}
	if fetcher, err := artifact.NewFetcher(artifactsDir); err == nil {
		service.artifactFetcher = fetcher
	}
	return service
}

func NewWithTailnet(st *store.Store, source TailnetSource) *Service {
	return &Service{store: st, tailnetSource: source}
}

func NewControlPlane(st *store.Store, artifactsDir string, source TailnetSource) *Service {
	service := NewWithArtifacts(st, artifactsDir)
	service.tailnetSource = source
	return service
}

// SetTerminalSessionCloser records how this service ends live terminal routes.
// Every constructor leaves it unset. The Hub sets it once on the
// process-lifetime service. Direct-database commands never set it.
func (s *Service) SetTerminalSessionCloser(close func(sessionIDs []string)) {
	s.terminalSessionCloser = close
}

// ArtifactBlobPublisher stores Hub-measured artifact bytes outside the local
// artifacts directory. A nil publisher keeps local files only.
type ArtifactBlobPublisher interface {
	PublishArtifact(ctx context.Context, artifactsDir, digest string, size int64) error
}

// SetArtifactBlobPublisher attaches the optional remote blob mirror. The Hub
// calls it once on the process-lifetime service. Tests leave it nil.
func (s *Service) SetArtifactBlobPublisher(publisher ArtifactBlobPublisher) {
	if s == nil {
		return
	}
	s.blobPublisher = publisher
}

// endClosedTerminalSessions runs only after the store apply has returned.
// The ledger rows are already committed. A replay carries no session ids.
func (s *Service) endClosedTerminalSessions(err error, sessionIDs []string) {
	if err != nil || s.terminalSessionCloser == nil || len(sessionIDs) == 0 {
		return
	}
	s.terminalSessionCloser(sessionIDs)
}

const (
	SourceKindWeb         = "web"
	SourceKindOperatorAPI = "operator-api"
	SourceKindDirectDBCLI = "direct-db-cli"
)

// Actor keeps transport provenance separate from a verified operator principal.
// HTTP adapters get WhoNode/WhoUser and Auth* from the same LocalAPI decision;
// direct-DB CLI intentionally has no such evidence and leaves Auth* empty.
type Actor struct {
	SourceAddr     string
	WhoNode        string
	WhoUser        string
	WhoUnavailable string
	UserAgent      string
	AuthSubject    string
	AuthNodeID     string
	AuthCapability string
	AuthMethod     string
	AuthDecision   string
	SourceKind     string
}

type MachineChannelRequest struct {
	MachineID          string
	Channel            string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	IdempotencyKey     string
	Actor              Actor
}

type MachineChannelResult = store.OperatorMachineChannelResult

type EnrollTokenPreviewRequest struct {
	DisplayName string
	TTLSeconds  int64
}

type EnrollTokenPreviewResult = store.OperatorEnrollTokenPreviewResult

type EnrollTokenCreateRequest struct {
	DisplayName    string
	TTLSeconds     int64
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

type EnrollTokenCreateResult = store.OperatorEnrollTokenCreateResult

type EnrollTokenStatusResult = store.OperatorPendingEnrollTokenResult

type EnrollTokenRevocationPreviewRequest struct {
	MachineID string
}

type EnrollTokenRevocationPreviewResult = store.OperatorEnrollTokenRevocationPreviewResult

type EnrollTokenRevocationRequest struct {
	MachineID      string
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

type EnrollTokenRevocationResult = store.OperatorEnrollTokenRevocationResult

func (s *Service) PreviewEnrollToken(req EnrollTokenPreviewRequest) (EnrollTokenPreviewResult, error) {
	return s.store.PreviewOperatorEnrollToken(req.DisplayName, req.TTLSeconds)
}

func (s *Service) PendingEnrollToken(machineID string) (EnrollTokenStatusResult, error) {
	return s.store.OperatorPendingEnrollToken(machineID)
}

func (s *Service) PreviewEnrollTokenRevocation(req EnrollTokenRevocationPreviewRequest) (EnrollTokenRevocationPreviewResult, error) {
	return s.store.PreviewOperatorEnrollTokenRevocation(req.MachineID)
}

func (s *Service) RevokeEnrollToken(req EnrollTokenRevocationRequest) (EnrollTokenRevocationResult, error) {
	digest := EnrollTokenRevocationSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorEnrollTokenRevocation(store.OperatorEnrollTokenRevocationRequest{
		MachineID: req.MachineID, PreviewDigest: req.PreviewDigest, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditRevokeToken
	entry.MachineID = req.MachineID
	entry.Subject = req.MachineID
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
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次刪除 pending enrollment ticket；active agent credential 未受影響"
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator audit 寫入失敗 action=%s subject=%s: %v",
				entry.Action, entry.Subject, auditErr)
		}
	}
	return result, err
}

// CreateEnrollToken intentionally does not run current preview/TTL validation
// before entering Store. A matching idempotency key must be able to replay its
// historical result after policy evolves; Store validates only on a cache
// miss, under the same writer transaction as issuance and audit.
func (s *Service) CreateEnrollToken(req EnrollTokenCreateRequest) (EnrollTokenCreateResult, error) {
	digest := EnrollTokenSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorEnrollToken(store.OperatorEnrollTokenCreateRequest{
		DisplayName: req.DisplayName, TTLSeconds: req.TTLSeconds,
		PreviewDigest: req.PreviewDigest, Reason: req.Reason, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditEnrollToken
	entry.Subject = req.DisplayName
	if entry.Subject == "" {
		entry.Subject = "(沒有名字)"
	}
	entry.Reason = req.Reason
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	entry.Detail = ""
	if result.MachineID != "" {
		entry.MachineID = result.MachineID
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
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator audit 寫入失敗 action=%s subject=%s: %v",
				entry.Action, entry.Subject, auditErr)
		}
	}
	return result, err
}

func (s *Service) MachineChannel(machineID string) (MachineChannelResult, error) {
	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return MachineChannelResult{}, err
	}
	return MachineChannelResult{
		MachineID: m.MachineID, DisplayName: m.DisplayName,
		Channel: m.Channel, PreviousChannel: m.Channel, Revision: m.ChannelRevision,
	}, nil
}

func (s *Service) ChangeMachineChannel(req MachineChannelRequest) (MachineChannelResult, error) {
	// Digest the operator literal before any internal representation mapping.
	// Missing/empty is invalid and must never alias explicit "none" on replay.
	digest := SemanticDigest(req)

	// This read is only an audit fallback. The transaction below re-reads and
	// guards every authoritative field; no decision is based on this snapshot.
	before, _ := s.store.GetMachine(req.MachineID)
	auditBase := auditFromActor(req.Actor)
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorMachineChannel(store.OperatorMachineChannelRequest{
		MachineID:          req.MachineID,
		Channel:            req.Channel,
		ExpectedRevision:   req.ExpectedRevision,
		ConfirmDisplayName: req.ConfirmDisplayName,
		IdempotencyKey:     req.IdempotencyKey,
		RequestDigest:      digest,
		Audit:              auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditMachineChannel
	entry.MachineID = req.MachineID
	entry.Subject = req.MachineID
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if before.DisplayName != "" {
		entry.Subject = before.DisplayName
		entry.Reason = channelLabel(before.Channel) + " → " + channelLabel(req.Channel)
	}
	if err == nil {
		entry.Subject = result.DisplayName
		entry.Reason = channelLabel(result.PreviousChannel) + " → " + channelLabel(result.Channel)
		if result.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次改 state 或 revision"
		}
	} else {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			entry.Detail = "idempotency conflict：" + entry.Detail
		}
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			// Replays already have their original atomic evidence; this per-attempt
			// row (and requests rejected before a key exists) remains best-effort.
			log.Printf("operator audit 寫入失敗 action=%s subject=%s: %v",
				entry.Action, entry.Subject, auditErr)
		}
	}
	return result, err
}

func auditFromActor(actor Actor) store.AuditEntry {
	return store.AuditEntry{
		SourceAddr: actor.SourceAddr, WhoNode: actor.WhoNode, WhoUser: actor.WhoUser,
		WhoUnavailable: actor.WhoUnavailable, UserAgent: actor.UserAgent,
		AuthSubject: actor.AuthSubject, AuthNodeID: actor.AuthNodeID,
		AuthCapability: actor.AuthCapability, AuthMethod: actor.AuthMethod,
		AuthDecision: actor.AuthDecision, SourceKind: actor.SourceKind,
	}
}

// SemanticDigest gives non-HTTP adapters the same body-conflict semantics
// without pretending that they had JSON bytes on the wire.
func SemanticDigest(req MachineChannelRequest) string {
	body := struct {
		Channel            string `json:"channel"`
		ExpectedRevision   *int64 `json:"expected_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
	}{req.Channel, req.ExpectedRevision, req.ConfirmDisplayName}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EnrollTokenSemanticDigest binds every domain-significant create field. In
// particular Reason is not mere presentation decoration: changing it while
// reusing a key must conflict rather than replay an audit statement the caller
// did not send.
func EnrollTokenSemanticDigest(req EnrollTokenCreateRequest) string {
	body := struct {
		DisplayName   string `json:"display_name"`
		TTLSeconds    int64  `json:"ttl_seconds"`
		PreviewDigest string `json:"preview_digest"`
		Reason        string `json:"reason"`
	}{req.DisplayName, req.TTLSeconds, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EnrollTokenRevocationSemanticDigest binds the path target and every
// domain-significant body field. Actor and transport metadata are evidence,
// not part of the operation's idempotent meaning.
func EnrollTokenRevocationSemanticDigest(req EnrollTokenRevocationRequest) string {
	body := struct {
		MachineID     string `json:"machine_id"`
		PreviewDigest string `json:"preview_digest"`
		Reason        string `json:"reason"`
	}{req.MachineID, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func NewIdempotencyKey(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("operator: generate idempotency key: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

func channelLabel(channel string) string {
	if channel == "" {
		return "未指派"
	}
	return channel
}
