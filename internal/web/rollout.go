package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

type deploymentListRow struct {
	View      store.DeploymentView
	Version   string
	Counts    string
	JobCounts []deploymentJobCount
}

// deploymentLedgerRow is deliberately based on the safe operator DTO. The
// Updates page still has a private Store-backed planning row below, but the
// public deployment ledger never receives Spec or CreatedBy in its view model.
type deploymentLedgerRow struct {
	Summary   operator.DeploymentSummary
	Version   string
	JobCounts []deploymentJobCount
}

type deploymentJobCount struct {
	State deploy.JobState
	Count int
	Href  string
}

type deploymentsPage struct {
	Rows         []deploymentLedgerRow
	View         string
	CurrentLabel string
	Heading      string
	Empty        string
	Total        int
	IsNew        bool
	Artifacts    []deploymentArtifactOption
}

type deploymentActionEligibility struct {
	Eligible bool
	Reason   string
}

type deploymentPage struct {
	Summary          operator.DeploymentSummary
	Version          string
	Status           string
	JobCounts        []deploymentJobCount
	JobsURL          string
	RestartCount     int
	TerminalStuck    []deploymentTargetRow
	SilentStuck      []deploymentTargetRow
	Included         []deploymentTargetRow
	Excluded         []deploymentExcluded
	ShowCanarySilent bool
	CanarySilent     []rollout.SilentFailure
	Independent      deploymentIndependentSummary
	Continue         deploymentActionEligibility
	SkipFailedBatch  deploymentActionEligibility
	Retry            deploymentActionEligibility
	Abandon          deploymentActionEligibility
	Rollout          rollout.CanaryAssessment
}

// deploymentIndependentSummary is the deployment's own cross-domain line. It
// counts targets, never claims the deployment itself was verified, and keeps
// the verdicts in contract order so two deployments read the same way.
type deploymentIndependentSummary struct {
	OpenedTargets int
	PassedTargets int
	LiveProducers int
	Verdicts      []deploymentIndependentVerdictRow
}

type deploymentIndependentVerdictRow struct {
	Verdict string
	Targets int
}

type deploymentExcluded struct {
	Target deploymentTargetRow
	Reason string
}

type deploymentTargetRow struct {
	MachineID, DisplayName, JobID, ExcludedReason string
	JobState                                      deploy.JobState
	BatchNo                                       int
	TerminalAt                                    *time.Time
	// Independent is empty exactly when the target has no job. An opened job
	// that nobody verified carries the absent verdict instead, because the
	// ledger was read for it and the answer was nothing.
	Independent     string
	IndependentRows int
	IndependentLive int
}

type deploymentArtifactOption struct {
	Version, SHA256, EnginesNode string
	SizeBytes                    int64
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	list, err := parseDeploymentsPageRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	if list.IsNew {
		entries, readErr := artifact.ScanCatalog(s.artifactsDir)
		if readErr != nil {
			s.fail(w, "讀取 Hub artifact catalog 失敗", readErr)
			return
		}
		for _, entry := range entries {
			// The creation picker is metadata-only, but it must still require a
			// trustworthy sidecar plus a present regular tarball whose size matches.
			// One malformed neighbor remains catalog evidence and cannot blank the
			// deployable choices for every other artifact.
			if entry.Status != artifact.CatalogAvailableUnverified || entry.Record == nil ||
				entry.Record.Name != "openclaw" {
				continue
			}
			record := *entry.Record
			list.Artifacts = append(list.Artifacts, deploymentArtifactOption{
				Version: record.Version, SHA256: record.SHA256, EnginesNode: record.EnginesNode, SizeBytes: record.Size,
			})
		}
		s.render(w, r, "deployments.html", page{
			Title: "新增部署", Nav: "deployments", Now: now.Local().Format("2006-01-02 15:04"),
			DeploymentList: &list,
		})
		return
	}
	items, total, err := s.operatorDeployments(now)
	if err != nil {
		s.fail(w, "讀取部署帳本失敗", err)
		return
	}
	// ⚠ 擋的是一張舊 finished 把仍在跑或暫停的部署推到第一屏下面。
	sort.SliceStable(items, func(i, j int) bool {
		if (items[i].State == store.DeploymentFinished) != (items[j].State == store.DeploymentFinished) {
			return items[i].State != store.DeploymentFinished
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	list.Total = total
	rows := make([]deploymentLedgerRow, 0, len(items))
	for _, item := range items {
		if !deploymentSummaryMatchesListView(item, list.View) {
			continue
		}
		rows = append(rows, deploymentLedgerRow{
			Summary: item, Version: deploymentMaterialVersion(item.Material),
			JobCounts: deploymentSafeJobCounts(item.DeploymentID, item.JobStateCounts),
		})
	}
	list.Rows = rows
	s.render(w, r, "deployments.html", page{
		Title: "部署", Nav: "deployments", Now: now.Local().Format("2006-01-02 15:04"),
		DeploymentList: &list,
	})
}

func (s *Server) operatorDeployments(now time.Time) ([]operator.DeploymentSummary, int, error) {
	request := operator.DeploymentListRequest{Limit: operator.MaxDeploymentReadLimit}
	items := []operator.DeploymentSummary{}
	total := 0
	for {
		result, err := s.operator.ListDeployments(request, now)
		if err != nil {
			return nil, 0, err
		}
		if total == 0 {
			total = result.Total
		}
		items = append(items, result.Items...)
		if result.NextCursor == nil {
			return items, total, nil
		}
		request.Cursor = *result.NextCursor
	}
}

func parseDeploymentsPageRequest(r *http.Request) (deploymentsPage, error) {
	if r == nil || r.URL == nil {
		return deploymentsPage{}, errors.New("部署查詢不存在。")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return deploymentsPage{}, errors.New("部署查詢不可是空的問號。")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return deploymentsPage{}, errors.New("部署查詢格式不合法。")
	}
	for key := range values {
		if key != "view" && key != "section" {
			return deploymentsPage{}, fmt.Errorf("不支援部署查詢參數 %q。", key)
		}
	}
	if section, present := values["section"]; present {
		if len(section) != 1 || (section[0] != "deployment-ledger" && section[0] != "deployment-new") {
			return deploymentsPage{}, errors.New("部署 section 不合法。")
		}
	}
	view := "all"
	if raw, present := values["view"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return deploymentsPage{}, errors.New("部署 view 必須恰有一個非空值。")
		}
		view = raw[0]
	}
	result := deploymentsPage{View: view}
	switch view {
	case "all":
		result.CurrentLabel, result.Heading, result.Empty = "全部", "全部 deployment", "還沒有任何部署"
	case "active":
		result.CurrentLabel, result.Heading, result.Empty = "進行中", "進行中的 deployment", "目前沒有 running 或 paused deployment。"
	case "stuck":
		result.CurrentLabel, result.Heading, result.Empty = "Paused / Stuck", "Paused / Stuck deployment", "目前沒有 paused 或 stuck deployment。"
	case "finished":
		result.CurrentLabel, result.Heading, result.Empty = "已完成", "已完成的 deployment", "目前沒有 finished deployment。"
	case "new":
		result.CurrentLabel, result.Heading, result.IsNew = "新增部署", "新增部署", true
	default:
		return deploymentsPage{}, errors.New("部署 view 只接受 all、active、stuck、finished 或 new。")
	}
	return result, nil
}

func deploymentSummaryMatchesListView(item operator.DeploymentSummary, selected string) bool {
	switch selected {
	case "active":
		return item.State != store.DeploymentFinished
	case "stuck":
		return item.State == store.DeploymentPaused || item.Stuck > 0
	case "finished":
		return item.State == store.DeploymentFinished
	default:
		return true
	}
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	detail, err := s.operator.DeploymentDetail(r.PathValue("id"), now)
	if err != nil {
		if errors.Is(err, store.ErrDeploymentNotFound) {
			http.Error(w, "找不到這個 deployment。", http.StatusNotFound)
			return
		}
		s.fail(w, "讀取 deployment 失敗", err)
		return
	}
	events, err := s.store.HubEventsBetween(detail.Item.CreatedAt, now)
	if err != nil {
		s.fail(w, "讀取 deployment 期間的 Hub 日誌失敗", err)
		return
	}
	dp := &deploymentPage{
		Summary: detail.Item, Version: deploymentMaterialVersion(detail.Item.Material),
		Status:    deploymentStatus(detail.Item),
		JobCounts: deploymentSafeJobCounts(detail.Item.DeploymentID, detail.Item.JobStateCounts),
		JobsURL:   deploymentJobsURL(detail.Item.DeploymentID), RestartCount: hubRestartCount(events),
	}
	if detail.Item.Channel == "canary" && detail.Item.State == store.DeploymentFinished && detail.Item.FinishedAt != nil {
		dp.ShowCanarySilent = true
		dp.CanarySilent, err = s.deploymentCanarySilentFailures(detail, now)
		if err != nil {
			s.fail(w, "讀取 canary finished 之後的沉默失敗證據失敗", err)
			return
		}
	}
	for _, target := range detail.Targets {
		row := deploymentTargetFromSafe(target)
		if target.StuckKind != nil {
			switch *target.StuckKind {
			case "terminal_failure":
				dp.TerminalStuck = append(dp.TerminalStuck, row)
			case "no_event":
				dp.SilentStuck = append(dp.SilentStuck, row)
			}
		}
		if target.ExcludedReason == nil {
			dp.Included = append(dp.Included, row)
		} else {
			dp.Excluded = append(dp.Excluded, deploymentExcluded{Target: row, Reason: exclusionReason(*target.ExcludedReason)})
		}
	}
	dp.Independent = deploymentIndependentFromSafe(detail.Independent)
	dp.Rollout = canaryAssessment(detail)
	dp.Continue = deploymentActionView("continue", detail.Actions.Continue)
	dp.SkipFailedBatch = deploymentActionView("skip_failed_batch", detail.Actions.SkipFailedBatch)
	dp.Retry = deploymentActionView("retry", detail.Actions.Retry)
	dp.Abandon = deploymentActionView("abandon", detail.Actions.Abandon)
	s.render(w, r, "deployment.html", page{
		Title: "部署 " + short(detail.Item.DeploymentID, 8), Nav: "deployments-detail",
		Now: now.Local().Format("2006-01-02 15:04"), Deployment: dp,
	})
}

func canaryAssessment(detail operator.DeploymentDetailResult) rollout.CanaryAssessment {
	in := rollout.CanaryInput{
		State: detail.Item.State, OpenedBatch: detail.Item.OpenedBatch,
		TotalBatches: detail.Item.TotalBatches, BatchSize: detail.Item.BatchSize,
		PauseAfterCanary: detail.Item.PauseAfterCanary,
	}
	for _, target := range detail.Targets {
		machine := rollout.CanaryMachine{
			MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo,
			Excluded: target.ExcludedReason != nil,
			Opened:   target.JobID != nil && *target.JobID != "",
		}
		if target.JobState != nil {
			machine.JobState = *target.JobState
		}
		if target.Independent != nil {
			machine.IndependentVerdict = target.Independent.Verdict
		}
		in.Targets = append(in.Targets, machine)
	}
	return rollout.AssessCanary(in)
}

func deploymentTargetFromSafe(target operator.DeploymentTargetSummary) deploymentTargetRow {
	row := deploymentTargetRow{MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo}
	if target.JobID != nil {
		row.JobID = *target.JobID
	}
	if target.JobState != nil {
		row.JobState = *target.JobState
	}
	if target.ExcludedReason != nil {
		row.ExcludedReason = *target.ExcludedReason
	}
	row.TerminalAt = target.TerminalAt
	if target.Independent != nil {
		row.Independent = target.Independent.Verdict
		row.IndependentRows = target.Independent.Rows
		row.IndependentLive = target.Independent.LiveProducers
	}
	return row
}

func deploymentIndependentFromSafe(summary operator.DeploymentIndependentSummary) deploymentIndependentSummary {
	view := deploymentIndependentSummary{
		OpenedTargets: summary.OpenedTargets, PassedTargets: summary.PassedTargets,
		LiveProducers: summary.LiveProducers,
		Verdicts:      make([]deploymentIndependentVerdictRow, 0, len(summary.Verdicts)),
	}
	// Only verdicts some target actually reached are shown. A row of six zeroes
	// is noise on a page whose question is "which targets did a second producer
	// speak for"; the JSON keeps every verdict for clients that compare shapes.
	for _, count := range summary.Verdicts {
		if count.Targets == 0 {
			continue
		}
		view.Verdicts = append(view.Verdicts,
			deploymentIndependentVerdictRow{Verdict: count.Verdict, Targets: count.Targets})
	}
	return view
}

func deploymentMaterialVersion(material operator.DeploymentMaterialSummary) string {
	if material.Status == operator.DeploymentMaterialRecorded && material.Version != nil {
		return *material.Version
	}
	return "未知"
}

func deploymentSafeJobCounts(deploymentID string, counts []operator.JobStateCount) []deploymentJobCount {
	byState := make(map[deploy.JobState]int, len(counts))
	for _, count := range counts {
		byState[count.State] = count.Count
	}
	return deploymentJobCounts(deploymentID, byState)
}

func deploymentActionView(action string, eligibility operator.DeploymentActionEligibility) deploymentActionEligibility {
	return deploymentActionEligibility{
		Eligible: eligibility.Eligible,
		Reason:   operator.DeploymentActionImpact(action, eligibility),
	}
}

func deploymentJobsURL(deploymentID string) string {
	query := make(url.Values)
	query.Set("deployment_id", deploymentID)
	return "/jobs?" + query.Encode()
}

func deploymentJobCounts(deploymentID string, counts map[deploy.JobState]int) []deploymentJobCount {
	rows := make([]deploymentJobCount, 0, len(deploy.AllJobStates))
	for _, state := range []deploy.JobState{
		deploy.Succeeded, deploy.Failed, deploy.ManualIntervention, deploy.Rejected,
		deploy.LeaseExpired, deploy.Running, deploy.Verifying, deploy.Claimed, deploy.NotStarted,
	} {
		if counts[state] == 0 {
			continue
		}
		query := make(url.Values)
		query.Set("deployment_id", deploymentID)
		query.Set("state", string(state))
		rows = append(rows, deploymentJobCount{State: state, Count: counts[state], Href: "/jobs?" + query.Encode()})
	}
	return rows
}

// deploymentCanarySilentFailures 讓最新一張 retry 的詳細頁和 promote gate 看同一組
// ancestry targets；舊 deployment 則仍只陳述自己當時的 snapshot，不能借用後來的結果。
func (s *Server) deploymentCanarySilentFailures(detail operator.DeploymentDetailResult, now time.Time) ([]rollout.SilentFailure, error) {
	item := detail.Item
	if item.Material.Status == operator.DeploymentMaterialRecorded && item.Material.Version != nil &&
		item.Material.ArtifactDigest != nil {
		facts, err := s.store.PromoteFacts(*item.Material.Version,
			strings.TrimPrefix(*item.Material.ArtifactDigest, "sha256:"), now)
		if err != nil {
			return nil, err
		}
		if facts.Canary != nil && facts.Canary.DeploymentID == item.DeploymentID {
			return facts.Silent, nil
		}
	}
	ids := make([]string, 0, len(detail.Targets))
	for _, target := range detail.Targets {
		ids = append(ids, target.MachineID)
	}
	return s.store.CanarySilentFailuresSince(ids, *item.FinishedAt, now)
}

func deploymentStatus(item operator.DeploymentSummary) string {
	switch item.State {
	case store.DeploymentPaused:
		if item.BoundaryPause != nil {
			return fmt.Sprintf("paused —— 安全閘門在批次 %d 後停住（%s）；後續批次沒有開始（沒有 auto-continue）",
				item.BoundaryPause.OpenedBatch, item.BoundaryPause.Kind)
		}
		return fmt.Sprintf("paused —— 批次 %d 失敗即停；後續批次沒有開始（沒有 auto-continue）", item.OpenedBatch)
	case store.DeploymentFinished:
		if item.Stuck > 0 {
			return fmt.Sprintf("finished，%d 台 stuck", item.Stuck)
		}
		return "finished。"
	default:
		return fmt.Sprintf("running；已開批次 %d / %d。", item.OpenedBatch, item.TotalBatches)
	}
}

func exclusionReason(reason string) string {
	switch reason {
	case "conflict":
		return "已有同一資源的非終態工作單；為避免兩張單同時改它而排除。"
	case "missing_package":
		return "目前量到的 Node 版本不符合 artifact 的 engines 要求。"
	case "unknown_node":
		return "Node 版本未知；artifact 相容性阻擋。"
	case "noncompliant":
		return "合規性動作生效中，這台領不到新工作單，所以不開單給它。"
	default:
		return "未識別的排除原因：" + reason
	}
}

func deploymentVersion(raw string) string {
	var spec model.OpenClawSpec
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.Version == "" {
		return "未知"
	}
	return spec.Version
}

func formatJobCounts(counts map[deploy.JobState]int) string {
	var parts []string
	// 已完成先講，讓 Hub 重啟後直接讀成「18 succeeded / 12 not_started」。
	for _, state := range []deploy.JobState{
		deploy.Succeeded, deploy.Failed, deploy.ManualIntervention, deploy.Rejected,
		deploy.LeaseExpired, deploy.Running, deploy.Verifying, deploy.Claimed, deploy.NotStarted,
	} {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}
	if len(parts) == 0 {
		return "0 jobs"
	}
	return strings.Join(parts, " / ")
}

func hubRestartCount(events []store.HubEvent) int {
	n := 0
	for _, event := range events {
		// ⚠ 只看列舉 kind；detail 是自由文字，掃它會把「started 不算」也算進來。
		if event.Kind == store.HubStarted {
			n++
		}
	}
	return n
}

type deploymentHeadlineRow struct {
	DeploymentID string
	Text         string
	Stuck        bool
	Unknown      bool
}

// deploymentHeadline 產生 SPEC §8.2 的第四句；多張 deployment 以換行分隔。
func deploymentHeadline(views []store.DeploymentView) string {
	rows := deploymentHeadlineRows(views)
	texts := make([]string, 0, len(rows))
	for _, row := range rows {
		texts = append(texts, row.Text)
	}
	return strings.Join(texts, "\n")
}

func deploymentHeadlineRows(views []store.DeploymentView) []deploymentHeadlineRow {
	active := make([]store.DeploymentView, 0, len(views))
	for _, view := range views {
		if view.State != store.DeploymentFinished {
			active = append(active, view)
		}
	}
	if len(active) == 0 {
		return []deploymentHeadlineRow{{Text: "沒有部署在跑。"}}
	}
	sort.SliceStable(active, func(i, j int) bool {
		iStuck := active[i].State == store.DeploymentPaused || active[i].Stuck > 0
		jStuck := active[j].State == store.DeploymentPaused || active[j].Stuck > 0
		if iStuck != jStuck {
			return iStuck
		}
		return active[i].CreatedAt.After(active[j].CreatedAt)
	})
	rows := make([]deploymentHeadlineRow, 0, len(active))
	for _, view := range active {
		isStuck := view.State == store.DeploymentPaused || view.Stuck > 0
		if !isStuck {
			rows = append(rows, deploymentHeadlineRow{DeploymentID: view.DeploymentID,
				Text: fmt.Sprintf("%s 部署 %s 在跑：%s。", view.Channel, short(view.DeploymentID, 8), formatJobCounts(view.Counts))})
			continue
		}
		if view.BoundaryPause != nil {
			rows = append(rows, deploymentHeadlineRow{DeploymentID: view.DeploymentID, Stuck: true,
				Text: fmt.Sprintf("部署卡住 —— %s %s：安全閘門在批次 %d 後停住（%s）",
					view.Channel, short(view.DeploymentID, 8), view.BoundaryPause.OpenedBatch, view.BoundaryPause.Reason)})
			continue
		}
		var machines []string
		for _, target := range view.Targets {
			if target.StuckKind == "" {
				continue
			}
			if len(machines) == 3 {
				break
			}
			if target.StuckKind == "no_event" {
				machines = append(machines, fmt.Sprintf("%s 15 分鐘沒事件（%s）", target.DisplayName, target.JobState))
			} else {
				machines = append(machines, fmt.Sprintf("%s %s（%s）", target.DisplayName, operator.TerminalJobOutcome(target.JobState), target.JobState))
			}
		}
		if more := view.Stuck - len(machines); more > 0 {
			machines = append(machines, fmt.Sprintf("+%d 台", more))
		}
		if len(machines) == 0 {
			machines = append(machines, "部署已暫停，但帳本沒有列出 stuck 機器")
		}
		rows = append(rows, deploymentHeadlineRow{DeploymentID: view.DeploymentID, Stuck: true,
			Text: fmt.Sprintf("部署卡住 —— %s %s：%s。", view.Channel, short(view.DeploymentID, 8), strings.Join(machines, "、"))})
	}
	return rows
}

type updatesPage struct {
	Artifacts     []updateArtifactRow
	ArtifactTotal int
	Channels      []channelUpdate
	NextHref      string
	StartOverHref string
	Continuation  bool
}

type updatesPageRequest struct {
	Section string
	Cursor  string
}

type channelUpdate struct {
	Name     string
	Members  []operator.UpdateMachineSummary
	Latest   *operator.DeploymentSummary
	Previews []channelPreview
}

type updateArtifactRow struct {
	ArtifactID                 string
	Version                    string
	SHA256                     string
	Size                       int64
	SizeKnown                  bool
	EnginesNode                string
	FetchedAt                  *time.Time
	Status                     operator.ArtifactReadStatus
	Issue                      string
	DeploymentReferences       int
	ActiveDeploymentReferences int
}

type channelPreview struct {
	Artifact         updateArtifactRow
	Summary          string
	Promote          string
	PromotionTargets []operator.DeploymentPromotionIndependentTargetPreview
}

func (s *Server) updates(w http.ResponseWriter, r *http.Request) {
	pageRequest, err := parseUpdatesPageRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	request := operator.UpdateReadRequest{Limit: operator.MaxArtifactReadLimit, Cursor: pageRequest.Cursor}
	if pageRequest.Section == "channel-canary" {
		request.Channel = "canary"
	} else if pageRequest.Section == "channel-stable" {
		request.Channel = "stable"
	}
	result, err := s.operator.Updates(request, now)
	if err != nil {
		if errors.Is(err, operator.ErrInvalidUpdateRead) {
			http.Error(w, "更新 cursor 不合法或不屬於目前 filter。", http.StatusBadRequest)
			return
		}
		s.fail(w, "讀取 updates 原生模型失敗", err)
		return
	}
	view := &updatesPage{
		Artifacts:     make([]updateArtifactRow, 0, len(result.Artifacts.Items)),
		ArtifactTotal: result.Artifacts.Total, Continuation: pageRequest.Cursor != "",
	}
	if result.Artifacts.NextCursor != nil {
		view.NextHref = updatesPageURL(pageRequest.Section, *result.Artifacts.NextCursor)
	}
	if pageRequest.Cursor != "" {
		view.StartOverHref = updatesPageURL(pageRequest.Section, "")
	}
	artifactRows := make(map[string]updateArtifactRow, len(result.Artifacts.Items))
	for _, item := range result.Artifacts.Items {
		row := updateArtifactRow{
			ArtifactID: item.ArtifactID, Version: pointerString(item.Version, "未知"),
			SHA256: "未知", EnginesNode: pointerString(item.EnginesNode, "未宣告"),
			FetchedAt: item.FetchedAt, Status: item.Status,
			DeploymentReferences: item.DeploymentReferences, ActiveDeploymentReferences: item.ActiveDeploymentReferences,
		}
		if item.SHA256 != nil {
			row.SHA256 = *item.SHA256
		}
		if item.SizeBytes != nil {
			row.Size = *item.SizeBytes
			row.SizeKnown = true
		}
		if item.Issue != nil {
			row.Issue = string(*item.Issue)
		}
		view.Artifacts = append(view.Artifacts, row)
		artifactRows[item.ArtifactID] = row
	}
	for _, channel := range result.Channels {
		block := channelUpdate{Name: channel.Name, Members: channel.Members, Latest: channel.LatestDeployment}
		for _, item := range channel.Previews {
			preview := channelPreview{Artifact: artifactRows[item.Artifact.ArtifactID]}
			if item.Plan == nil {
				preview.Summary = "無法建立 rollout 預覽（" + strings.Join(item.Blockers, "、") + "）"
			} else {
				preview.Summary = fmt.Sprintf("影響 %d 台，衝突 %d，缺套件 %d，unreachable %d",
					item.Plan.Impact, item.Plan.Conflicts, item.Plan.MissingPackages, item.Plan.Unreachable)
				if item.Plan.UnknownNodes > 0 {
					preview.Summary += fmt.Sprintf("，node 版本未知 %d", item.Plan.UnknownNodes)
				}
			}
			if item.Promotion != nil {
				if item.Promotion.Allowed {
					preview.Promote = "stable promote gate 已通過"
				} else {
					preview.Promote = "stable promote 鎖著（" + strings.Join(item.Promotion.Blockers, "、") + "）"
				}
				if item.Promotion.EarliestAt != nil {
					preview.Promote += "；最早 " + item.Promotion.EarliestAt.Local().Format("2006-01-02 15:04")
				}
				preview.Promote += fmt.Sprintf("；跨故障域 verifier %d / %d 台通過",
					item.Promotion.IndependentPassedTargets, len(item.Promotion.IndependentTargets))
				preview.PromotionTargets = item.Promotion.IndependentTargets
			}
			block.Previews = append(block.Previews, preview)
		}
		view.Channels = append(view.Channels, block)
	}
	s.render(w, r, "updates.html", page{Title: "更新", Nav: "updates", Now: now.Local().Format("2006-01-02 15:04"), Updates: view})
}

func parseUpdatesPageRequest(r *http.Request) (updatesPageRequest, error) {
	if r == nil || r.URL == nil {
		return updatesPageRequest{}, errors.New("更新查詢不存在。")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return updatesPageRequest{}, errors.New("更新查詢不可是空的問號。")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return updatesPageRequest{}, errors.New("更新查詢格式不合法。")
	}
	for key := range values {
		if key != "section" && key != "cursor" {
			return updatesPageRequest{}, fmt.Errorf("不支援更新查詢參數 %q。", key)
		}
	}
	result := updatesPageRequest{}
	if raw, present := values["section"]; present {
		if len(raw) != 1 || !validUpdatesPageQueryValue(raw[0], 32) {
			return updatesPageRequest{}, errors.New("更新 section 必須恰有一個 canonical 值。")
		}
		switch raw[0] {
		case "artifacts", "channel-canary", "channel-stable":
			result.Section = raw[0]
		default:
			return updatesPageRequest{}, errors.New("更新 section 不合法。")
		}
	}
	if raw, present := values["cursor"]; present {
		if len(raw) != 1 || !validUpdatesPageQueryValue(raw[0], 2048) {
			return updatesPageRequest{}, errors.New("更新 cursor 必須恰有一個 canonical 值。")
		}
		result.Cursor = raw[0]
	}
	return result, nil
}

func validUpdatesPageQueryValue(value string, maxBytes int) bool {
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

func updatesPageURL(section, cursor string) string {
	values := make(url.Values)
	if section != "" {
		values.Set("section", section)
	}
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	fragment := section
	if fragment == "" {
		fragment = "artifacts"
	}
	return (&url.URL{Path: "/updates", RawQuery: values.Encode(), Fragment: fragment}).String()
}

func pointerString(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

func isClawctlRelease(install model.OpenClawInstall) bool {
	if install.RunningDir == "" || install.ReleasesDir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(install.ReleasesDir), filepath.Clean(install.RunningDir))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
