package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// artifactWebOperator is a narrow HTML adapter port. Production installs the
// same *operator.Service used by every other human-control adapter; the
// interface exists only so Web tests do not make requests to the public npm
// registry.
type artifactWebOperator interface {
	ListArtifacts(operator.ArtifactListRequest, time.Time) (operator.ArtifactListResult, error)
	ArtifactDetail(context.Context, string, time.Time) (operator.ArtifactDetailResult, error)
	PreviewArtifactFetch(context.Context, operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error)
	ApplyArtifactFetch(context.Context, operator.ArtifactFetchApplyRequest) (operator.ArtifactFetchApplyResult, error)
	ArtifactFetchOperation(string) (store.ArtifactFetchOperation, error)
	ArtifactFetchOperations(store.ArtifactFetchListRequest) (store.ArtifactFetchListResult, error)
}

type catalogWebOperator interface {
	ListCatalogManifests(operator.CatalogManifestListRequest, time.Time) (operator.CatalogManifestListResult, error)
	PreviewStandardCatalogManifest(context.Context, operator.StandardCatalogManifestPreviewRequest) (operator.StandardCatalogManifestPreviewResult, error)
	PublishStandardCatalogManifest(context.Context, operator.CatalogManifestPublishRequest) (operator.CatalogManifestPublishResult, error)
	PreviewMachineProfile(context.Context, operator.MachineProfilePreviewRequest) (operator.MachineProfilePreviewResult, error)
	PublishReviewedMachineProfile(context.Context, operator.MachineProfilePublishRequest) (operator.MachineProfilePublishResult, error)
	PreviewMachineProfileAssignment(context.Context, operator.MachineProfileAssignmentPreviewRequest) (operator.MachineProfileAssignmentPreviewResult, error)
	AssignMachineProfile(context.Context, operator.MachineProfileAssignmentRequest) (operator.MachineProfileAssignmentResult, error)
}

const artifactFetchWebFormMaxBytes int64 = 64 << 10

type appsSurfacePage struct {
	View              string
	CurrentLabel      string
	Overview          *appsOverview
	OpenClaw          []appSurfaceMachine
	Artifacts         *operator.ArtifactListResult
	CatalogConfigured bool
	Operations        *artifactFetchOperationsView
	Store             *catalogStoreView
	Profiles          *catalogProfilesView
	Assignments       *catalogAssignmentsView
}

type appsOverview struct {
	ActiveMachines   int
	ObservedInstalls int
	ManagedReleases  int
	KnownVersions    int
}

// appSurfaceMachine is deliberately narrower than store.Machine and install
// facts. In particular, no executable or release-directory path reaches the
// template data graph.
type appSurfaceMachine struct {
	MachineID      string
	DisplayName    string
	Channel        string
	Version        string
	NodeVersion    string
	ReleaseKnown   bool
	ManagedRelease bool
	SucceededJobID string
	SucceededJobAt *time.Time
}

type artifactDetailPage struct {
	Item        operator.ArtifactSummary
	EvaluatedAt time.Time
}

// artifactFetchOperationView is an explicit browser allowlist. The Store DTO
// is already safe for operator JSON, but HTML has an even smaller need: origin,
// upstream integrity, preview/identity digests, and free-form worker error
// detail never enter the template.
type artifactFetchOperationView struct {
	OperationID     string
	Name            string
	Version         string
	State           store.ArtifactFetchState
	Phase           store.ArtifactFetchPhase
	ProgressBytes   int64
	MaxBytes        int64
	Attempt         int64
	ResultSHA256    *string
	ResultSizeBytes *int64
	ErrorCode       *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

type artifactFetchOperationsView struct {
	Items       []artifactFetchOperationView
	Total       int
	EvaluatedAt time.Time
}

type artifactFetchOperationPage struct {
	Item artifactFetchOperationView
}

// artifactFetchReviewPage carries only what the confirmation form needs. The
// two opaque replay-binding values are hidden form state and are never printed
// as operator-facing evidence.
type artifactFetchReviewPage struct {
	Name                        string
	Version                     string
	SourceKind                  string
	PolicyVersion               string
	EnginesNode                 *string
	MaxBytes                    int64
	AlreadyAvailableAndVerified bool
	ExistingArtifactSHA256      *string
	EnqueueAllowed              bool
	Blockers                    []string
	Reason                      string
	PreviewDigest               string
	IdempotencyKey              string
	CapabilityAllowed           bool
}

func (s *Server) appsSurface(w http.ResponseWriter, r *http.Request) {
	view, err := parseAppsSurfaceView(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	data := &appsSurfacePage{View: view, CurrentLabel: appsSurfaceLabel(view)}
	switch view {
	case "overview", "openclaw":
		rows, err := s.openClawSurfaceRows()
		if err != nil {
			s.fail(w, "讀取 OpenClaw 應用事實失敗", err)
			return
		}
		if view == "openclaw" {
			data.OpenClaw = rows
			break
		}
		overview := &appsOverview{ActiveMachines: len(rows)}
		versions := make(map[string]struct{})
		for _, row := range rows {
			if row.Version != "未知" {
				overview.ObservedInstalls++
				versions[row.Version] = struct{}{}
			}
			if row.ReleaseKnown && row.ManagedRelease {
				overview.ManagedReleases++
			}
		}
		overview.KnownVersions = len(versions)
		data.Overview = overview
	case "artifacts":
		data.CatalogConfigured = strings.TrimSpace(s.artifactsDir) != ""
		if data.CatalogConfigured {
			result, err := s.artifactOperator.ListArtifacts(operator.ArtifactListRequest{
				Limit: operator.MaxArtifactReadLimit,
			}, now)
			if err != nil {
				s.fail(w, "讀取 artifact catalog 失敗", err)
				return
			}
			data.Artifacts = &result
		}
	case "fetch":
	case "operations":
		result, err := s.artifactOperator.ArtifactFetchOperations(store.ArtifactFetchListRequest{
			Limit: store.MaxArtifactFetchReadLimit,
		})
		if err != nil {
			s.fail(w, "讀取 artifact fetch operations 失敗", err)
			return
		}
		data.Operations = projectArtifactFetchOperations(result)
	case "store":
		storeView, err := s.catalogStoreSurface(now)
		if err != nil {
			s.fail(w, "讀取 Standard Store 失敗", err)
			return
		}
		data.Store = storeView
	case "profiles":
		profilesView, err := s.catalogProfilesSurface(now)
		if err != nil {
			s.fail(w, "讀取 Profiles 失敗", err)
			return
		}
		data.Profiles = profilesView
	case "assignments":
		assignmentsView, err := s.catalogAssignmentsSurface(now)
		if err != nil {
			s.fail(w, "讀取 Assignments 失敗", err)
			return
		}
		data.Assignments = assignmentsView
	}
	s.render(w, r, "apps.html", page{
		Title: appsSurfaceLabel(view), Nav: "apps", Now: now.Local().Format("2006-01-02 15:04"),
		Apps: data,
	})
}

func parseAppsSurfaceView(r *http.Request) (string, error) {
	if r == nil || r.URL == nil {
		return "", errors.New("應用查詢不存在。")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return "", errors.New("應用查詢不可是空的問號。")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", errors.New("應用查詢格式不合法。")
	}
	for key := range values {
		if key != "view" {
			return "", fmt.Errorf("不支援應用查詢參數 %q。", key)
		}
	}
	view := "overview"
	if raw, present := values["view"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return "", errors.New("應用 view 必須只出現一次且不可為空。")
		}
		view = raw[0]
	}
	switch view {
	case "overview", "openclaw", "store", "profiles", "assignments", "artifacts", "fetch", "operations":
		return view, nil
	default:
		return "", errors.New("應用 view 不合法。")
	}
}

func appsSurfaceLabel(view string) string {
	switch view {
	case "openclaw":
		return "OpenClaw"
	case "artifacts":
		return "Artifacts"
	case "store":
		return "Store"
	case "profiles":
		return "Profiles"
	case "assignments":
		return "Assignments"
	case "fetch":
		return "Fetch"
	case "operations":
		return "Operations"
	default:
		return "概觀"
	}
}

func (s *Server) openClawSurfaceRows() ([]appSurfaceMachine, error) {
	machines, err := s.store.ListMachines()
	if err != nil {
		return nil, err
	}
	active := make([]store.Machine, 0, len(machines))
	ids := make([]string, 0, len(machines))
	for _, machine := range machines {
		if machine.RetiredAt != nil {
			continue
		}
		active = append(active, machine)
		ids = append(ids, machine.MachineID)
	}
	installs, err := s.store.LatestOpenClawInstalls(ids)
	if err != nil {
		return nil, err
	}
	rows := make([]appSurfaceMachine, 0, len(active))
	for _, machine := range active {
		row := appSurfaceMachine{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName,
			Channel: machine.Channel, Version: "未知", NodeVersion: "未知",
		}
		if install := installs[machine.MachineID]; install != nil {
			// Observation fields are agent-controlled. Keep the HTML projection on
			// the same bounded-text contract as the Updates JSON model rather than
			// reflecting oversized or directional/control content into the browser.
			if validWebBoundedText(install.RunningDirVersion, 128) {
				row.Version = install.RunningDirVersion
			}
			if validWebBoundedText(install.RunningDir, 4096) &&
				validWebBoundedText(install.ReleasesDir, 4096) {
				row.ReleaseKnown = true
				row.ManagedRelease = isClawctlRelease(*install)
			}
			if validWebBoundedText(install.NodeVersion, 128) {
				row.NodeVersion = install.NodeVersion
			}
		}
		job, ok, err := s.store.LatestSucceededJobForResource(machine.MachineID, "openclaw", "openclaw")
		if err != nil {
			return nil, err
		}
		if ok {
			row.SucceededJobID = job.JobID
			row.SucceededJobAt = job.TerminalAt
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) artifactDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requestHasNoQuery(r) {
		http.Error(w, "artifact detail 不接受 query parameters", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(s.artifactsDir) == "" {
		s.renderAppsActionStatus(w, r, http.StatusServiceUnavailable, r.PathValue("id"),
			"無法驗證 artifact", "Artifact catalog 無法取得。", "/apps?view=artifacts", "回 Artifacts")
		return
	}
	now := time.Now().UTC()
	result, err := s.artifactOperator.ArtifactDetail(r.Context(), r.PathValue("id"), now)
	if err != nil {
		status, detail := http.StatusInternalServerError, "讀取 artifact 證據失敗。"
		if errors.Is(err, operator.ErrArtifactNotFound) {
			status, detail = http.StatusNotFound, "找不到指定的 artifact。"
		} else if errors.Is(err, operator.ErrInvalidArtifactRead) {
			status, detail = http.StatusBadRequest, "artifact id 不合法。"
		}
		s.renderAppsActionStatus(w, r, status, r.PathValue("id"), "無法驗證 artifact", detail,
			"/apps?view=artifacts", "回 Artifacts")
		return
	}
	s.render(w, r, "artifact_detail.html", page{
		Title: "Artifact " + short(result.Item.ArtifactID, 12), Nav: "apps",
		Now:            now.Local().Format("2006-01-02 15:04"),
		ArtifactDetail: &artifactDetailPage{Item: result.Item, EvaluatedAt: result.EvaluatedAt},
	})
}

func (s *Server) artifactFetchOperationDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requestHasNoQuery(r) {
		http.Error(w, "artifact fetch operation detail 不接受 query parameters", http.StatusBadRequest)
		return
	}
	operation, err := s.artifactOperator.ArtifactFetchOperation(r.PathValue("id"))
	if err != nil {
		status, detail := http.StatusInternalServerError, "讀取 artifact fetch operation 失敗。"
		if errors.Is(err, store.ErrArtifactFetchNotFound) {
			status, detail = http.StatusNotFound, "找不到指定的 artifact fetch operation。"
		}
		s.renderAppsActionStatus(w, r, status, r.PathValue("id"), "無法讀取 fetch operation", detail,
			"/apps?view=operations", "回 Operations")
		return
	}
	now := time.Now().UTC()
	s.render(w, r, "artifact_fetch_operation.html", page{
		Title: "Fetch operation " + short(operation.OperationID, 12), Nav: "apps",
		Now:                    now.Local().Format("2006-01-02 15:04"),
		ArtifactFetchOperation: &artifactFetchOperationPage{Item: projectArtifactFetchOperation(operation)},
	})
}

func projectArtifactFetchOperations(result store.ArtifactFetchListResult) *artifactFetchOperationsView {
	view := &artifactFetchOperationsView{
		Items: make([]artifactFetchOperationView, 0, len(result.Items)),
		Total: result.Total, EvaluatedAt: result.EvaluatedAt,
	}
	for _, operation := range result.Items {
		view.Items = append(view.Items, projectArtifactFetchOperation(operation))
	}
	return view
}

func projectArtifactFetchOperation(operation store.ArtifactFetchOperation) artifactFetchOperationView {
	return artifactFetchOperationView{
		OperationID: operation.OperationID, Name: operation.Name, Version: operation.Version,
		State: operation.State, Phase: operation.Phase, ProgressBytes: operation.ProgressBytes,
		MaxBytes: operation.MaxBytes, Attempt: operation.Attempt,
		ResultSHA256: operation.ResultSHA256, ResultSizeBytes: operation.ResultSizeBytes,
		ErrorCode: operation.ErrorCode, CreatedAt: operation.CreatedAt, UpdatedAt: operation.UpdatedAt,
		StartedAt: operation.StartedAt, FinishedAt: operation.FinishedAt,
	}
}

func (s *Server) previewArtifactFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireArtifactFetchAdmin(w, r) {
		return
	}
	values, err := parseArtifactFetchWebForm(w, r,
		map[string]bool{"name": true, "version": true, "reason": true},
		[]string{"name", "version", "reason"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "artifact",
			"沒有建立 fetch preview", err.Error(), "/apps?view=fetch", "回 Fetch")
		return
	}
	name, version, reason := values.Get("name"), values.Get("version"), strings.TrimSpace(values.Get("reason"))
	if !validArtifactFetchWebTarget(name, version) ||
		!validWebBoundedText(reason, 500) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "artifact",
			"沒有建立 fetch preview", "請選擇 package、輸入完整版本與最多 500 bytes 的理由。",
			"/apps?view=fetch", "回 Fetch")
		return
	}
	preview, err := s.artifactOperator.PreviewArtifactFetch(r.Context(), operator.ArtifactFetchPreviewRequest{
		Name: name, Version: version,
	})
	if err != nil {
		status, detail := artifactFetchWebError(err, true)
		s.renderAppsActionStatus(w, r, status, name+"@"+version,
			"沒有建立 fetch preview", detail, "/apps?view=fetch", "回 Fetch")
		return
	}
	if err := validateArtifactFetchPreviewForWeb(preview, name, version); err != nil {
		s.renderAppsActionStatus(w, r, http.StatusInternalServerError, name+"@"+version,
			"沒有建立 fetch preview", "預覽結果不符合 artifact intake 契約。", "/apps?view=fetch", "回 Fetch")
		return
	}
	key, err := operator.NewIdempotencyKey("web-artifact-fetch")
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusInternalServerError, name+"@"+version,
			"沒有建立 fetch preview", "無法產生這次確認所需的 request identity。", "/apps?view=fetch", "回 Fetch")
		return
	}
	now := time.Now().UTC()
	s.render(w, r, "artifact_fetch_review.html", page{
		Title: "確認 fetch " + name + "@" + version, Nav: "apps", Now: now.Local().Format("2006-01-02 15:04"),
		ArtifactFetchReview: &artifactFetchReviewPage{
			Name: preview.Name, Version: preview.Version, SourceKind: preview.SourceKind,
			PolicyVersion: preview.PolicyVersion,
			EnginesNode:   preview.EnginesNode, MaxBytes: preview.MaxBytes,
			AlreadyAvailableAndVerified: preview.AlreadyAvailableAndVerified,
			ExistingArtifactSHA256:      preview.ExistingArtifactSHA256,
			EnqueueAllowed:              preview.EnqueueAllowed, Blockers: append([]string(nil), preview.Blockers...),
			Reason: reason, PreviewDigest: preview.PreviewDigest, IdempotencyKey: key,
			CapabilityAllowed: accessFromRequest(r).CanAdmin,
		},
	})
}

func validateArtifactFetchPreviewForWeb(result operator.ArtifactFetchPreviewResult, name, version string) error {
	if result.SchemaVersion != operator.ArtifactFetchPreviewSchemaVersion || result.Name != name ||
		result.Version != version || result.PolicyVersion == "" || result.PreviewedAt.IsZero() ||
		result.MaxBytes <= 0 || !strings.HasPrefix(result.PreviewDigest, "sha256:") ||
		!artifact.ValidSHA256Hex(strings.TrimPrefix(result.PreviewDigest, "sha256:")) || result.Blockers == nil {
		return errors.New("incoherent artifact fetch preview")
	}
	switch result.SourceKind {
	case artifact.ArtifactSourceNPM:
		if name != "openclaw" || result.RegistryOrigin != artifact.ProductionRegistryOrigin ||
			result.PolicyVersion != artifact.FetchPolicyVersion || result.MaxBytes != artifact.DefaultArtifactMaxBytes {
			return errors.New("incoherent npm artifact fetch source")
		}
	case artifact.ArtifactSourceNode:
		if name != "node-runtime" || result.RegistryOrigin != artifact.ProductionNodeDistributionOrigin ||
			result.PolicyVersion != artifact.NodeRuntimeFetchPolicyVersion || result.EnginesNode != nil ||
			result.MaxBytes != artifact.DefaultNodeRuntimeBundleMaxBytes {
			return errors.New("incoherent node artifact fetch source")
		}
	case artifact.ArtifactSourceHermesImage:
		if name != "hermes-agent" || result.RegistryOrigin != artifact.ProductionHermesRegistryOrigin ||
			result.PolicyVersion != artifact.HermesImageFetchPolicyVersion || result.EnginesNode != nil ||
			result.MaxBytes != artifact.DefaultHermesImageBundleMaxBytes {
			return errors.New("incoherent Hermes artifact fetch source")
		}
	default:
		return errors.New("unknown artifact fetch source")
	}
	if result.AlreadyAvailableAndVerified && (result.ExistingArtifactSHA256 == nil ||
		!artifact.ValidSHA256Hex(*result.ExistingArtifactSHA256)) {
		return errors.New("verified cache result lacks a canonical digest")
	}
	return nil
}

func (s *Server) applyArtifactFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireArtifactFetchAdmin(w, r) {
		return
	}
	values, err := parseArtifactFetchWebForm(w, r, map[string]bool{
		"name": true, "version": true, "preview_digest": true, "confirm_name": true,
		"confirm_version": true, "reason": true, "idempotency_key": true,
	}, []string{"name", "version", "preview_digest", "confirm_name", "confirm_version", "reason", "idempotency_key"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "artifact",
			"沒有建立 fetch operation", err.Error(), "/apps?view=fetch", "回 Fetch")
		return
	}
	name, version := values.Get("name"), values.Get("version")
	confirmName, confirmVersion := values.Get("confirm_name"), values.Get("confirm_version")
	reason, previewDigest, key := strings.TrimSpace(values.Get("reason")), values.Get("preview_digest"), values.Get("idempotency_key")
	if !validArtifactFetchWebTarget(name, version) ||
		len(confirmName) > 128 || confirmName != strings.TrimSpace(confirmName) ||
		len(confirmVersion) > 128 || confirmVersion != strings.TrimSpace(confirmVersion) ||
		!validWebBoundedText(reason, 500) || len(key) > 200 || key != strings.TrimSpace(key) ||
		!strings.HasPrefix(previewDigest, "sha256:") || !artifact.ValidSHA256Hex(strings.TrimPrefix(previewDigest, "sha256:")) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, name+"@"+version,
			"沒有建立 fetch operation", "Typed confirmation 或 preview request 格式不合法。",
			"/apps?view=fetch", "回 Fetch")
		return
	}
	result, err := s.artifactOperator.ApplyArtifactFetch(r.Context(), operator.ArtifactFetchApplyRequest{
		Name: name, Version: version, PreviewDigest: previewDigest,
		ConfirmName: confirmName, ConfirmVersion: confirmVersion, Reason: reason,
		IdempotencyKey: key, Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		status, detail := artifactFetchWebError(err, false)
		s.renderAppsActionStatus(w, r, status, name+"@"+version,
			"沒有建立 fetch operation", detail, "/apps?view=fetch", "回 Fetch")
		return
	}
	if result.Operation.OperationID == "" {
		s.renderAppsActionStatus(w, r, http.StatusInternalServerError, name+"@"+version,
			"無法顯示 fetch operation", "建立結果缺少 operation identity；請從動作紀錄確認結果。",
			"/apps?view=operations", "回 Operations")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, "/apps/artifact-fetches/"+url.PathEscape(result.Operation.OperationID), http.StatusSeeOther)
}

func validArtifactFetchWebTarget(name, version string) bool {
	if version == "" || len(version) > 128 || version != strings.TrimSpace(version) {
		return false
	}
	switch name {
	case "openclaw":
		return artifact.ValidOpenClawVersion(version)
	case "node-runtime":
		return artifact.ValidNodeRuntimeVersion(version)
	case "hermes-agent":
		return artifact.ValidHermesVersion(version)
	default:
		return false
	}
}

func (s *Server) requireArtifactFetchAdmin(w http.ResponseWriter, r *http.Request) bool {
	if accessFromRequest(r).CanAdmin {
		return true
	}
	s.renderAppsActionStatus(w, r, http.StatusForbidden, "artifact fetch",
		"沒有執行 artifact fetch", "Artifact fetch preview 與 apply 需要 admin capability。",
		"/apps?view=fetch", "回 Fetch")
	return false
}

func parseArtifactFetchWebForm(w http.ResponseWriter, r *http.Request, allowed map[string]bool,
	required []string,
) (url.Values, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery || r.URL.RawQuery != "" {
		return nil, errors.New("artifact fetch form 不接受 query parameters")
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return nil, errors.New("artifact fetch form 只接受 application/x-www-form-urlencoded")
	}
	if r.Body == nil {
		return nil, errors.New("artifact fetch form 缺少 request body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, artifactFetchWebFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		return nil, errors.New("artifact fetch form 編碼不合法")
	}
	for key, values := range r.PostForm {
		if !allowed[key] || len(values) != 1 {
			return nil, fmt.Errorf("artifact fetch form 欄位 %q 不合法或重複", key)
		}
	}
	for _, key := range required {
		values, ok := r.PostForm[key]
		if !ok || len(values) != 1 || values[0] == "" {
			return nil, fmt.Errorf("artifact fetch form 缺少 %s", key)
		}
	}
	return r.PostForm, nil
}

func artifactFetchWebError(err error, preview bool) (int, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "讀取 registry metadata 逾時；沒有建立 operation。"
	case errors.Is(err, artifact.ErrRegistryPolicy), errors.Is(err, artifact.ErrMetadataInvalid),
		errors.Is(err, artifact.ErrMetadataTooLarge):
		return http.StatusBadGateway, "Registry metadata 未通過 intake policy；沒有建立 operation。"
	case errors.Is(err, store.ErrArtifactFetchPrepareFailed):
		return http.StatusBadGateway, "Registry metadata 驗證失敗；request 未入列；使用相同確認重試。"
	case errors.Is(err, operator.ErrInvalidArtifactFetchPreview), errors.Is(err, artifact.ErrInvalidFetchRequest):
		return http.StatusBadRequest, "Artifact name 或 exact version 不合法。"
	}
	status, code, _ := operator.HTTPError(err)
	if status != http.StatusInternalServerError {
		if code == store.OperatorCodeArtifactFetchPreviewStale {
			return status, "Registry metadata 已改變；請重新 Preview 後再確認。"
		}
		return status, "Artifact fetch 被 canonical operator policy 拒絕（" + code + "）。"
	}
	if preview {
		return status, "建立 artifact fetch preview 失敗。"
	}
	return status, "建立 artifact fetch operation 失敗。"
}

func requestHasNoQuery(r *http.Request) bool {
	return r != nil && r.URL != nil && !r.URL.ForceQuery && r.URL.RawQuery == ""
}

func validWebBoundedText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func (s *Server) renderAppsActionStatus(w http.ResponseWriter, r *http.Request, status int,
	subject, headline, detail, back, backLabel string,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.render(w, r, "action.html", page{
		Title: headline, Nav: "apps", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Apps: &appsSurfacePage{CurrentLabel: appsSurfaceLabelFromBack(back)},
		Action: &actionResult{
			Subject: subject, Headline: headline, Detail: detail, Back: back, BackLabel: backLabel,
		},
	})
}

func appsSurfaceLabelFromBack(back string) string {
	switch {
	case strings.Contains(back, "view=artifacts"):
		return "Artifacts"
	case strings.Contains(back, "view=store"):
		return "Store"
	case strings.Contains(back, "view=profiles"):
		return "Profiles"
	case strings.Contains(back, "view=assignments"):
		return "Assignments"
	case strings.Contains(back, "view=operations"):
		return "Operations"
	case strings.Contains(back, "view=fetch"):
		return "Fetch"
	default:
		return "概觀"
	}
}
