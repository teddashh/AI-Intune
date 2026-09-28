package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/operator"
)

type catalogArtifactOption struct {
	SHA256  string
	Name    string
	Version string
}

type catalogMachineOption struct {
	MachineID   string
	DisplayName string
	OS          string
	Arch        string

	// Profile 是這台現在身上那一份 profile。nil 是「還沒有被指派過任何 profile」。
	//
	// ⚠⚠ 這一欄回答的是「現在誰有」，不是「可以指派給誰」。這一頁原本只列得出後
	// 者，而那兩件事在畫面上長得一模一樣：一張列著五台機器的表，看起來像五台都被
	// 指派了，實際上帳本上一列指派都沒有。
	Profile *catalogMachineProfile
}

// catalogMachineProfile 是一台機器現行那一份指派。
type catalogMachineProfile struct {
	ProfileID  string
	Revision   int64
	AssignedAt time.Time
	AssignedBy string
}

type catalogStoreView struct {
	Packages          []operator.CatalogManifestRecord
	Artifacts         []catalogArtifactOption
	NodeVersions      []string
	CatalogConfigured bool
}

// catalogProfilesView 與 catalogAssignmentsView 都拿 operator.ProfileReport 當已發佈
// 那幾版的唯一來源。
//
// ⚠⚠ 兩頁與 `/reports/profile` 讀同一份投影。各自算一次的話，漂掉的樣子是同一版
// profile 在兩頁上有兩個「穿在幾台身上」，而那兩個數字沒有一個問得出誰是對的。
//
// ⚠ 換掉的是 `ListMachineProfiles`：它的上限是 100 版而且**靜靜截斷**，所以第 101 版
// 發佈之後，這兩頁會漏掉它而畫面上完全看不出來。報告那一側超過 500 版是拒絕，不是截斷。
type catalogProfilesView struct {
	Report   operator.ProfileReport
	Packages []operator.CatalogManifestRecord
}

type catalogAssignmentsView struct {
	Report   operator.ProfileReport
	Machines []catalogMachineOption
}

type catalogPackageReviewPage struct {
	Preview         operator.StandardCatalogManifestPreviewResult
	ManifestToken   string
	Reason          string
	IdempotencyKey  string
	PackageIdentity string
	CanApply        bool
}

type catalogProfileReviewPage struct {
	Preview         operator.MachineProfilePreviewResult
	ProfileToken    string
	Reason          string
	IdempotencyKey  string
	ProfileIdentity string
	CanApply        bool
}

type catalogAssignmentReviewPage struct {
	Preview        operator.MachineProfileAssignmentPreviewResult
	Reason         string
	IdempotencyKey string
	CanApply       bool
}

func (s *Server) catalogStoreSurface(now time.Time) (*catalogStoreView, error) {
	manifests, err := s.catalogOperator.ListCatalogManifests(operator.CatalogManifestListRequest{
		Limit: operator.MaxCatalogReadLimit,
	}, now)
	if err != nil {
		return nil, err
	}
	view := &catalogStoreView{Packages: manifests.Items, CatalogConfigured: strings.TrimSpace(s.artifactsDir) != ""}
	for _, item := range manifests.Items {
		if item.Manifest.ID == "node-runtime" {
			view.NodeVersions = append(view.NodeVersions, item.Manifest.Version)
		}
	}
	if !view.CatalogConfigured {
		return view, nil
	}
	artifacts, err := s.artifactOperator.ListArtifacts(operator.ArtifactListRequest{
		Limit: operator.MaxArtifactReadLimit,
	}, now)
	if err != nil {
		return nil, err
	}
	for _, item := range artifacts.Items {
		if item.Name == nil || item.Version == nil ||
			(*item.Name != "openclaw" && *item.Name != "hermes-agent" && *item.Name != "node-runtime") ||
			!artifact.ValidSHA256Hex(item.ArtifactID) {
			continue
		}
		view.Artifacts = append(view.Artifacts, catalogArtifactOption{
			SHA256: item.ArtifactID, Name: *item.Name, Version: *item.Version,
		})
	}
	return view, nil
}

func (s *Server) catalogProfilesSurface(now time.Time) (*catalogProfilesView, error) {
	report, err := s.operator.ProfileReport(now)
	if err != nil {
		return nil, err
	}
	manifests, err := s.catalogOperator.ListCatalogManifests(operator.CatalogManifestListRequest{
		Limit: operator.MaxCatalogReadLimit,
	}, now)
	if err != nil {
		return nil, err
	}
	return &catalogProfilesView{Report: report, Packages: manifests.Items}, nil
}

func (s *Server) catalogAssignmentsSurface(now time.Time) (*catalogAssignmentsView, error) {
	report, err := s.operator.ProfileReport(now)
	if err != nil {
		return nil, err
	}
	machines, err := s.store.ListMachines()
	if err != nil {
		return nil, err
	}
	worn, err := profileWornByMachine(report)
	if err != nil {
		return nil, err
	}
	view := &catalogAssignmentsView{Report: report, Machines: make([]catalogMachineOption, 0, len(machines))}
	for _, machine := range machines {
		if machine.RetiredAt != nil {
			continue
		}
		option := catalogMachineOption{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName,
			OS: machine.OS, Arch: machine.Arch,
		}
		if profile, wearing := worn[machine.MachineID]; wearing {
			option.Profile = &profile
		}
		view.Machines = append(view.Machines, option)
	}
	sort.Slice(view.Machines, func(i, j int) bool {
		if view.Machines[i].DisplayName != view.Machines[j].DisplayName {
			return view.Machines[i].DisplayName < view.Machines[j].DisplayName
		}
		return view.Machines[i].MachineID < view.Machines[j].MachineID
	})
	return view, nil
}

// profileWornByMachine 把報告反過來讀：一台機器現在身上是哪一份 profile。
//
// ⚠ 帳本上一台機器只有一份現行指派（`FleetProfileAssignments` 只回 assignment_revision
// 最大的那一筆），所以這張表一台一格。一台落在兩版上的話這裡當場拒絕，不是靜靜留下
// 後掃到的那一版——那樣畫面會顯示一個會隨讀取順序改變的答案。
func profileWornByMachine(report operator.ProfileReport) (map[string]catalogMachineProfile, error) {
	worn := map[string]catalogMachineProfile{}
	for _, row := range report.Profiles {
		for _, machine := range row.Machines {
			if machine.Retired {
				continue
			}
			if existing, duplicate := worn[machine.MachineID]; duplicate {
				return nil, fmt.Errorf(
					"%s 同時穿著 %s@%d 與 %s@%d", machine.DisplayName,
					existing.ProfileID, existing.Revision, row.ProfileID, row.Revision)
			}
			worn[machine.MachineID] = catalogMachineProfile{
				ProfileID: row.ProfileID, Revision: row.Revision,
				AssignedAt: machine.AssignedAt, AssignedBy: machine.AssignedBy,
			}
		}
	}
	return worn, nil
}

func (s *Server) previewCatalogPackage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=store") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"artifact_sha256": 1, "node_runtime_version": 1, "reason": 1,
	}, []string{"artifact_sha256", "reason"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Store package", "沒有建立 package preview",
			err.Error(), "/apps?view=store", "回 Store")
		return
	}
	artifactSHA, nodeVersion, reason := values.Get("artifact_sha256"), values.Get("node_runtime_version"), values.Get("reason")
	if !artifact.ValidSHA256Hex(artifactSHA) || nodeVersion != strings.TrimSpace(nodeVersion) || len(nodeVersion) > 128 ||
		!validWebBoundedText(reason, 500) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Store package", "沒有建立 package preview",
			"請選擇 artifact、Node runtime 與發布理由。", "/apps?view=store", "回 Store")
		return
	}
	preview, err := s.catalogOperator.PreviewStandardCatalogManifest(r.Context(), operator.StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: artifactSHA, NodeRuntimeVersion: nodeVersion,
	})
	if err != nil {
		s.renderCatalogError(w, r, err, "Store package", "沒有建立 package preview", "/apps?view=store", "回 Store")
		return
	}
	key, err := operator.NewIdempotencyKey("web-catalog-package")
	if err != nil {
		s.renderCatalogError(w, r, err, preview.Manifest.ID+"@"+preview.Manifest.Version,
			"沒有建立 package preview", "/apps?view=store", "回 Store")
		return
	}
	token, err := encodeCatalogManifestToken(preview.Manifest)
	if err != nil {
		s.renderCatalogError(w, r, err, preview.Manifest.ID+"@"+preview.Manifest.Version,
			"沒有建立 package preview", "/apps?view=store", "回 Store")
		return
	}
	s.render(w, r, "catalog_review.html", page{
		Title: "確認 Store package", Nav: "apps", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Apps: &appsSurfacePage{CurrentLabel: "Store"},
		CatalogPackageReview: &catalogPackageReviewPage{
			Preview: preview, ManifestToken: token, Reason: reason, IdempotencyKey: key,
			PackageIdentity: preview.Manifest.ID + "@" + preview.Manifest.Version, CanApply: true,
		},
	})
}

func (s *Server) publishCatalogPackage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=store") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"manifest": 1, "preview_digest": 1, "confirm_package": 1, "reason": 1, "idempotency_key": 1,
	}, []string{"manifest", "preview_digest", "confirm_package", "reason", "idempotency_key"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Store package", "沒有發布 package",
			err.Error(), "/apps?view=store", "回 Store")
		return
	}
	manifest, err := decodeCatalogManifestToken(values.Get("manifest"))
	identity := manifest.ID + "@" + manifest.Version
	if err != nil || values.Get("confirm_package") != identity || !validWebBoundedText(values.Get("reason"), 500) ||
		!validCatalogWebKey(values.Get("idempotency_key")) || !validCatalogWebDigest(values.Get("preview_digest")) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, identity, "沒有發布 package",
			"Package、確認文字或 request identity 不合法。", "/apps?view=store", "回 Store")
		return
	}
	result, err := s.catalogOperator.PublishStandardCatalogManifest(r.Context(), operator.CatalogManifestPublishRequest{
		Manifest: manifest, ConfirmPackageID: manifest.ID, ConfirmVersion: manifest.Version,
		PreviewDigest: values.Get("preview_digest"), Reason: values.Get("reason"),
		IdempotencyKey: values.Get("idempotency_key"), Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderCatalogError(w, r, err, identity, "沒有發布 package", "/apps?view=store", "回 Store")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, "/apps?view=store", http.StatusSeeOther)
}

func (s *Server) previewCatalogProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=profiles") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"profile_id": 1, "revision": 1, "package": appcatalog.MaxDependencies, "reason": 1,
	}, []string{"profile_id", "revision", "package", "reason"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Profile", "沒有建立 profile preview",
			err.Error(), "/apps?view=profiles", "回 Profiles")
		return
	}
	revision, err := strconv.ParseInt(values.Get("revision"), 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != values.Get("revision") ||
		!validWebBoundedText(values.Get("profile_id"), 128) || !validWebBoundedText(values.Get("reason"), 500) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Profile", "沒有建立 profile preview",
			"Profile ID、revision 或發布理由不合法。", "/apps?view=profiles", "回 Profiles")
		return
	}
	refs := make([]appcatalog.PackageRef, 0, len(values["package"]))
	seen := make(map[string]bool, len(values["package"]))
	for _, identity := range values["package"] {
		packageID, version, ok := strings.Cut(identity, "@")
		if !ok || strings.Contains(version, "@") || seen[identity] ||
			!validWebBoundedText(packageID, 128) || !validWebBoundedText(version, 128) {
			s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Profile", "沒有建立 profile preview",
				"Package selection 不合法。", "/apps?view=profiles", "回 Profiles")
			return
		}
		seen[identity] = true
		refs = append(refs, appcatalog.PackageRef{PackageID: packageID, Version: version})
	}
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: values.Get("profile_id"), Revision: revision, Packages: refs,
	}
	preview, err := s.catalogOperator.PreviewMachineProfile(r.Context(), operator.MachineProfilePreviewRequest{Profile: profile})
	if err != nil {
		s.renderCatalogError(w, r, err, profile.ID+"@"+values.Get("revision"),
			"沒有建立 profile preview", "/apps?view=profiles", "回 Profiles")
		return
	}
	key, err := operator.NewIdempotencyKey("web-catalog-profile")
	if err != nil {
		s.renderCatalogError(w, r, err, profile.ID+"@"+values.Get("revision"),
			"沒有建立 profile preview", "/apps?view=profiles", "回 Profiles")
		return
	}
	token, err := encodeCatalogProfileToken(preview.Profile)
	if err != nil {
		s.renderCatalogError(w, r, err, profile.ID+"@"+values.Get("revision"),
			"沒有建立 profile preview", "/apps?view=profiles", "回 Profiles")
		return
	}
	s.render(w, r, "catalog_review.html", page{
		Title: "確認 Profile", Nav: "apps", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Apps: &appsSurfacePage{CurrentLabel: "Profiles"},
		CatalogProfileReview: &catalogProfileReviewPage{
			Preview: preview, ProfileToken: token, Reason: values.Get("reason"), IdempotencyKey: key,
			ProfileIdentity: preview.Profile.ID + "@" + strconv.FormatInt(preview.Profile.Revision, 10), CanApply: true,
		},
	})
}

func (s *Server) publishCatalogProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=profiles") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"profile": 1, "preview_digest": 1, "confirm_profile": 1, "reason": 1, "idempotency_key": 1,
	}, []string{"profile", "preview_digest", "confirm_profile", "reason", "idempotency_key"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Profile", "沒有發布 profile",
			err.Error(), "/apps?view=profiles", "回 Profiles")
		return
	}
	profile, err := decodeCatalogProfileToken(values.Get("profile"))
	identity := profile.ID + "@" + strconv.FormatInt(profile.Revision, 10)
	if err != nil || values.Get("confirm_profile") != identity || !validWebBoundedText(values.Get("reason"), 500) ||
		!validCatalogWebKey(values.Get("idempotency_key")) || !validCatalogWebDigest(values.Get("preview_digest")) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, identity, "沒有發布 profile",
			"Profile、確認文字或 request identity 不合法。", "/apps?view=profiles", "回 Profiles")
		return
	}
	result, err := s.catalogOperator.PublishReviewedMachineProfile(r.Context(), operator.MachineProfilePublishRequest{
		Profile: profile, ConfirmProfileID: profile.ID, ConfirmRevision: profile.Revision,
		PreviewDigest: values.Get("preview_digest"), Reason: values.Get("reason"),
		IdempotencyKey: values.Get("idempotency_key"), Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderCatalogError(w, r, err, identity, "沒有發布 profile", "/apps?view=profiles", "回 Profiles")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, "/apps?view=profiles", http.StatusSeeOther)
}

func (s *Server) previewCatalogAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=assignments") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"machine_id": 1, "profile": 1, "reason": 1,
	}, []string{"machine_id", "profile", "reason"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Assignment", "沒有建立 assignment preview",
			err.Error(), "/apps?view=assignments", "回 Assignments")
		return
	}
	profileID, revisionText, ok := strings.Cut(values.Get("profile"), "@")
	revision, parseErr := strconv.ParseInt(revisionText, 10, 64)
	if !ok || strings.Contains(revisionText, "@") || parseErr != nil || revision <= 0 ||
		!validWebBoundedText(values.Get("machine_id"), 128) || strings.Contains(values.Get("machine_id"), "/") ||
		!validWebBoundedText(profileID, 128) || !validWebBoundedText(values.Get("reason"), 500) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Assignment", "沒有建立 assignment preview",
			"Machine、Profile 或指派理由不合法。", "/apps?view=assignments", "回 Assignments")
		return
	}
	preview, err := s.catalogOperator.PreviewMachineProfileAssignment(r.Context(), operator.MachineProfileAssignmentPreviewRequest{
		MachineID: values.Get("machine_id"), ProfileID: profileID, ProfileRevision: revision,
	})
	if err != nil {
		s.renderCatalogError(w, r, err, values.Get("machine_id"), "沒有建立 assignment preview",
			"/apps?view=assignments", "回 Assignments")
		return
	}
	key, err := operator.NewIdempotencyKey("web-catalog-assignment")
	if err != nil {
		s.renderCatalogError(w, r, err, preview.DisplayName, "沒有建立 assignment preview",
			"/apps?view=assignments", "回 Assignments")
		return
	}
	s.render(w, r, "catalog_review.html", page{
		Title: "確認 Profile Assignment", Nav: "apps", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Apps: &appsSurfacePage{CurrentLabel: "Assignments"},
		CatalogAssignmentReview: &catalogAssignmentReviewPage{
			Preview: preview, Reason: values.Get("reason"), IdempotencyKey: key, CanApply: len(preview.Blockers) == 0,
		},
	})
}

func (s *Server) applyCatalogAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireCatalogAdmin(w, r, "/apps?view=assignments") {
		return
	}
	values, err := parseCatalogWebForm(w, r, map[string]int{
		"machine_id": 1, "profile_id": 1, "profile_revision": 1, "preview_digest": 1,
		"confirm_name": 1, "reason": 1, "idempotency_key": 1,
	}, []string{"machine_id", "profile_id", "profile_revision", "preview_digest", "confirm_name", "reason", "idempotency_key"})
	if err != nil {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Assignment", "沒有指派 profile",
			err.Error(), "/apps?view=assignments", "回 Assignments")
		return
	}
	revision, parseErr := strconv.ParseInt(values.Get("profile_revision"), 10, 64)
	if parseErr != nil || revision <= 0 || !validWebBoundedText(values.Get("machine_id"), 128) ||
		strings.Contains(values.Get("machine_id"), "/") || !validWebBoundedText(values.Get("profile_id"), 128) ||
		!validWebBoundedText(values.Get("confirm_name"), 200) || !validWebBoundedText(values.Get("reason"), 500) ||
		!validCatalogWebKey(values.Get("idempotency_key")) || !validCatalogWebDigest(values.Get("preview_digest")) {
		s.renderAppsActionStatus(w, r, http.StatusBadRequest, "Assignment", "沒有指派 profile",
			"Assignment、確認文字或 request identity 不合法。", "/apps?view=assignments", "回 Assignments")
		return
	}
	result, err := s.catalogOperator.AssignMachineProfile(r.Context(), operator.MachineProfileAssignmentRequest{
		MachineID: values.Get("machine_id"), ProfileID: values.Get("profile_id"), ProfileRevision: revision,
		ConfirmDisplayName: values.Get("confirm_name"), PreviewDigest: values.Get("preview_digest"),
		Reason: values.Get("reason"), IdempotencyKey: values.Get("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderCatalogError(w, r, err, values.Get("machine_id"), "沒有指派 profile",
			"/apps?view=assignments", "回 Assignments")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, "/machines/"+url.PathEscape(result.MachineID), http.StatusSeeOther)
}

func parseCatalogWebForm(w http.ResponseWriter, r *http.Request, allowed map[string]int,
	required []string,
) (url.Values, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery || r.URL.RawQuery != "" {
		return nil, errors.New("Store form 不接受 query parameters")
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Body == nil {
		return nil, errors.New("Store form 必須是 application/x-www-form-urlencoded")
	}
	r.Body = http.MaxBytesReader(w, r.Body, artifactFetchWebFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		return nil, errors.New("Store form 編碼不合法")
	}
	for key, values := range r.PostForm {
		max := allowed[key]
		if max == 0 || len(values) == 0 || len(values) > max {
			return nil, fmt.Errorf("Store form 欄位 %q 不合法或重複", key)
		}
	}
	for _, key := range required {
		values := r.PostForm[key]
		if len(values) == 0 {
			return nil, fmt.Errorf("Store form 缺少 %s", key)
		}
		for _, value := range values {
			if value == "" {
				return nil, fmt.Errorf("Store form 缺少 %s", key)
			}
		}
	}
	return r.PostForm, nil
}

func encodeCatalogManifestToken(manifest appcatalog.Manifest) (string, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	canonical, err := appcatalog.ParseManifest(raw)
	if err != nil {
		return "", err
	}
	raw, _ = json.Marshal(canonical)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCatalogManifestToken(token string) (appcatalog.Manifest, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 16<<10 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return appcatalog.Manifest{}, errors.New("manifest token 不合法")
	}
	manifest, err := appcatalog.ParseManifest(raw)
	if err != nil {
		return appcatalog.Manifest{}, err
	}
	canonical, _ := encodeCatalogManifestToken(manifest)
	if canonical != token {
		return appcatalog.Manifest{}, errors.New("manifest token 不是 canonical")
	}
	return manifest, nil
}

func encodeCatalogProfileToken(profile appcatalog.MachineProfile) (string, error) {
	raw, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	canonical, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return "", err
	}
	raw, _ = json.Marshal(canonical)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCatalogProfileToken(token string) (appcatalog.MachineProfile, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 16<<10 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return appcatalog.MachineProfile{}, errors.New("profile token 不合法")
	}
	profile, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return appcatalog.MachineProfile{}, err
	}
	canonical, _ := encodeCatalogProfileToken(profile)
	if canonical != token {
		return appcatalog.MachineProfile{}, errors.New("profile token 不是 canonical")
	}
	return profile, nil
}

func validCatalogWebDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifact.ValidSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func validCatalogWebKey(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 200 && !strings.ContainsAny(value, "\r\n")
}

func (s *Server) requireCatalogAdmin(w http.ResponseWriter, r *http.Request, back string) bool {
	if accessFromRequest(r).CanAdmin {
		return true
	}
	s.renderAppsActionStatus(w, r, http.StatusForbidden, "Store", "沒有執行這個動作",
		"這個動作不可用。", back, "返回")
	return false
}

func (s *Server) renderCatalogError(w http.ResponseWriter, r *http.Request, err error,
	subject, headline, back, backLabel string,
) {
	status, _, detail := operator.HTTPError(err)
	if status == http.StatusInternalServerError {
		detail = headline + "。"
	}
	s.renderAppsActionStatus(w, r, status, subject, headline, detail, back, backLabel)
}

var _ catalogWebOperator = (*operator.Service)(nil)
