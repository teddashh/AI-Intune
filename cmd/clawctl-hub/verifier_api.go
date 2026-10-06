package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// verifierAuthed is the third producer plane's middleware. It resolves a
// verifier bearer and nothing else: a machine bearer presented here fails, and
// a verifier bearer presented to h.authed fails, because the two credential
// namespaces are separate tables with separate hashes.
//
// ⚠ Every authentication failure returns one opaque message. Never reveal
// whether a credential is unknown, revoked, or belongs to the other plane.
func (h *hub) verifierAuthed(next func(w http.ResponseWriter, r *http.Request, verifier store.Verifier)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, http.StatusUnauthorized, model.ErrUnauthorized, "缺少 bearer token")
			return
		}
		verifier, err := h.store.AuthenticateVerifier(token)
		if err != nil {
			// ⚠ 不要透露憑證不存在、已撤銷，或屬於另一個 plane。
			if errors.Is(err, store.ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, model.ErrUnauthorized, "token 無效")
				return
			}
			h.finishStoreError(w, "authenticate verifier", "", "internal error", err)
			return
		}
		next(w, r, verifier)
	}
}

func (h *hub) handleIndependentVerification(w http.ResponseWriter, r *http.Request, verifier store.Verifier) {
	var req model.IndependentVerificationRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	if req.SchemaVersion != model.IndependentVerificationSchemaVersion {
		writeErr(w, http.StatusBadRequest, model.ErrBadSchemaVersion,
			"這個 Hub 只認得 schema_version=2，收到的是不同版本；請升級 verifier")
		return
	}
	err := h.store.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: req.JobID, RuleID: req.RuleID,
		Command: req.Command, ExitCode: req.ExitCode, StdoutExcerpt: req.StdoutExcerpt,
		StderrExcerpt: req.StderrExcerpt, ObservedDigest: req.ObservedDigest,
		ObservedVersion: req.ObservedVersion,
		Passed:          req.Passed, VerifiedAt: req.VerifiedAt,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidJobEvidence):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
				"rule_id／command／output 大小、observed_digest／observed_version 或 verified_at 不合法")
		case errors.Is(err, store.ErrJobNotFound):
			writeErr(w, http.StatusNotFound, "JOB_NOT_FOUND", "找不到這張工作單")
		case errors.Is(err, store.ErrVerifierNotEligible):
			// The failure domain matching the job's machine is the whole point of
			// this plane, so the caller is told which condition it failed.
			writeErr(w, http.StatusForbidden, "VERIFIER_NOT_ELIGIBLE",
				"這個 verifier 的 failure domain 與這張工作單的機器相同，或憑證已撤銷")
		default:
			h.writeStoreError(w, "寫入獨立驗證證據", "", err)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleVerificationAssignments hands back only what this credential was
// explicitly given. There is no filter parameter and no listing of eligible
// work: eligibility under the separation rule covers the whole fleet minus one
// machine, and that is precisely the enumeration this plane must not offer.
func (h *hub) handleVerificationAssignments(w http.ResponseWriter, r *http.Request, verifier store.Verifier) {
	var req struct{}
	if !decodeJobRequest(w, r, &req) {
		return
	}
	assignments, err := h.store.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		h.writeStoreError(w, "讀取待驗的工作單", "", err)
		return
	}
	items := make([]model.VerificationAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		items = append(items, model.VerificationAssignment{
			AssignmentID: assignment.AssignmentID, JobID: assignment.JobID,
			MachineID: assignment.MachineID, MachineName: assignment.MachineName,
			AssignedAt: assignment.AssignedAt,
		})
	}
	writeJSON(w, http.StatusOK, model.VerificationAssignmentsResponse{
		SchemaVersion: model.SchemaVersion, Assignments: items,
	})
}
