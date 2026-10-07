package main

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type catalogManifestPublishOperatorRequest struct {
	Manifest         *appcatalog.Manifest `json:"manifest"`
	ConfirmPackageID *string              `json:"confirm_package_id,omitempty"`
	ConfirmVersion   *string              `json:"confirm_version,omitempty"`
	PreviewDigest    *string              `json:"preview_digest,omitempty"`
	Reason           *string              `json:"reason"`
}

type standardCatalogManifestPreviewOperatorRequest struct {
	ArtifactSHA256     *string `json:"artifact_sha256"`
	NodeRuntimeVersion *string `json:"node_runtime_version"`
}

type machineProfilePublishOperatorRequest struct {
	Profile          *appcatalog.MachineProfile `json:"profile"`
	ConfirmProfileID *string                    `json:"confirm_profile_id,omitempty"`
	ConfirmRevision  *int64                     `json:"confirm_revision,omitempty"`
	PreviewDigest    *string                    `json:"preview_digest,omitempty"`
	Reason           *string                    `json:"reason"`
}

type machineProfilePreviewOperatorRequest struct {
	Profile *appcatalog.MachineProfile `json:"profile"`
}

type machineProfileAssignmentPreviewOperatorRequest struct {
	ProfileID       *string `json:"profile_id"`
	ProfileRevision *int64  `json:"profile_revision"`
}

type machineProfileAssignmentOperatorRequest struct {
	ProfileID          *string `json:"profile_id"`
	ProfileRevision    *int64  `json:"profile_revision"`
	ConfirmDisplayName *string `json:"confirm_display_name"`
	PreviewDigest      *string `json:"preview_digest"`
	Reason             *string `json:"reason"`
}

func (h *hub) handleListOperatorCatalogManifests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorCatalogManifestListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListCatalogManifests(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidCatalogRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "catalog manifest filter 或 cursor 不合法")
			return
		}
		log.Printf("failed to read catalog manifests: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 catalog manifests 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorStandardCatalogManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if rejection := validateCatalogPreviewTransport(r); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body standardCatalogManifestPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.ArtifactSHA256 == nil || body.NodeRuntimeVersion == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "standard catalog preview 必須明列 artifact_sha256 與 node_runtime_version")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).PreviewStandardCatalogManifest(r.Context(),
		operator.StandardCatalogManifestPreviewRequest{
			ArtifactSHA256: *body.ArtifactSHA256, NodeRuntimeVersion: *body.NodeRuntimeVersion,
		})
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "預覽 standard catalog manifest")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePublishOperatorCatalogManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if rejection := validateCatalogMutationTransport(r); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditCatalogManifest,
			"catalog manifest request", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body catalogManifestPublishOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditCatalogManifest,
			"catalog manifest request", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Manifest == nil || body.Reason == nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditCatalogManifest,
			"catalog manifest request", http.StatusBadRequest, "BAD_REQUEST",
			"catalog manifest publication 必須明列 manifest 與 reason")
		return
	}
	request := operator.CatalogManifestPublishRequest{
		Manifest: *body.Manifest, Reason: *body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	}
	standardFields := 0
	for _, present := range []bool{body.ConfirmPackageID != nil, body.ConfirmVersion != nil, body.PreviewDigest != nil} {
		if present {
			standardFields++
		}
	}
	if standardFields != 0 && standardFields != 3 {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditCatalogManifest,
			"catalog manifest request", http.StatusBadRequest, "BAD_REQUEST",
			"standard catalog publication 必須明列全部 confirmation 欄位")
		return
	}
	service := operator.NewWithArtifacts(h.store, h.artifactsDir)
	var result operator.CatalogManifestPublishResult
	var err error
	if standardFields == 3 {
		request.ConfirmPackageID, request.ConfirmVersion, request.PreviewDigest =
			*body.ConfirmPackageID, *body.ConfirmVersion, *body.PreviewDigest
		result, err = service.PublishStandardCatalogManifest(r.Context(), request)
	} else {
		result, err = service.PublishCatalogManifest(r.Context(), request)
	}
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "發布 catalog manifest")
		return
	}
	status := http.StatusCreated
	if result.AlreadyPublished || result.Replayed {
		status = http.StatusOK
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}

func (h *hub) handleListOperatorMachineProfiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorMachineProfileListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListMachineProfiles(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidCatalogRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine profile filter 或 cursor 不合法")
			return
		}
		log.Printf("failed to read machine profiles: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 machine profiles 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorMachineProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if rejection := validateCatalogPreviewTransport(r); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body machineProfilePreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Profile == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine profile preview 必須明列 profile")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).PreviewMachineProfile(r.Context(),
		operator.MachineProfilePreviewRequest{Profile: *body.Profile})
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "預覽 machine profile")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePublishOperatorMachineProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if rejection := validateCatalogMutationTransport(r); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfile,
			"machine profile request", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body machineProfilePublishOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfile,
			"machine profile request", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Profile == nil || body.Reason == nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfile,
			"machine profile request", http.StatusBadRequest, "BAD_REQUEST",
			"machine profile publication 必須明列 profile 與 reason")
		return
	}
	request := operator.MachineProfilePublishRequest{
		Profile: *body.Profile, Reason: *body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	}
	reviewFields := 0
	for _, present := range []bool{body.ConfirmProfileID != nil, body.ConfirmRevision != nil, body.PreviewDigest != nil} {
		if present {
			reviewFields++
		}
	}
	if reviewFields != 0 && reviewFields != 3 {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfile,
			"machine profile request", http.StatusBadRequest, "BAD_REQUEST",
			"reviewed machine profile publication 必須明列全部 confirmation 欄位")
		return
	}
	service := operator.NewWithArtifacts(h.store, h.artifactsDir)
	var result operator.MachineProfilePublishResult
	var err error
	if reviewFields == 3 {
		request.ConfirmProfileID, request.ConfirmRevision, request.PreviewDigest =
			*body.ConfirmProfileID, *body.ConfirmRevision, *body.PreviewDigest
		result, err = service.PublishReviewedMachineProfile(r.Context(), request)
	} else {
		result, err = service.PublishMachineProfile(r.Context(), request)
	}
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "發布 machine profile")
		return
	}
	status := http.StatusCreated
	if result.AlreadyPublished || result.Replayed {
		status = http.StatusOK
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}

func (h *hub) handlePreviewOperatorMachineProfileAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if rejection := validateCatalogPreviewTransport(r); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body machineProfileAssignmentPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.ProfileID == nil || body.ProfileRevision == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "profile assignment preview 必須明列 profile_id 與 profile_revision")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).PreviewMachineProfileAssignment(r.Context(),
		operator.MachineProfileAssignmentPreviewRequest{
			MachineID: r.PathValue("id"), ProfileID: *body.ProfileID, ProfileRevision: *body.ProfileRevision,
		})
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "預覽 machine profile assignment")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorMachineProfileAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if rejection := validateCatalogMutationTransport(r); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfileAssign,
			r.PathValue("id"), rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	var body machineProfileAssignmentOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfileAssign,
			r.PathValue("id"), rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.ProfileID == nil || body.ProfileRevision == nil || body.ConfirmDisplayName == nil ||
		body.PreviewDigest == nil || body.Reason == nil {
		h.rejectOperatorCatalogTransport(w, r, actor, store.AuditMachineProfileAssign,
			r.PathValue("id"), http.StatusBadRequest, "BAD_REQUEST",
			"profile assignment 必須明列所有欄位")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).AssignMachineProfile(r.Context(),
		operator.MachineProfileAssignmentRequest{
			MachineID: r.PathValue("id"), ProfileID: *body.ProfileID, ProfileRevision: *body.ProfileRevision,
			ConfirmDisplayName: *body.ConfirmDisplayName, PreviewDigest: *body.PreviewDigest,
			Reason: *body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	if err != nil {
		writeOperatorCatalogMutationError(w, err, "指派 machine profile")
		return
	}
	status := http.StatusCreated
	if result.AlreadyAssigned || result.Replayed {
		status = http.StatusOK
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}

func validateCatalogPreviewTransport(r *http.Request) *operatorTransportRejection {
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		return &operatorTransportRejection{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "profile assignment preview 不接受 query parameters"}
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &operatorTransportRejection{Status: http.StatusUnsupportedMediaType, Code: "UNSUPPORTED_MEDIA_TYPE", Detail: "Content-Type 必須是 application/json"}
	}
	return nil
}

func validateCatalogMutationTransport(r *http.Request) *operatorTransportRejection {
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		return &operatorTransportRejection{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "catalog mutation 不接受 query parameters"}
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &operatorTransportRejection{Status: http.StatusUnsupportedMediaType, Code: "UNSUPPORTED_MEDIA_TYPE", Detail: "Content-Type 必須是 application/json"}
	}
	return nil
}

func writeOperatorCatalogMutationError(w http.ResponseWriter, err error, operation string) {
	status, code, detail := operator.HTTPError(err)
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("%s failed: %v", operation, err)
	}
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorCatalogTransport(w http.ResponseWriter, r *http.Request, actor operator.Actor,
	action store.AuditAction, subject string, status int, code, detail string,
) {
	entry := store.AuditEntry{Action: action, Subject: subject}
	if action == store.AuditMachineProfileAssign {
		entry.MachineID = r.PathValue("id")
		if machine, err := h.store.GetMachine(entry.MachineID); err == nil {
			entry.Subject = machine.DisplayName
		}
	}
	h.recordOperatorTransportRejection(r, actor, entry, code, detail)
	writeErr(w, status, code, detail)
}

func parseOperatorCatalogManifestListRequest(r *http.Request) (operator.CatalogManifestListRequest, error) {
	values, err := parseCatalogListQuery(r, map[string]bool{"package_id": true, "kind": true, "limit": true, "cursor": true})
	if err != nil {
		return operator.CatalogManifestListRequest{}, err
	}
	packageID, err := oneCatalogQueryValue(values, "package_id", 128)
	if err != nil {
		return operator.CatalogManifestListRequest{}, err
	}
	kind, err := oneCatalogQueryValue(values, "kind", 16)
	if err != nil {
		return operator.CatalogManifestListRequest{}, err
	}
	cursor, err := oneCatalogQueryValue(values, "cursor", 2048)
	if err != nil {
		return operator.CatalogManifestListRequest{}, err
	}
	limit, err := catalogQueryLimit(values)
	if err != nil {
		return operator.CatalogManifestListRequest{}, err
	}
	return operator.CatalogManifestListRequest{PackageID: packageID, Kind: appcatalog.PackageKind(kind), Limit: limit, Cursor: cursor}, nil
}

func parseOperatorMachineProfileListRequest(r *http.Request) (operator.MachineProfileListRequest, error) {
	values, err := parseCatalogListQuery(r, map[string]bool{"profile_id": true, "limit": true, "cursor": true})
	if err != nil {
		return operator.MachineProfileListRequest{}, err
	}
	profileID, err := oneCatalogQueryValue(values, "profile_id", 128)
	if err != nil {
		return operator.MachineProfileListRequest{}, err
	}
	cursor, err := oneCatalogQueryValue(values, "cursor", 2048)
	if err != nil {
		return operator.MachineProfileListRequest{}, err
	}
	limit, err := catalogQueryLimit(values)
	if err != nil {
		return operator.MachineProfileListRequest{}, err
	}
	return operator.MachineProfileListRequest{ProfileID: profileID, Limit: limit, Cursor: cursor}, nil
}

func parseCatalogListQuery(r *http.Request, allowed map[string]bool) (url.Values, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery && r.URL.RawQuery == "" {
		return nil, errors.New("catalog list query 不合法")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("catalog list query 編碼不合法")
	}
	for key := range values {
		if !allowed[key] {
			return nil, fmt.Errorf("catalog list 不接受 query parameter %q", key)
		}
	}
	return values, nil
}

func oneCatalogQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("catalog list %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}

func catalogQueryLimit(values url.Values) (int, error) {
	raw, present := values["limit"]
	if !present {
		return operator.DefaultCatalogReadLimit, nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
		return 0, errors.New("catalog list limit 必須只出現一次且不可為空")
	}
	limit, err := strconv.Atoi(raw[0])
	if err != nil || limit < 1 || limit > operator.MaxCatalogReadLimit {
		return 0, fmt.Errorf("catalog list limit 必須介於 1 與 %d", operator.MaxCatalogReadLimit)
	}
	return limit, nil
}
