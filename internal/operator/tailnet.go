package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type TailnetOverview struct {
	Available          bool                      `json:"available"`
	Unavailable        string                    `json:"unavailable,omitempty"`
	ObservedAt         time.Time                 `json:"observed_at,omitempty"`
	PeerCount          int                       `json:"peer_count"`
	Unenrolled         []tailnet.Peer            `json:"unenrolled"`
	OnlineButSilent    []TailnetMachinePeer      `json:"online_but_silent"`
	RetiredButOnline   []tailnet.RetiredPeer     `json:"retired_but_online"`
	Ignored            []store.TailnetPeerIgnore `json:"ignored"`
	LegacyIgnoredCount int                       `json:"legacy_ignored_count"`
}

type TailnetMachinePeer struct {
	MachineID string       `json:"machine_id"`
	Peer      tailnet.Peer `json:"peer"`
}

type TailnetPeerIgnorePreviewRequest struct {
	PeerID    string    `json:"peer_id"`
	Action    string    `json:"action"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Reason    string    `json:"reason"`
}

type TailnetPeerIgnorePreview struct {
	PeerID           string    `json:"peer_id"`
	Hostname         string    `json:"hostname"`
	Action           string    `json:"action"`
	ObservedAt       time.Time `json:"observed_at"`
	CurrentlyIgnored bool      `json:"currently_ignored"`
	IgnoredAfter     bool      `json:"ignored_after"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	ExpectedRevision int64     `json:"expected_revision"`
	Reason           string    `json:"reason"`
	PreviewDigest    string    `json:"preview_digest"`
}

type TailnetPeerIgnoreApplyRequest struct {
	TailnetPeerIgnorePreviewRequest
	ExpectedRevision int64
	ConfirmHostname  string
	PreviewDigest    string
	IdempotencyKey   string
	Actor            Actor
}

type TailnetPeerIgnoreResult = store.OperatorTailnetPeerIgnoreResult

func (s *Service) Tailnet(ctx context.Context, now time.Time) (TailnetOverview, error) {
	if s.tailnetSource == nil {
		return TailnetOverview{}, errors.New("operator: tailnet source is not configured")
	}
	active, err := s.store.TailnetPeerIgnores(now)
	if err != nil {
		return TailnetOverview{}, err
	}
	legacyCount, err := s.store.TailnetLegacyIgnoredCount()
	if err != nil {
		return TailnetOverview{}, err
	}
	status := s.tailnetSource.Get(ctx)
	if !status.Available {
		result := emptyTailnetOverview()
		result.Unavailable = status.Unavailable
		result.Ignored = active
		result.LegacyIgnoredCount = legacyCount
		return result, nil
	}
	roster, err := s.store.RosterForTailnet()
	if err != nil {
		return TailnetOverview{}, err
	}
	ignoredKeys, err := s.store.IgnoredPeersAt(now)
	if err != nil {
		return TailnetOverview{}, err
	}
	ov, err := s.store.Overview(now)
	if err != nil {
		return TailnetOverview{}, err
	}
	silent := map[string]bool{}
	for _, machine := range ov.Machines {
		if machine.State == state.Unreachable || machine.State == state.NeverReported {
			silent[machine.MachineID] = true
		}
	}
	reconciled := tailnet.Reconcile(status, roster, ignoredKeys, silent)
	machinePeers := make([]TailnetMachinePeer, 0, len(reconciled.OnlineButSilent))
	for id, peer := range reconciled.OnlineButSilent {
		machinePeers = append(machinePeers, TailnetMachinePeer{MachineID: id, Peer: peer})
	}
	sort.Slice(machinePeers, func(i, j int) bool { return machinePeers[i].MachineID < machinePeers[j].MachineID })
	result := TailnetOverview{
		Available: true, ObservedAt: status.ObservedAt, PeerCount: 1 + len(status.Peers),
		Unenrolled: reconciled.Unenrolled, OnlineButSilent: machinePeers,
		RetiredButOnline: reconciled.RetiredButOnline, Ignored: active,
		LegacyIgnoredCount: legacyCount,
	}
	if result.Unenrolled == nil {
		result.Unenrolled = []tailnet.Peer{}
	}
	if result.RetiredButOnline == nil {
		result.RetiredButOnline = []tailnet.RetiredPeer{}
	}
	if result.Ignored == nil {
		result.Ignored = []store.TailnetPeerIgnore{}
	}
	return result, nil
}

func emptyTailnetOverview() TailnetOverview {
	return TailnetOverview{
		Unenrolled:       []tailnet.Peer{},
		OnlineButSilent:  []TailnetMachinePeer{},
		RetiredButOnline: []tailnet.RetiredPeer{},
		Ignored:          []store.TailnetPeerIgnore{},
	}
}

func (s *Service) PreviewTailnetPeerIgnore(ctx context.Context, req TailnetPeerIgnorePreviewRequest, now time.Time) (TailnetPeerIgnorePreview, error) {
	peerID, reason := strings.TrimSpace(req.PeerID), strings.TrimSpace(req.Reason)
	if peerID == "" || len(peerID) > 128 || reason == "" || len(reason) > 500 || (req.Action != "ignore" && req.Action != "unignore") {
		return TailnetPeerIgnorePreview{}, &store.OperatorRequestError{Code: store.OperatorCodeTailnetPeerInvalid, Detail: "peer、action 與 reason 必須完整且合法"}
	}
	if req.Action == "ignore" && (!req.ExpiresAt.After(now.UTC()) || req.ExpiresAt.After(now.UTC().Add(366*24*time.Hour))) {
		return TailnetPeerIgnorePreview{}, &store.OperatorRequestError{Code: store.OperatorCodeTailnetPeerInvalid, Detail: "expires_at 必須在現在之後且不超過 366 天"}
	}
	current, found, err := s.store.TailnetPeerIgnore(peerID, now)
	if err != nil {
		return TailnetPeerIgnorePreview{}, err
	}
	currentlyIgnored := found && current.Active
	if req.Action == "unignore" && !currentlyIgnored {
		return TailnetPeerIgnorePreview{}, &store.OperatorRequestError{Code: store.OperatorCodeTailnetPeerInvalid, Detail: "這台 peer 目前沒有有效的忽略規則"}
	}
	var status tailnet.Status
	if s.tailnetSource != nil {
		status = s.tailnetSource.Get(ctx)
	}
	peer, ok := findTailnetPeer(status, peerID)
	if !ok && req.Action == "unignore" {
		peer = tailnet.Peer{StableID: peerID, Hostname: current.Hostname}
		status.ObservedAt = current.UpdatedAt
		ok = true
	}
	if !ok {
		if !status.Available {
			detail := status.Unavailable
			if detail == "" {
				detail = "Tailnet 清單無法取得"
			}
			return TailnetPeerIgnorePreview{}, &store.OperatorRequestError{Code: store.OperatorCodeTailnetSourceUnavailable, Detail: detail}
		}
		return TailnetPeerIgnorePreview{}, &store.OperatorRequestError{Code: store.OperatorCodeTailnetPeerNotFound, Detail: "目前 Tailnet 清單找不到這台 peer"}
	}
	revision := int64(0)
	if found {
		revision = current.Revision
	}
	preview := TailnetPeerIgnorePreview{
		PeerID: peerID, Hostname: peer.Hostname, Action: req.Action, ObservedAt: status.ObservedAt,
		CurrentlyIgnored: currentlyIgnored, IgnoredAfter: req.Action == "ignore", ExpiresAt: req.ExpiresAt.UTC(),
		ExpectedRevision: revision, Reason: reason,
	}
	if req.Action == "unignore" {
		preview.ExpiresAt = time.Time{}
	}
	preview.PreviewDigest = tailnetPeerIgnorePreviewDigest(preview)
	return preview, nil
}

func (s *Service) ApplyTailnetPeerIgnore(ctx context.Context, req TailnetPeerIgnoreApplyRequest, now time.Time) (TailnetPeerIgnoreResult, error) {
	digest := TailnetPeerIgnoreSemanticDigest(req)
	current, previewErr := s.PreviewTailnetPeerIgnore(ctx, req.TailnetPeerIgnorePreviewRequest, now)
	currentDigest := ""
	hostname := strings.TrimSpace(req.ConfirmHostname)
	if previewErr == nil {
		currentDigest = current.PreviewDigest
		hostname = current.Hostname
	}
	audit := auditFromActor(req.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, digest
	createdBy := firstNonEmpty(req.Actor.AuthSubject, req.Actor.WhoUser, req.Actor.WhoNode, req.Actor.SourceAddr, "operator")
	result, err := s.store.ApplyOperatorTailnetPeerIgnore(store.OperatorTailnetPeerIgnoreRequest{
		PeerID: strings.TrimSpace(req.PeerID), Hostname: hostname, Action: req.Action,
		ExpiresAt: req.ExpiresAt.UTC(), ExpectedRevision: req.ExpectedRevision,
		ConfirmHostname: req.ConfirmHostname, PreviewDigest: req.PreviewDigest,
		CurrentPreviewDigest: currentDigest, Reason: req.Reason, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, CreatedBy: createdBy, Audit: audit,
	})
	if err != nil {
		var rejection *store.OperatorRequestError
		alreadyAudited := result.Audited
		if errors.As(err, &rejection) {
			alreadyAudited = rejection.Audited
		}
		if !alreadyAudited {
			audit.Action, audit.Subject, audit.OK, audit.Detail = store.AuditTailnetPeerIgnore, req.PeerID, false, err.Error()
			if auditErr := s.store.RecordAudit(audit); auditErr != nil {
				log.Printf("operator tailnet audit failed: %v", auditErr)
			}
		}
	}
	return result, err
}

func findTailnetPeer(status tailnet.Status, peerID string) (tailnet.Peer, bool) {
	for _, peer := range append([]tailnet.Peer{status.Self}, status.Peers...) {
		if peer.StableID == peerID {
			return peer, true
		}
	}
	return tailnet.Peer{}, false
}

func tailnetPeerIgnorePreviewDigest(preview TailnetPeerIgnorePreview) string {
	copy := preview
	copy.PreviewDigest = ""
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TailnetPeerIgnoreSemanticDigest(req TailnetPeerIgnoreApplyRequest) string {
	body := struct {
		PeerID           string    `json:"peer_id"`
		Action           string    `json:"action"`
		ExpiresAt        time.Time `json:"expires_at,omitempty"`
		ExpectedRevision int64     `json:"expected_revision"`
		ConfirmHostname  string    `json:"confirm_hostname"`
		PreviewDigest    string    `json:"preview_digest"`
		Reason           string    `json:"reason"`
	}{req.PeerID, req.Action, req.ExpiresAt.UTC(), req.ExpectedRevision, req.ConfirmHostname, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
