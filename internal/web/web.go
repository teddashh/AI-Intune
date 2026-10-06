// Package web 是 Hub 的網頁介面。
//
// 設計規則（docs/SPEC.md §8）：
//
//  1. **文案必須是句子。** 「samplehub1 32 秒前回報」比一顆綠點有用一百倍。
//     一排彩色圓點需要人自己翻譯成意思，而人在早上不會翻譯。
//  2. **每一盞燈都要點得進去看到證據。** 點不進去的燈等於沒有燈。
//  3. **L1 的綠燈只准說「最近有跑完」，不准說「健康」或「成功」。**
//     實測上游 status='ok' 有 64% 的 summary 在描述失敗。
//  4. **沒做到的功能不畫灰按鈕。** 灰按鈕會讓人每次都重新確認一次它還是不能按。
//
// 伺服器端渲染、零 JavaScript build step。這不是偷懶 —— 複雜度的上限是
// 「半夜一個人修得動」，而一個需要 npm install 才能改一行字的儀表板，
// 半夜是修不動的。
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	// drill 讀還原演練的章；nil = 不看。用 SetDrillStampReader 塞。
	drill DrillStampReader
	store *store.Store
	// operator is the canonical human-control read/use-case boundary. HTML is
	// an adapter over the same machine queries used by JSON and the official
	// CLI; it must not grow a second interpretation of fleet state.
	operator *operator.Service
	// artifactOperator is the same canonical service behind operator.  Keeping
	// the narrow port separate lets the HTML adapter be exercised without a
	// live registry; production always installs the concrete operator.Service.
	artifactOperator artifactWebOperator
	// catalogOperator owns Standard Store, profile publication, and exact
	// machine assignment previews/mutations through the same service instance.
	catalogOperator catalogWebOperator
	tmpl            *template.Template
	hubHost         string
	// hubBase is trusted deployment configuration used in generated agent
	// commands. It must never be inferred from the caller-controlled HTTP Host.
	hubBase string
	// artifactsDir 由 Hub 啟動路徑注入；空字串等同尚未 fetch。
	artifactsDir string
	// agentBundles is the immutable two-architecture bootstrap release owned by
	// the running Hub version.
	agentBundles *agentBundleCatalog
	// tailnet 是名冊的**獨立證人**。名冊是我們自己寫進去的，
	// 所以它答不出「有沒有一台機器存在、而我忘了把它放進名冊」。
	tailnet *tailnet.Cache
	// retentionPolicy is the same validated runtime policy used by the
	// scheduled maintenance loop and the operator JSON boundary.
	retentionPolicy store.RetentionPolicy
}

func New(s *store.Store, hubHost string) (*Server, error) {
	t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	cache := tailnet.NewCache()
	op := operator.NewWithTailnet(s, cache)
	return &Server{
		store: s, operator: op, artifactOperator: op, catalogOperator: op, tmpl: t, hubHost: hubHost,
		tailnet: cache, retentionPolicy: store.DefaultRetention(),
	}, nil
}

// SetTailnetStatus 釘住 tailnet 那一份答案。**只給測試用。**
// ⚠ 不釘的話，畫面上「名冊之外」那一段的內容取決於跑測試那台機器的
// tailnet 狀態。理由與代價寫在 tailnet.Cache.SetStatus 的註解裡。
func (s *Server) SetTailnetStatus(st tailnet.Status) {
	s.tailnet.SetStatus(st)
	s.SetTailnetCache(s.tailnet)
}

func (s *Server) SetTailnetCache(cache *tailnet.Cache) {
	if cache == nil {
		cache = tailnet.NewCache()
	}
	s.tailnet = cache
	op := operator.NewControlPlane(s.store, s.artifactsDir, cache)
	s.operator, s.artifactOperator, s.catalogOperator = op, op, op
}

// SetArtifactsDir tells both presentation-only artifact views and the
// canonical operator service where Hub-verified deployment material lives.
// Keeping one service instance without the catalog would make HTML preview
// disagree with the JSON/CLI adapters over whether apply is possible.
func (s *Server) SetArtifactsDir(dir string) {
	s.artifactsDir = dir
	op := operator.NewControlPlane(s.store, dir, s.tailnet)
	s.operator = op
	s.artifactOperator = op
	s.catalogOperator = op
}

// SetOperatorService installs the Hub-owned control-plane service after all
// runtime-only backends have been configured on it.
func (s *Server) SetOperatorService(service *operator.Service) {
	if service == nil {
		return
	}
	s.operator, s.artifactOperator, s.catalogOperator = service, service, service
}

// SetHubBase sets the configured address agents should use to reach this Hub.
// The serve path supplies the same publicBase used by reports.
func (s *Server) SetHubBase(base string) { s.hubBase = strings.TrimRight(base, "/") }

// SetRetentionPolicy installs the already-validated runtime policy used by
// automatic and operator-initiated maintenance. An invalid policy is rejected
// instead of silently changing what the review page says will be deleted.
func (s *Server) SetRetentionPolicy(policy store.RetentionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	s.retentionPolicy = policy
	return nil
}

func (s *Server) Routes(mux *http.ServeMux) []string {
	patterns := []string{
		"GET /{$}",
		"GET /machines",
		"GET /machines/enrollment",
		"GET /downloads/agent/{arch}",
		"GET /machines/lifecycle",
		"GET /machines/configuration",
		"GET /machines/compliance",
		"GET /machines/diagnostics",
		"GET /machines/{id}",
		"GET /jobs",
		"GET /jobs/{id}",
		"GET /apps",
		"GET /deployments",
		"GET /deployments/{id}",
		"GET /updates",
		"GET /reports",
		"GET /reports/changes",
		"GET /reports/tickets",
		"GET /reports/tickets.csv",
		"GET /machines/{id}/timeline",
		"GET /machines/{id}/timeline.csv",
		"GET /apps/artifacts/{id}",
		"GET /apps/artifact-fetches/{id}",
		"GET /settings/tailnet",
		"GET /tenant/maintenance",
		"GET /tenant/maintenance/restore-drills/{id}",
		"GET /tenant/data",
		"GET /machines/{id}/data",
		"GET /machines/{id}/data.csv",
		"GET /reports/enrollment",
		"GET /reports/enrollment.csv",
		"GET /reports/software",
		"GET /reports/software.csv",
		"GET /reports/install",
		"GET /reports/install.csv",
		"GET /reports/profile",
		"GET /reports/profile.csv",
		"GET /preferences/navigation-language/{locale}",
	}
	mux.HandleFunc(patterns[0], s.dashboard)
	mux.HandleFunc(patterns[1], s.dashboard)
	mux.HandleFunc(patterns[2], s.enrollment)
	mux.HandleFunc(patterns[3], s.downloadAgentBundle)
	mux.HandleFunc(patterns[4], s.lifecycle)
	mux.HandleFunc(patterns[5], s.configuration)
	mux.HandleFunc(patterns[6], s.compliance)
	mux.HandleFunc(patterns[7], s.diagnostics)
	mux.HandleFunc(patterns[8], s.machine)
	mux.HandleFunc(patterns[9], s.jobs)
	mux.HandleFunc(patterns[10], s.job)
	mux.HandleFunc(patterns[11], s.appsSurface)
	mux.HandleFunc(patterns[12], s.deployments)
	mux.HandleFunc(patterns[13], s.deployment)
	mux.HandleFunc(patterns[14], s.updates)
	mux.HandleFunc(patterns[15], s.reports)
	mux.HandleFunc(patterns[16], s.changesPage)
	mux.HandleFunc(patterns[17], s.tickets)
	mux.HandleFunc(patterns[18], s.ticketsCSV)
	mux.HandleFunc(patterns[19], s.machineTimeline)
	mux.HandleFunc(patterns[20], s.machineTimelineCSV)
	mux.HandleFunc(patterns[21], s.artifactDetail)
	mux.HandleFunc(patterns[22], s.artifactFetchOperationDetail)
	mux.HandleFunc(patterns[23], s.tailnetSettings)
	mux.HandleFunc(patterns[24], s.maintenance)
	mux.HandleFunc(patterns[25], s.restoreDrillOperation)
	mux.HandleFunc(patterns[26], s.dataDisclosure)
	mux.HandleFunc(patterns[27], s.machineData)
	mux.HandleFunc(patterns[28], s.machineDataCSV)
	mux.HandleFunc(patterns[29], s.enrollmentReport)
	mux.HandleFunc(patterns[30], s.enrollmentReportCSV)
	mux.HandleFunc(patterns[31], s.softwareReport)
	mux.HandleFunc(patterns[32], s.softwareReportCSV)
	mux.HandleFunc(patterns[33], s.installReport)
	mux.HandleFunc(patterns[34], s.installReportCSV)
	mux.HandleFunc(patterns[35], s.profileReport)
	mux.HandleFunc(patterns[36], s.profileReportCSV)
	mux.HandleFunc(patterns[37], s.setNavigationLanguage)
	// ⚠ 寫入路徑全部在 actions.go，而且全部是 POST。理由寫在那個檔案的開頭。
	return append(patterns, s.actionRoutes(mux)...)
}

type maintenancePageView struct {
	Status            operator.RetentionStatus
	Observations      string
	Checkins          string
	Occupancy         string
	LastRun           string
	Restore           restoreDrillNote
	RestoreOperations store.RestoreDrillListResult
	RestoreActive     bool
	DiskClean         []store.DiskCleanSummaryView
}

func (s *Server) maintenance(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	status, err := s.operator.RetentionStatus(s.retentionPolicy, now)
	if err != nil {
		s.fail(w, "讀取維護狀態失敗", err)
		return
	}
	restoreOperations, err := s.operator.RestoreDrillOperations(5)
	if err != nil {
		s.fail(w, "讀取還原演練狀態失敗", err)
		return
	}
	restoreActive := false
	for _, operation := range restoreOperations.Items {
		if operation.State == store.RestoreDrillQueued || operation.State == store.RestoreDrillRunning {
			restoreActive = true
		}
	}
	diskClean, err := s.operator.DiskCleanSummaries(now)
	if err != nil {
		s.fail(w, "讀取磁碟清理狀態失敗", err)
		return
	}
	lastRun := "尚未執行過清理"
	if status.HasRun && status.LastPruneAt != nil {
		lastRun = fmt.Sprintf("%s，刪除 %d 列", status.LastPruneAt.Local().Format("2006-01-02 15:04:05"), status.LastPruneRows)
	}
	s.render(w, r, "maintenance.html", page{
		Title: "維護", Nav: "tenant-maintenance", Now: now.Local().Format("2006-01-02 15:04"),
		Maintenance: &maintenancePageView{
			Status: status, Observations: state.HumanDur(s.retentionPolicy.Observations),
			Checkins: state.HumanDur(s.retentionPolicy.Checkins), Occupancy: state.HumanDur(s.retentionPolicy.Occupancy),
			LastRun: lastRun, Restore: s.restoreDrill(now),
			RestoreOperations: restoreOperations, RestoreActive: restoreActive,
			DiskClean: diskClean,
		},
	})
}

func (s *Server) restoreDrillOperation(w http.ResponseWriter, r *http.Request) {
	operation, err := s.operator.RestoreDrillOperation(r.PathValue("id"))
	if errors.Is(err, store.ErrRestoreDrillNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "讀取還原演練 operation 失敗", err)
		return
	}
	s.render(w, r, "maintenance_restore_drill_operation.html", page{
		Title: "還原演練", Nav: "tenant-maintenance", Now: time.Now().Local().Format("2006-01-02 15:04"),
		RestoreDrillOperation: &operation,
	})
}

func (s *Server) tailnetSettings(w http.ResponseWriter, r *http.Request) {
	result, err := s.operator.Tailnet(r.Context(), time.Now().UTC())
	if err != nil {
		s.fail(w, "讀取 Tailnet 狀態失敗", err)
		return
	}
	s.render(w, r, "tailnet.html", page{
		Title: "Tailnet", Nav: "tailnet", Now: time.Now().Local().Format("2006-01-02 15:04"),
		TailnetSettings: &result,
	})
}

// enrollment is the discoverable home for the already-canonical enrollment
// preview/create flow.  The page itself is read-only; its forms still enter
// through actions.go and the operator service, so moving the UI does not invent
// a second enrollment contract.
func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	machines, err := s.operator.ListMachines(now)
	if err != nil {
		s.fail(w, "讀取機器名冊失敗", err)
		return
	}
	ov, ok := machines.StoreOverview()
	if !ok {
		s.fail(w, "讀取機器名冊失敗", errors.New("operator machine overview bridge unavailable"))
		return
	}
	limit, err := s.operator.EnrollmentLimit(now)
	if err != nil {
		s.fail(w, "讀取註冊上限失敗", err)
		return
	}
	s.render(w, r, "enrollment.html", page{
		Title:           "裝置註冊",
		Nav:             "machines-enrollment",
		Now:             now.Local().Format("2006-01-02 15:04"),
		Overview:        ov,
		Tailnet:         s.tailnetResult(r.Context(), ov),
		EnrollmentName:  strings.TrimSpace(r.URL.Query().Get("name")),
		EnrollmentLimit: &limit,
	})
}

// page 是兩個頁面共用的資料袋。用 struct 而不是 map 是為了讓樣板打錯字時
// 在測試裡爆炸，而不是在畫面上安靜地變成空字串 —— 一個安靜消失的欄位，
// 在這個產品裡跟一台安靜消失的機器是同一種 bug。
type page struct {
	Title                 string
	Nav                   string
	Now                   string
	Locale                navigationLocaleView
	Access                accessView
	SubNav                subNavigation
	TailnetSettings       *operator.TailnetOverview
	TailnetIgnorePreview  *operator.TailnetPeerIgnorePreview
	Maintenance           *maintenancePageView
	RetentionPrunePreview *operator.RetentionPrunePreview
	RetentionPruneReason  string
	RestoreDrillPreview   *operator.RestoreDrillPreview
	RestoreDrillReason    string
	RestoreDrillOperation *store.RestoreDrillOperation
	IdempotencyKey        string
	// VerifierAssignmentPreview is the confirmation page for asking a second
	// producer to look at one job. It carries no command, because the Hub does
	// not decide what the verifier measures.
	VerifierAssignmentPreview *verifierAssignmentPreviewView

	// Configuration is Devices > Configuration: the settings every agent is
	// meant to run, beside the settings each one reported running.
	Configuration           *configurationPageView
	SettingPolicyReview     *settingPolicyReviewPage
	SettingAssignmentReview *settingAssignmentReviewPage

	// Compliance is Devices > Compliance: the conditions an operator set,
	// beside what each machine actually reported measuring up to.
	Compliance                 *compliancePageView
	CompliancePolicyReview     *compliancePolicyReviewPage
	ComplianceAssignmentReview *complianceAssignmentReviewPage

	// Dashboard
	Overview     store.Overview
	Dashboard    dashboardStats
	MachineTable dashboardMachineTable
	MachineIndex bool
	MachineList  *machineIndexPageView
	Headline     []string
	HubInFleet   string
	// HubEvents：Hub 這 24 小時對自己的日誌（重啟、迴圈卡住、時鐘跳動）。
	// ⚠ 空的意思是「沒記到」，不是「沒事」。網路斷線它自己看不到。
	HubEvents []store.HubEvent
	// RestoreDrill：上一次還原演練。Known=false 是 Hub 沒在看章。
	RestoreDrill restoreDrillNote
	// DeadSignals：已經死掉的證據管線。⚠ 空的不代表證據健康，
	// 只代表那幾條沒抓到問題。畫面上的字必須跟著這樣寫。
	DeadSignals []store.DeadSignal
	// DoubleAgents：一台機器上不只一個 agent 在回報。
	// ⚠ 跟 DeadSignals 一樣，空的不代表「每台都只有一個」——
	// 舊版 agent 不送 agent_started_at，那些機器上有幾個我們看不出來。
	DoubleAgents []store.DoubleAgent
	// DeploymentHeadlines 是 SPEC §8.2 的第四句；卡住的部署先講。
	DeploymentHeadlines []deploymentHeadlineRow
	// Tailnet：名冊 vs tailnet 的對照。⚠ Available=false 的時候，
	// 空的 Unenrolled 意思是「我沒問到」，不是「沒有名冊外的機器」。
	Tailnet tailnet.Result

	// Tools 是工具 × 機器的版本分佈。⚠ 它只講「這個機隊裡誰最新」，
	// 不講「該不該升級」—— 我們沒有上游的版本來源。它跟軟體清查那一頁讀的是
	// 同一份 operator.SoftwareReport，見 software_report.go。
	Tools softwareMatrix

	// 報告
	Reports          *reportIndexView
	MachineTimeline  *machineTimelineView
	DataDisclosure   *dataDisclosureView
	MachineData      *machineDataView
	EnrollmentReport *enrollmentReportView
	SoftwareReport   *softwareReportView
	InstallReport    *installReportView
	ProfileReport    *profileReportView
	// EnrollmentLimit 是裝置註冊那一頁最上面那一段：這個 Hub 還收不收得下一台。
	EnrollmentLimit *operator.EnrollmentLimitResult
	// EnrollmentLimitReview 只在確認頁上有值。
	EnrollmentLimitReview *operator.EnrollmentLimitPreview
	EnrollmentLimitReason string
	Tickets               *ticketPageView
	Changes               *changePageView
	Lifecycle             *lifecyclePageView
	Diagnostics           *diagnosticPageView
	DiagnosticPreview     *diagnosticPreviewView

	// 單機頁
	// MachineDetail owns the typed monitor, identity, resource, history and
	// expectation sections.
	MachineDetail *operator.MachineDetailResult
	Machine       machinePageMachine
	// MachineConnect is the separate typed BFF disclosure for actionable BAT
	// coordinates. It is deliberately absent from MachineDetail JSON.
	MachineConnect *operator.MachineConnectResult
	// MachineActions is the device action catalogue: what this operator can do
	// to this machine right now. It is nil when their capabilities cover none
	// of them, so the page has no 動作 section at all rather than an empty one.
	MachineActions *machineActionsView
	// MachineRenamePreview is populated only on the rename review page.
	MachineRenamePreview *operator.MachineRenamePreviewResult
	MachineRenameReason  string
	// MachineNotesPreview is populated only on the registry-notes review page.
	MachineNotesPreview *operator.MachineNotesPreviewResult
	MachineNotesReason  string
	// PendingEnrollment comes from the dedicated operator read contract. It is
	// nil only when the registry machine has no unused enrollment ticket; token
	// plaintext and hashes never enter this page model.
	PendingEnrollment *operator.EnrollTokenStatusResult
	// MachineEvidence is the bounded typed disclosure for credentials,
	// occupancy, systemd observations and the two free-text sections that no
	// longer travel through StoreDetail.
	MachineEvidence *operator.MachineEvidenceResult
	// MachineJournals is prepared in Go so the template only performs a unit
	// lookup; it never rebuilds evidence relationships from display text.
	MachineJournals         map[string]operator.MachineJournal
	L2NotWired              bool
	Strip                   []Block
	StripTiers              []StripTier
	CheckinWindowText       string
	Schedule                Schedule
	MachineMonitorInterval  time.Duration
	MachineMonitorClockSkew *time.Duration
	LatestMachineCheckin    *operator.MachineCheckin
	Jobs                    []operator.JobSummary
	// ChannelIdempotencyKey is minted per machine-page render. Resubmitting the
	// same rendered form replays; reloading gets a fresh operator attempt.
	ChannelIdempotencyKey string
	LifecyclePreview      *lifecyclePreviewView

	// 工作單證據頁
	Job     *jobPage
	JobList *jobsPage

	// Phase 4 編排頁。
	DeploymentList          *deploymentsPage
	Deployment              *deploymentPage
	DeploymentCreateReview  *deploymentCreateReview
	DeploymentActionReview  *deploymentActionReview
	Updates                 *updatesPage
	Apps                    *appsSurfacePage
	ArtifactDetail          *artifactDetailPage
	ArtifactFetchReview     *artifactFetchReviewPage
	ArtifactFetchOperation  *artifactFetchOperationPage
	CatalogPackageReview    *catalogPackageReviewPage
	CatalogProfileReview    *catalogProfileReviewPage
	CatalogAssignmentReview *catalogAssignmentReviewPage

	// Retention 是「這一頁看到的資料往回只到哪裡」。
	//
	// ⚠ 它必須出現在畫面上。一頁只剩 14 天心跳的機器，
	// 看起來跟一台 14 天前才出生的機器一模一樣 ——
	// 而「資料被清掉了」跟「這段時間沒有資料」是完全不同的兩件事。
	Retention retentionNote

	// 動作紀錄
	Audit *auditPageView
	// Action 只在「按了但沒做成」的那一頁有值。
	Action *actionResult
	// EnrollPreview freezes the exact impact reviewed by the operator and the
	// one request key that the confirmation POST must reuse.
	EnrollPreview *enrollPreviewResult
	// EnrollTokenRevocationPreview freezes the exact pending ticket and makes
	// the non-effects on registry membership and active agent credentials
	// explicit before confirmation.
	EnrollTokenRevocationPreview *enrollTokenRevocationPreviewResult
	// Token 只在剛開完票的那一頁有值。⚠ 它帶著明文 token，
	// 而且只有 fresh create 才能帶；replay 只 render recovery instructions。
	Token *tokenResult

	// EnrollmentName is an optional convenience value from a machine-page CTA.
	// It is never a confirmation value: the preview service still validates the
	// submitted display name and the create step freezes its own digest/key.
	EnrollmentName string
	AgentBundles   []agentBundleView
}

type machinePageMachine struct {
	MachineID         string
	DisplayName       string
	Channel           string
	ChannelRevision   int64
	LifecycleRevision int64
	RetiredAt         *time.Time
}

func machinePageMachineFrom(detail operator.MachineDetailResult,
	lifecycle operator.MachineLifecycleReadResult,
) (machinePageMachine, error) {
	if detail.Item.MachineID == "" || lifecycle.MachineID != detail.Item.MachineID ||
		lifecycle.LifecycleRevision < 0 || lifecycle.ChannelRevision < 0 {
		return machinePageMachine{}, errors.New("machine page registry projections are inconsistent")
	}
	return machinePageMachine{
		MachineID: detail.Item.MachineID, DisplayName: detail.Item.DisplayName,
		Channel: lifecycle.Channel, ChannelRevision: lifecycle.ChannelRevision,
		LifecycleRevision: lifecycle.LifecycleRevision, RetiredAt: lifecycle.RetiredAt,
	}, nil
}

// subNavigation is the common second level below the product-wide sidebar.
// Only already-delivered destinations belong here; an empty or disabled link
// would make operators re-discover on every visit that the function is absent.
type subNavigation struct {
	Label string
	Title string
	Items []subNavigationItem
}

type subNavigationItem struct {
	Label       string
	Href        string
	Section     string
	Current     bool
	CurrentKind string
}

func makeSubNavigation(label, current string, items ...subNavigationItem) subNavigation {
	for i := range items {
		items[i].Current = items[i].Label == current
		items[i].CurrentKind = "page"
		if hash := strings.LastIndex(items[i].Href, "#"); hash >= 0 && hash+1 < len(items[i].Href) {
			items[i].Section = items[i].Href[hash+1:]
			base := items[i].Href[:hash]
			separator := "?"
			if strings.Contains(base, "?") {
				separator = "&"
			}
			items[i].Href = base + separator + "section=" + url.QueryEscape(items[i].Section) + "#" + items[i].Section
			items[i].CurrentKind = "location"
		}
	}
	return subNavigation{Label: label, Title: subNavigationTitle(label), Items: items}
}

func subNavigationTitle(label string) string {
	switch label {
	case "裝置子選單", "裝置詳細資料子選單", "更新子選單":
		return "裝置"
	case "代理程式子選單", "代理程式活動詳細資料子選單":
		return "代理程式"
	case "疑難排解子選單":
		return "疑難排解 + 支援"
	case "應用子選單", "部署子選單":
		return "應用程式"
	case "部署詳細資料子選單":
		return "部署"
	case "報告子選單":
		return "報告"
	case "租用戶子選單":
		return "租用戶管理"
	default:
		return "AI-Intune"
	}
}

func localizedAppsLabel(label string) string {
	switch label {
	case "Store":
		return "應用程式目錄"
	case "Profiles":
		return "設定檔"
	case "Assignments":
		return "指派"
	case "Artifacts":
		return "安裝套件"
	case "Fetch":
		return "套件擷取"
	case "Operations":
		return "作業"
	default:
		return label
	}
}

func selectSubNavigationLocation(nav *subNavigation, section string) {
	section = strings.TrimSpace(section)
	if nav == nil || section == "" {
		return
	}
	selected := -1
	for i := range nav.Items {
		if nav.Items[i].Section == section {
			if selected == -1 || nav.Items[i].Current {
				selected = i
			}
		}
	}
	if selected == -1 {
		return
	}
	for i := range nav.Items {
		nav.Items[i].Current = i == selected
	}
}

type accessView struct {
	Known       bool
	Login       string
	Device      string
	Subject     string
	Permissions string
	Attribution string
	CanView     bool
	CanOperate  bool
	CanAdmin    bool
}

func accessFromRequest(r *http.Request) accessView {
	if r == nil {
		return accessView{}
	}
	principal, ok := operatorauth.PrincipalFromContext(r.Context())
	if !ok {
		return accessView{}
	}
	return accessView{
		Known: true, Login: principal.TailnetUserLogin, Device: principal.DeviceName,
		Subject: principal.StableSubject(), Permissions: principal.PermissionLabel(),
		Attribution: principal.Attribution(), CanView: principal.Has(operatorauth.View),
		CanOperate: principal.Has(operatorauth.Operate), CanAdmin: principal.Has(operatorauth.Admin),
	}
}

// dashboardStats 是總覽第一屏的 rollup。模板只呈現，不從敘述句反推數字。
// Reporting 只看 Hub 是否仍持有 freshness window 內的心跳。尤其不能只看
// identity_conflict state：identity hint 是歷史證據，衝突可以比最後心跳活得更久。
// Attention 只算分母內且不是 Online 的機器；分母是全部未退役名冊列。
type dashboardStats struct {
	Expected          int
	Reporting         int
	Attention         int
	Findings          int
	ActiveDeployments int
	StuckDeployments  int
	DeploymentsKnown  bool
}

// dashboardMachineTable 是總覽名冊表格自己的 view。它只縮小表格列，絕不改
// Overview；KPI、headline、findings 因而仍以完整且各自明定的 scope 計算。
type dashboardMachineTable struct {
	Filter      string
	FilterLabel string
	Rows        []store.MachineRow
}

func buildDashboardMachineTable(ov store.Overview, filter string) dashboardMachineTable {
	table := dashboardMachineTable{Rows: ov.Machines}
	var include func(store.MachineRow) bool
	switch filter {
	case "managed":
		table.Filter, table.FilterLabel = filter, "名冊機器（分母內）"
		include = func(machine store.MachineRow) bool { return machine.InDenominator() }
	case "reporting":
		table.Filter, table.FilterLabel = filter, "正在回報"
		include = func(machine store.MachineRow) bool {
			return machine.InDenominator() && state.IsReportingAt(machine.Facts, ov.Now)
		}
	case "attention":
		table.Filter, table.FilterLabel = filter, "需要查看"
		include = func(machine store.MachineRow) bool {
			return machine.InDenominator() && machine.State != state.Online
		}
	default:
		return table
	}

	table.Rows = make([]store.MachineRow, 0, len(ov.Machines))
	for _, machine := range ov.Machines {
		if include(machine) {
			table.Rows = append(table.Rows, machine)
		}
	}
	return table
}

func buildDashboardStats(ov store.Overview, deployments []store.DeploymentView, deploymentsKnown bool) dashboardStats {
	stats := dashboardStats{
		Expected:         ov.Expected,
		Reporting:        ov.ReportingCount(),
		Findings:         len(ov.Findings),
		DeploymentsKnown: deploymentsKnown,
	}
	for _, machine := range ov.Machines {
		if !machine.InDenominator() {
			continue
		}
		if machine.State != state.Online {
			stats.Attention++
		}
	}
	for _, deployment := range deployments {
		if deployment.State == store.DeploymentFinished {
			continue
		}
		stats.ActiveDeployments++
		if deployment.State == store.DeploymentPaused || deployment.Stuck > 0 {
			stats.StuckDeployments++
		}
	}
	return stats
}

// Schedule 是「下一次心跳該什麼時候到、幾點會被判失聯」。
//
// ⚠ 這一區是看著一次失聯演練加上去的，理由值得寫下來：
//
// 演練跑到一半我去看畫面，Hub 說 Degraded，而我知道那台的 agent 已經停了。
// 看起來就是「機器死了但 Hub 沒發現」—— 正是這個產品要修的那個 bug。
// 我開始查資料庫、懷疑時區。實際上只是還沒到 210 秒的門檻。
//
// **一個有寬限期的判定，在寬限期內看起來跟壞掉一模一樣。** 只寫「現在是
// Degraded」的畫面，會讓每一個在寬限期內看它的人得出跟我一樣的錯誤結論。
// 所以要把門檻的時間點畫出來 —— 讓人能分辨「還沒到」跟「不會到」。
type Schedule struct {
	// Known 為 false 代表這台從來沒報到過，沒有「下一次」可言。
	Known bool
	// DueAt 是下一次心跳的預定時間；Overdue 代表它已經遲到了。
	DueAt   time.Time
	Overdue bool
	// UnreachableAt 是「再沒消息就判失聯」的那一刻。
	UnreachableAt time.Time
	// CountdownText 用人話講還剩多久，或已經超過多久。
	CountdownText string
}

func scheduleOf(f state.Facts, now time.Time) Schedule {
	if !f.EverCheckedIn {
		return Schedule{}
	}
	interval := f.CheckinInterval
	if interval <= 0 {
		interval = state.CheckinInterval
	}
	due := f.LastCheckinReceived.Add(interval)
	deadline := state.CheckinDeadline(f)

	s := Schedule{Known: true, DueAt: due, UnreachableAt: deadline, Overdue: now.After(due)}
	switch {
	case now.After(deadline):
		s.CountdownText = fmt.Sprintf("已經超過判定門檻 %s", state.HumanDur(now.Sub(deadline)))
	default:
		// ⚠ 「%s沒消息」不是「%s 沒消息」。HumanDur 回傳的是「3 分鐘」，
		// 後面再補一個空格就變成「再 3 分鐘 沒消息」—— 兩個中文字中間夾一個
		// 半形空格。這個 bug 在 state.go 修過一次，換一層又重演了一次，
		// 所以下面那支 TestNoStraySpaceBetweenChineseChars 掃的是整頁輸出，
		// 不是逐條字串比對。
		s.CountdownText = fmt.Sprintf("再 %s沒消息就判失聯", state.HumanDur(deadline.Sub(now)))
	}
	return s
}

func scheduleOfMachineMonitor(m operator.MachineMonitor, now time.Time) Schedule {
	facts := state.Facts{
		EverCheckedIn:   m.EverCheckedIn,
		CheckinInterval: time.Duration(m.CheckinIntervalSeconds) * time.Second,
	}
	if m.LastCheckinReceivedAt != nil {
		facts.LastCheckinReceived = *m.LastCheckinReceivedAt
	}
	return scheduleOf(facts, now)
}

// ---------------------------------------------------------------- dashboard

// retentionNote 是清舊資料這件事在畫面上的樣子。
//
// ⚠ 它的三種狀態要講三句不一樣的話，而且**沒有一句是空白**：
//
//	沒清過        → 「這裡看到的就是全部」（一句好消息，但要講出來才算）
//	清過          → 「更早的已經清掉了」（那個最舊的日期不是出生日）
//	讀不到紀錄    → 「不知道清過沒有」（不准當成沒清過）
//
// 第三種是最容易寫錯的：把「讀不到」跟「沒清過」合成同一個 zero value，
// 畫面就會說「這裡看到的就是全部」—— 一句技術上沒根據、而且會誤導人的話。
type retentionNote struct {
	// Known=false 代表讀不到清理紀錄，不是「沒清過」。
	Known bool
	// EverRun 只在 Known 時有意義。
	EverRun   bool
	LastPrune time.Time
	Rows      int64

	// Oldest 是這台機器身上最舊的一筆觀測（只有單機頁會填）。
	Oldest    time.Time
	HasOldest bool
}

// restoreDrillNote 是總覽最下面那一句「上一次還原演練」。
type restoreDrillNote struct {
	Known   bool // Hub 有在看章
	Done    bool // 章存在
	At      time.Time
	Overdue bool // 超過 90 天
	Days    int
}

// DrillStampReader 讓 main 把「讀章」塞進來；web 刻意不知道章長什麼樣、放在哪。
// nil = 不看（測試、或沒設定），畫面上會說「不知道」而不是「沒做過」。
type DrillStampReader func() (time.Time, bool)

// SetDrillStampReader 由 main 在起 Hub 的時候呼叫。
func (s *Server) SetDrillStampReader(f DrillStampReader) { s.drill = f }

func (s *Server) restoreDrill(now time.Time) restoreDrillNote {
	n := restoreDrillNote{}
	if s.drill == nil {
		return n
	}
	n.Known = true
	at, ok := s.drill()
	if !ok {
		return n
	}
	n.Done, n.At = true, at
	n.Days = int(now.Sub(at).Hours() / 24)
	n.Overdue = now.Sub(at) > 90*24*time.Hour
	return n
}

func (s *Server) retentionNote(machineID string) retentionNote {
	n := retentionNote{}
	at, rows, ok, err := s.store.LastPrune()
	if err != nil {
		// ⚠ 讀不到就讓它保持 Known=false。這裡**不可以**回一個
		// 「沒清過」的樂觀預設值 —— 那會讓畫面替我們保證一件沒問過的事。
		log.Printf("retention status unavailable：%v", err)
		return n
	}
	n.Known, n.EverRun, n.LastPrune, n.Rows = true, ok, at, rows
	if machineID != "" {
		if oldest, has, err := s.store.OldestObservation(machineID); err == nil {
			n.Oldest, n.HasOldest = oldest, has
		}
	}
	return n
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	machineRequest := operator.MachineListRequest{}
	var err error
	if r.URL.Path == "/machines" {
		machineRequest, err = parseMachineIndexRequest(r)
		if err != nil {
			http.Error(w, "machine filter 或 cursor 不合法", http.StatusBadRequest)
			return
		}
	}
	var machines operator.MachineListResult
	if r.URL.Path == "/machines" {
		machines, err = s.operator.ListMachinesPage(machineRequest, now)
	} else {
		machines, err = s.operator.ListMachines(now)
	}
	if err != nil {
		s.fail(w, "讀取機隊狀態失敗", err)
		return
	}
	ov, ok := machines.StoreOverview()
	if !ok {
		s.fail(w, "讀取機隊狀態失敗", errors.New("operator machine overview bridge unavailable"))
		return
	}
	displayNames := machineDisplayNameProjection(ov)
	// ⚠ 證據體檢壞掉不該讓整個總覽打不開。看不到機隊比看不到體檢嚴重。
	dead, err := s.store.DeadSignals(now)
	if err != nil {
		log.Printf("證據體檢失敗（總覽照常顯示）：%v", err)
		dead = nil
	} else {
		dead = projectDeadSignalDisplayNames(dead, displayNames)
	}
	// ⚠ 同理：重複 agent 的偵測壞掉不該讓總覽打不開。
	dup, err := s.store.DoubleAgents(now)
	if err != nil {
		log.Printf("重複 agent 偵測失敗（總覽照常顯示）：%v", err)
		dup = nil
	} else {
		dup = projectDoubleAgentDisplayNames(dup, displayNames)
	}

	// ⚠ 讀不到 Hub 的日誌不該讓總覽打不開。
	hubEvents, err := s.store.HubEventsBetween(now.Add(-24*time.Hour), now)
	if err != nil {
		log.Printf("讀 Hub 自己的日誌失敗（總覽照常顯示）：%v", err)
		hubEvents = nil
	} else {
		hubEvents = projectHubEventPresentation(hubEvents)
	}
	deploymentViews, err := s.store.ListDeployments(now)
	var deploymentLines []deploymentHeadlineRow
	deploymentsKnown := err == nil
	if err != nil {
		log.Printf("讀 deployment 帳本失敗（總覽照常顯示）：%v", err)
		deploymentLines = []deploymentHeadlineRow{{Text: "讀不到部署帳本，不能判斷有沒有部署卡住。", Unknown: true}}
	} else {
		deploymentViews = projectDeploymentDisplayNames(deploymentViews, displayNames)
		deploymentLines = deploymentHeadlineRows(deploymentViews)
	}

	nav := "dashboard"
	title := "總覽"
	machineIndex := false
	if r.URL.Path == "/machines" {
		nav = "machines"
		title = "裝置"
		machineIndex = true
	}
	s.render(w, r, "dashboard.html", page{
		Title:               title,
		Nav:                 nav,
		Now:                 now.Local().Format("2006-01-02 15:04"),
		Overview:            ov,
		Dashboard:           buildDashboardStats(ov, deploymentViews, deploymentsKnown),
		MachineTable:        buildDashboardMachineTable(ov, r.URL.Query().Get("machines")),
		MachineIndex:        machineIndex,
		MachineList:         buildMachineIndexPage(machineRequest, machines),
		Headline:            headline(ov),
		HubInFleet:          hubInFleet(ov, s.hubHost),
		HubEvents:           hubEvents,
		RestoreDrill:        s.restoreDrill(now),
		DeadSignals:         dead,
		DoubleAgents:        dup,
		DeploymentHeadlines: deploymentLines,
		Tailnet:             s.tailnetResult(r.Context(), ov),
		Retention:           s.retentionNote(""),
		Tools:               s.softwareMatrix(now),
	})
}

// tailnetResult 拿 tailnet 當名冊的第二個來源。
//
// ⚠ 這裡任何一步失敗，都必須讓結果變成「不知道」而不是「沒事」。
// 一個空的、乾淨的、沒有壞消息的答案，第一個要懷疑的是有沒有問對地方（§5.9）。
func (s *Server) tailnetResult(ctx context.Context, ov store.Overview) tailnet.Result {
	roster, err := s.store.RosterForTailnet()
	if err != nil {
		return tailnet.Result{Unavailable: "讀不到名冊：" + err.Error()}
	}
	ignored, err := s.store.IgnoredPeers()
	if err != nil {
		return tailnet.Result{Unavailable: "讀不到忽略清單：" + err.Error()}
	}
	// silent：Hub 這邊收不到心跳的機器。tailnet 說它 online 的話，
	// 「機器死了」就要改口成「機器活著，是 agent 沒在報」——
	// 這兩件事下一步要做的完全不同。
	silent := map[string]bool{}
	for _, m := range ov.Machines {
		if m.State == state.Unreachable || m.State == state.NeverReported {
			silent[m.MachineID] = true
		}
	}
	result := tailnet.Reconcile(s.tailnet.Get(ctx), roster, ignored, silent)
	return projectTailnetDisplayNames(result, machineDisplayNameProjection(ov))
}

// headline 產生第一屏那幾句話。
//
// ⚠ 它刻意回傳「句子」而不是一包數字。第一屏不是一排圓點，
// 是幾句你不必翻譯就懂的話。
func headline(ov store.Overview) []string {
	var out []string
	counts := ov.DenominatorCounts()

	// 第一句永遠是分母。名冊有 5 台、只有 4 台報到，那 1 台要出現在這句話裡。
	//
	// ⚠ 新鮮的身分衝突心跳算在「正在回報」裡；歷史衝突本身不能替過期
	// 心跳續命。identity hint 是 durable evidence，reporting 是 freshness fact。
	//
	// 這是實機演練改的。原本它跟 Unreachable / NeverReported 歸在一起，於是畫面寫
	// 「名冊上 3 台，0 台正在回報」，而同一頁的表格裡那台衝突的機器寫著
	// 「36 秒前」。它明明在講話 —— 衝突的意思不是「它沒回報」，而是
	// 「它有回報，但你不知道那些回報來自哪一台實體機器」。
	//
	// 第一屏跟它下面的表格自相矛盾，人就會停止相信第一屏。那是 §5.2 修過的
	// 同一個 bug，只是換了一個狀態再犯一次。
	conflicts := counts[state.IdentityConflict]
	reported := ov.ReportingCount()
	first := fmt.Sprintf("名冊上 %d 台，%d 台正在回報。", ov.Expected, reported)
	var missing []string
	for _, st := range []state.State{state.Unreachable, state.NeverReported} {
		if n := counts[st]; n > 0 {
			missing = append(missing, fmt.Sprintf("%d 台%s", n, label(st)))
		}
	}
	if len(missing) > 0 {
		first += strings.Join(missing, "、") + "。"
	}
	out = append(out, first)

	// 身分衝突自己一句。它是最嚴重的狀態，而且它的意思不是一般人猜得到的：
	// 「有回報」跟「知道是誰在回報」是兩件事，而一個只會數台數的第一屏
	// 沒辦法表達這個差別。
	if conflicts > 0 {
		const why = "同一個 machine_id 被一台以上的機器使用，所以你不知道那些回報來自哪一台。"
		// ⚠ 兩個分支各自寫完整的句子，不要用字串拼一半。
		// 拼出來的第一版是「其中 %s」+「的身分對不上」，機器名是拉丁字母，
		// 結果變成「其中 drilltarget3的身分對不上」—— 拉丁字後面漏了空格。
		// 中文跟拉丁字之間的空格規則沒辦法靠拼接維持，只能靠寫完整句子。
		var s string
		if conflicts == 1 {
			// 只有一台就點名。「其中 1 台」逼人自己去表格裡找是哪一台，
			// 而第一屏的工作正是讓人不必找。
			name := ""
			for _, m := range ov.Machines {
				if m.InDenominator() && m.State == state.IdentityConflict {
					name = m.DisplayName
					break
				}
			}
			s = fmt.Sprintf("其中 %s 的身分對不上 —— %s", name, why)
		} else {
			s = fmt.Sprintf("其中 %d 台的身分對不上 —— %s", conflicts, why)
		}
		out = append(out, s)
	}

	// 第二句：最該看的那台是誰、為什麼。Machines 已經照嚴重度排好。
	//
	// ⚠ 只有在「最嚴重的那個狀態就它一台」時才點名。四台都是「從未報到」的時候
	// 寫「最該看的是 sampleagent1」是誤導 —— 那個名字是照字母排序挑的，不是因為它
	// 比較嚴重。而且第一句已經說了「4 台從未報到」，再點一個名字只是把讀者的
	// 注意力從四台縮到一台。
	//
	// ⚠ 接的字用「：」不用「 —— 」，因為 Reason 自己就常常含有破折號，
	// 兩個疊在一起會變成「最該看的是 sampleagent1 —— 從未報到 —— 它在名冊上…」。
	//
	// ⚠ 身分衝突跳過 —— 上面已經有一句專門講它了，再點一次名只是把同一件事
	// 說兩遍，而第一屏的行數是這個產品最貴的資源。
	var worst store.MachineRow
	hasWorst := false
	for _, machine := range ov.Machines {
		if machine.InDenominator() {
			worst = machine
			hasWorst = true
			break
		}
	}
	if hasWorst && worst.State != state.Online && worst.State != state.IdentityConflict {
		if counts[worst.State] == 1 {
			out = append(out, fmt.Sprintf("最該看的是 %s：%s", worst.DisplayName, worst.Reason))
		} else if worst.State == state.Degraded {
			// 降級沒有出現在第一句的清單裡，所以這裡要講出來。
			out = append(out, fmt.Sprintf("%d 台降級，先看 %s：%s",
				counts[worst.State], worst.DisplayName, worst.Reason))
		}
	}

	// 第三句：憑證。有明確截止時間的東西值得單獨講一句。
	var expired []string
	for _, f := range ov.Findings {
		if f.Kind == "credential" && f.Severity >= 3 {
			expired = append(expired, f.DisplayName)
		}
	}
	if len(expired) > 0 {
		out = append(out, fmt.Sprintf("%s 的登入需要重新處理。", strings.Join(dedupe(expired), "、")))
	}

	if len(out) == 1 {
		switch {
		case ov.Expected == 0:
			out = append(out, "名冊是空的 —— 還沒有任何機器被納管。")

		case reported == ov.Expected && len(ov.Findings) > 0:
			// ⚠ 有 advisory 的時候不准說「沒事」。
			//
			// 這一條是看著真機的畫面改的：第一屏寫「沒有需要處理的事」，
			// 同一頁下面卻列著兩條發現 —— 那就是在同一個畫面上自打嘴巴，
			// 而人會相信上面那句，因為它比較大。advisory 的意思是
			// 「不影響狀態」，不是「不存在」。把 unknown 洗成綠燈是這個
			// 專案最想避免的事，第一屏尤其不能帶頭洗。
			out = append(out, fmt.Sprintf(
				"所有機器都有在回報，另外有 %d 件它們自己講不清楚的事列在下面。", len(ov.Findings)))

		case reported == ov.Expected:
			// ⚠ 措辭：「都有在回報」，不是「一切正常」。
			// 我們知道它們在講話，不知道它們講的內容對不對。
			out = append(out, "所有機器都有在回報，也沒有任何一台講出不對勁的地方。")
		}
	}
	return out
}

// hubInFleet 回答「Hub 是不是裝在它自己管的機器上」。
// 是的話 Dashboard 要常駐警告：那台死掉時你會同時失去服務與知情能力。
func hubInFleet(ov store.Overview, hubHost string) string {
	if hubHost == "" {
		return ""
	}
	for _, m := range ov.Machines {
		if m.InDenominator() && (m.Hostname == hubHost || m.DisplayName == hubHost) {
			return m.DisplayName
		}
	}
	return ""
}

// ---------------------------------------------------------------- 單機頁

func (s *Server) machine(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	machine, err := s.operator.MachineDetail(r.PathValue("id"), now)
	if err != nil {
		http.Error(w, "名冊上沒有這台機器。", http.StatusNotFound)
		return
	}
	connect, err := s.operator.MachineConnect(machine)
	if err != nil {
		s.fail(w, "讀取 BAT 連線座標失敗", err)
		return
	}
	lifecycle, err := s.operator.MachineLifecycle(machine.Item.MachineID)
	if err != nil {
		s.fail(w, "讀取機器 lifecycle 失敗", err)
		return
	}
	machineView, err := machinePageMachineFrom(machine, lifecycle)
	if err != nil {
		s.fail(w, "讀取機器名冊欄位失敗", err)
		return
	}
	// Detail and evidence share one evaluation cutoff, but this composition is
	// not one cross-query atomic snapshot. Each typed result keeps its own
	// disclosure boundary.
	evidence, err := s.operator.MachineEvidence(operator.MachineEvidenceRequest{
		MachineID: machine.Item.MachineID,
	}, now)
	if err != nil {
		s.fail(w, "讀取機器文字證據失敗", err)
		return
	}
	var pendingEnrollment *operator.EnrollTokenStatusResult
	pending, err := s.operator.PendingEnrollToken(machine.Item.MachineID)
	switch {
	case err == nil:
		pendingEnrollment = &pending
	case errors.Is(err, store.ErrEnrollTokenNotPending):
		// No unused ticket is the normal state after enrollment or revocation.
	default:
		s.fail(w, "讀取 pending enrollment ticket 失敗", err)
		return
	}
	journals := make(map[string]operator.MachineJournal, len(evidence.Journals.Items))
	for _, journal := range evidence.Journals.Items {
		journals[journal.Unit.Text] = journal
	}
	jobList, err := s.operator.ListJobs(operator.JobListRequest{
		MachineID: machine.Item.MachineID,
		Limit:     10,
	}, now)
	if err != nil {
		s.fail(w, "讀取機器工作單失敗", err)
		return
	}
	access := accessFromRequest(r)
	catalogue, err := s.operator.MachineActions(operator.MachineActionsRequest{
		Detail: machine, Connect: connect, Lifecycle: lifecycle,
		Granted: operator.MachineActionGrant{Operate: access.CanOperate, Admin: access.CanAdmin},
	})
	if err != nil {
		s.fail(w, "讀取裝置動作目錄失敗", err)
		return
	}
	channelKey, err := operator.NewIdempotencyKey("web-machine-channel")
	if err != nil {
		s.fail(w, "產生 channel 表單 request key 失敗", err)
		return
	}
	monitorInterval := time.Duration(machine.Monitor.CheckinIntervalSeconds) * time.Second
	var monitorClockSkew *time.Duration
	if machine.Monitor.ClockSkewSeconds != nil {
		value := time.Duration(*machine.Monitor.ClockSkewSeconds) * time.Second
		monitorClockSkew = &value
	}
	var latestMachineCheckin *operator.MachineCheckin
	if count := len(machine.Checkins.Items); count > 0 {
		value := machine.Checkins.Items[count-1]
		latestMachineCheckin = &value
	}
	s.render(w, r, "machine.html", page{
		Title:                   machine.Item.DisplayName,
		Nav:                     "machines-detail",
		Now:                     now.Local().Format("2006-01-02 15:04"),
		MachineDetail:           &machine,
		Machine:                 machineView,
		MachineConnect:          &connect,
		MachineActions:          machineActionsViewFrom(catalogue),
		PendingEnrollment:       pendingEnrollment,
		MachineEvidence:         &evidence,
		MachineJournals:         journals,
		Strip:                   machineDetailStrip(machine.Checkins.Items, monitorInterval),
		StripTiers:              stripTiers(),
		CheckinWindowText:       disclosureWindowText(time.Duration(machine.Disclosure.CheckinWindowSeconds) * time.Second),
		Schedule:                scheduleOfMachineMonitor(machine.Monitor, now),
		MachineMonitorInterval:  monitorInterval,
		MachineMonitorClockSkew: monitorClockSkew,
		LatestMachineCheckin:    latestMachineCheckin,
		Jobs:                    jobList.Items,
		ChannelIdempotencyKey:   channelKey,
		// L2NotWired：成果判定有沒有接通。
		// ⚠ 目前永遠是 true —— 上游的 terminal_outcome 全機隊都是 NULL，
		// 而 status='ok' 只代表回合正常結束。這一行必須常駐在單機頁上，
		// 直到真的有結構化的成果訊號為止。哪天它變成 false，
		// 條件應該是「這台的 TerminalOutcomePopulated > 0」而不是拿掉這個欄位。
		L2NotWired: true,
		Retention:  s.retentionNote(machine.Item.MachineID),
	})
}

type jobPage struct {
	Summary  operator.JobSummary
	Evidence operator.JobEvidenceResult
	Rollback *operator.JobVerificationEvidence
	// NoEvidence／StageOnly 只給 manual_intervention 的句子用：
	// ⚠ 「機器停在中間」是對「切換過」的單講的。2026-09-06 D5（job f159f75f）在 stage 就被逾時砍掉，
	// 機器一個檔案都沒動，頁面卻寫「停在中間」—— 多講了。停在哪一步要看證據，不看狀態名。
	NoEvidence bool
	StageOnly  bool
	// EligibleVerifiers is filtered by the same rule the Hub enforces on the
	// write: a verifier whose failure domain is this job's machine is not
	// independent of it, so it is never offered here.
	EligibleVerifiers []operator.VerifierItem
}

// job 是人看的工作單與證據頁。safe metadata 與 bounded evidence 都由
// operator typed disclosure boundary 提供；這不是 agent 能列舉別人
// job_id 的協定端點。
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	result, err := s.operator.JobDetail(r.PathValue("id"), now)
	if errors.Is(err, store.ErrJobNotFound) {
		http.Error(w, "找不到這張工作單。", http.StatusNotFound)
		return
	}
	if errors.Is(err, operator.ErrInvalidJobRead) {
		http.Error(w, "工作單 ID 不合法。", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, "讀取工作單失敗", err)
		return
	}
	evidence, err := s.operator.JobEvidence(operator.JobEvidenceRequest{JobID: r.PathValue("id")}, now)
	if errors.Is(err, store.ErrJobNotFound) {
		http.Error(w, "找不到這張工作單。", http.StatusNotFound)
		return
	}
	if errors.Is(err, operator.ErrInvalidJobRead) {
		http.Error(w, "工作單 ID 不合法。", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, "讀取工作單證據失敗", err)
		return
	}
	view := &jobPage{
		Summary: result.Item, Evidence: evidence,
	}
	view.NoEvidence = evidence.Verifications.Total == 0
	// A truncated window cannot prove that every saved result was only a stage
	// result, even when every currently rendered row is one.
	view.StageOnly = evidence.Verifications.Total > 0 && !evidence.Verifications.Truncated
	for i := range evidence.Verifications.Items {
		if evidence.Verifications.Items[i].RuleID.Text == "rollback" {
			view.Rollback = &evidence.Verifications.Items[i]
		}
		if evidence.Verifications.Items[i].RuleID.Text != "stage" {
			view.StageOnly = false
		}
	}
	if accessFromRequest(r).CanOperate {
		verifiers, err := s.operator.Verifiers()
		if err != nil {
			s.fail(w, "讀取 verifier 名冊失敗", err)
			return
		}
		for _, verifier := range verifiers.Verifiers {
			if verifier.State == store.VerifierStateActive &&
				verifier.FailureDomain != result.Item.MachineID {
				view.EligibleVerifiers = append(view.EligibleVerifiers, verifier)
			}
		}
	}
	s.render(w, r, "job.html", page{
		Title: "代理程式活動 " + short(result.Item.JobID, 8), Nav: "jobs-detail",
		Now: now.Local().Format("2006-01-02 15:04"), Job: view,
	})
}

// Block 是心跳長條圖的一格。
type Block struct {
	Class  string
	Height int
	Title  string
	Status string
}

// StripTier 是心跳狀態條的封閉詞彙：一個顏色配一句意義。
// ⚠ 圖例是這張表的渲染，不是第二份手寫文案。
// stripReceivedAt 仍然自己寫它的字面值，兩邊由
// TestTheHeartbeatStripLegendCoversEveryTierItCanRender 綁住——
// 漂掉要紅，而不是靠共用常數讓它不可能漂。
type StripTier struct {
	Class   string
	Meaning string
}

func stripTiers() []StripTier {
	return []StripTier{
		{Class: "green", Meaning: "準時"},
		{Class: "amber", Meaning: "稍晚"},
		{Class: "red", Meaning: "嚴重遲到"},
		{Class: "grey", Meaning: "顯示範圍起點，前一筆未知"},
	}
}

// disclosureWindowText 把讀取窗口講成人話。
// ⚠ 整數小時要講「24 小時」，不是 state.HumanDur 的「24.0 小時」。
func disclosureWindowText(d time.Duration) string {
	if d > 0 && d < 48*time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d 小時", int(d/time.Hour))
	}
	return state.HumanDur(d)
}

func machineDetailStrip(pts []operator.MachineCheckin, interval time.Duration) []Block {
	received := make([]time.Time, 0, len(pts))
	for _, point := range pts {
		received = append(received, point.ReceivedAt)
	}
	return stripReceivedAt(received, interval)
}

// stripReceivedAt 把心跳接收時間轉成供狀態條繪製的區塊。
// ⚠ 高度表示該次心跳距離上一次有多久，不是任何品質指標；一格高只代表那次之前安靜了很久。
// 顏色只表示是否遲到，因為心跳能回答的問題就只有這一個。
func stripReceivedAt(received []time.Time, interval time.Duration) []Block {
	if interval <= 0 {
		interval = state.CheckinInterval
	}
	const maxBlocks = 90
	var predecessor *time.Time
	if len(received) > maxBlocks {
		p := received[len(received)-maxBlocks-1]
		predecessor = &p
		received = received[len(received)-maxBlocks:]
	}
	out := make([]Block, 0, len(received))
	for i, receivedAt := range received {
		if i == 0 && predecessor == nil {
			out = append(out, Block{
				Class: "grey", Height: 6,
				Title:  receivedAt.Local().Format("01-02 15:04:05"),
				Status: "顯示範圍起點，前一筆未知",
			})
			continue
		}
		var previous time.Time
		if i == 0 {
			previous = *predecessor
		} else {
			previous = received[i-1]
		}
		gap := receivedAt.Sub(previous)
		class, status := "green", "準時"
		switch {
		case gap > 3*interval:
			class, status = "red", "嚴重遲到"
		case gap > interval*3/2:
			class, status = "amber", "稍晚"
		}
		// 6..26px，以 interval 的 4 倍為滿格。
		h := 6 + int(float64(gap)/float64(4*interval)*20)
		if h > 26 {
			h = 26
		}
		if h < 6 {
			h = 6
		}
		out = append(out, Block{
			Class:  class,
			Height: h,
			Title: fmt.Sprintf("%s　距上一次 %s",
				receivedAt.Local().Format("01-02 15:04:05"), state.HumanDur(gap)),
			Status: status,
		})
	}
	return out
}

// ---------------------------------------------------------------- helpers

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data page) {
	locale := navigationLocaleFromRequest(r)
	data.Locale = navigationLocaleFor(r, data.Title)
	data.Access = accessFromRequest(r)
	localizeAccessView(&data.Access, locale)
	if s.agentBundles != nil {
		data.AgentBundles = append([]agentBundleView(nil), s.agentBundles.views...)
	}
	if len(data.SubNav.Items) == 0 {
		data.SubNav = subNavigationFor(data)
	}
	selectSubNavigationLocation(&data.SubNav, r.URL.Query().Get("section"))
	localizeSubNavigation(&data.SubNav, locale)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", data.Locale.Tag)
	w.Header().Add("Vary", "Cookie")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// ⚠ 這裡不能靜悄悄。樣板錯了畫面會少一整塊，而少一塊的畫面
		// 看起來就像「那台沒事」—— 跟這個產品要避免的失效模式一模一樣。
		log.Printf("樣板 %s 渲染失敗：%v", name, err)
	}
}

// subNavigationFor keeps the IA in one typed place instead of duplicating a
// slightly different set of links in every template.  Fragment destinations
// intentionally point at evidence that is already on the rendered page; later
// slices can turn them into separate routes without changing their labels.
func subNavigationFor(data page) subNavigation {
	switch data.Nav {
	case "machines":
		current := "概觀"
		if data.MachineList != nil && data.MachineList.CurrentLabel != "" && data.MachineList.HasFilters {
			current = data.MachineList.CurrentLabel
			if current == "所有機器" {
				current = "所有裝置"
			}
		}
		return makeSubNavigation("裝置子選單", current, []subNavigationItem{
			{Label: "概觀", Href: "/machines#fleet-kpis"},
			{Label: "所有裝置", Href: "/machines#machines"},
			{Label: "監視", Href: "/machines#findings"},
			{Label: "註冊裝置", Href: "/machines/enrollment"},
			{Label: "組態", Href: "/machines/configuration"},
			{Label: "合規性", Href: "/machines/compliance"},
			{Label: "生命週期", Href: "/machines/lifecycle"},
			{Label: "更新", Href: "/updates"},
			{Label: "代理程式活動", Href: "/jobs"},
			{Label: "已退役", Href: "/machines?lifecycle=retired#machines"},
		}...)
	case "machines-enrollment":
		return makeSubNavigation("裝置子選單", "註冊裝置", []subNavigationItem{
			{Label: "概觀", Href: "/machines#fleet-kpis"},
			{Label: "所有裝置", Href: "/machines#machines"},
			{Label: "監視", Href: "/machines#findings"},
			{Label: "註冊裝置", Href: "/machines/enrollment"},
			{Label: "組態", Href: "/machines/configuration"},
			{Label: "合規性", Href: "/machines/compliance"},
			{Label: "生命週期", Href: "/machines/lifecycle"},
			{Label: "更新", Href: "/updates"},
			{Label: "代理程式活動", Href: "/jobs"},
			{Label: "已退役", Href: "/machines?lifecycle=retired#machines"},
		}...)
	case "machines-lifecycle":
		return makeSubNavigation("裝置子選單", "生命週期", []subNavigationItem{
			{Label: "概觀", Href: "/machines#fleet-kpis"},
			{Label: "所有裝置", Href: "/machines#machines"},
			{Label: "監視", Href: "/machines#findings"},
			{Label: "註冊裝置", Href: "/machines/enrollment"},
			{Label: "組態", Href: "/machines/configuration"},
			{Label: "合規性", Href: "/machines/compliance"},
			{Label: "生命週期", Href: "/machines/lifecycle"},
			{Label: "更新", Href: "/updates"},
			{Label: "代理程式活動", Href: "/jobs"},
			{Label: "已退役", Href: "/machines?lifecycle=retired#machines"},
		}...)
	case "machines-configuration":
		return makeSubNavigation("裝置子選單", "組態", []subNavigationItem{
			{Label: "概觀", Href: "/machines#fleet-kpis"},
			{Label: "所有裝置", Href: "/machines#machines"},
			{Label: "監視", Href: "/machines#findings"},
			{Label: "註冊裝置", Href: "/machines/enrollment"},
			{Label: "組態", Href: "/machines/configuration"},
			{Label: "合規性", Href: "/machines/compliance"},
			{Label: "生命週期", Href: "/machines/lifecycle"},
			{Label: "更新", Href: "/updates"},
			{Label: "代理程式活動", Href: "/jobs"},
			{Label: "已退役", Href: "/machines?lifecycle=retired#machines"},
		}...)
	case "machines-compliance":
		return makeSubNavigation("裝置子選單", "合規性", []subNavigationItem{
			{Label: "概觀", Href: "/machines#fleet-kpis"},
			{Label: "所有裝置", Href: "/machines#machines"},
			{Label: "監視", Href: "/machines#findings"},
			{Label: "註冊裝置", Href: "/machines/enrollment"},
			{Label: "組態", Href: "/machines/configuration"},
			{Label: "合規性", Href: "/machines/compliance"},
			{Label: "生命週期", Href: "/machines/lifecycle"},
			{Label: "更新", Href: "/updates"},
			{Label: "代理程式活動", Href: "/jobs"},
			{Label: "已退役", Href: "/machines?lifecycle=retired#machines"},
		}...)
	case "machines-diagnostics":
		return makeSubNavigation("疑難排解子選單", "裝置診斷",
			subNavigationItem{Label: "裝置診斷", Href: "/machines/diagnostics"},
		)
	case "jobs":
		return makeSubNavigation("代理程式子選單", "概觀",
			subNavigationItem{Label: "概觀", Href: "/jobs#agent-overview"},
			subNavigationItem{Label: "活動", Href: "/jobs#job-ledger"},
			subNavigationItem{Label: "裝置註冊", Href: "/machines/enrollment"},
			subNavigationItem{Label: "設定檔", Href: "/apps?view=profiles"},
			subNavigationItem{Label: "部署", Href: "/deployments"},
			subNavigationItem{Label: "稽核記錄", Href: "/audit"},
		)
	case "jobs-detail":
		if data.Job == nil {
			return subNavigation{}
		}
		base := "/jobs/" + data.Job.Summary.JobID
		return makeSubNavigation("代理程式活動詳細資料子選單", "概觀",
			subNavigationItem{Label: "所有活動", Href: "/jobs"},
			subNavigationItem{Label: "概觀", Href: base + "#job-overview"},
			subNavigationItem{Label: "期望狀態", Href: base + "#desired-state"},
			subNavigationItem{Label: "事件", Href: base + "#job-events"},
			subNavigationItem{Label: "驗證證據", Href: base + "#job-verifications"},
		)
	case "machines-detail":
		id := data.Machine.MachineID
		current := "概觀"
		if data.MachineTimeline != nil {
			id = data.MachineTimeline.Result.MachineID
			current = "事件時間軸"
		}
		if data.MachineData != nil {
			id = data.MachineData.Result.MachineID
			current = "資料"
		}
		if data.EnrollTokenRevocationPreview != nil {
			id = data.EnrollTokenRevocationPreview.MachineID
			current = "動作"
		}
		if data.LifecyclePreview != nil {
			id = data.LifecyclePreview.MachineID
			current = "動作"
		}
		if id == "" && data.Action != nil {
			if candidate := detailIDFromBack(data.Action.Back, "/machines/"); candidate != "" {
				id = candidate
				current = "動作"
			}
		}
		if id == "" {
			return subNavigation{}
		}
		base := "/machines/" + id
		items := []subNavigationItem{
			{Label: "概觀", Href: base + "#machine-overview"},
			{Label: "監視", Href: base + "#monitor"},
			{Label: "屬性", Href: base + "#properties"},
			{Label: "應用與憑證", Href: base + "#apps-and-credentials"},
			{Label: "工作單", Href: base + "#jobs"},
		}
		// 動作那一節只有在這個操作員真的有動作可做時才存在，所以選單也只在那時
		// 才指得過去。這個問題由 operator 回答，單機的每一頁才會給同一份選單。
		if operator.MachineActionsReachable(operator.MachineActionGrant{
			Operate: data.Access.CanOperate, Admin: data.Access.CanAdmin,
		}) || current == "動作" {
			items = append(items, subNavigationItem{Label: "動作", Href: base + "#actions"})
		}
		items = append(items, subNavigationItem{Label: "事件時間軸", Href: base + "/timeline"})
		items = append(items, subNavigationItem{Label: "資料", Href: base + "/data"})
		return makeSubNavigation("裝置詳細資料子選單", current, items...)
	case "apps":
		current := "概觀"
		if data.Apps != nil && data.Apps.CurrentLabel != "" {
			current = data.Apps.CurrentLabel
		} else if data.ArtifactDetail != nil {
			current = "Artifacts"
		} else if data.ArtifactFetchReview != nil {
			current = "Fetch"
		} else if data.ArtifactFetchOperation != nil {
			current = "Operations"
		}
		current = localizedAppsLabel(current)
		return makeSubNavigation("應用子選單", current,
			subNavigationItem{Label: "概觀", Href: "/apps?view=overview"},
			subNavigationItem{Label: "OpenClaw", Href: "/apps?view=openclaw"},
			subNavigationItem{Label: "應用程式目錄", Href: "/apps?view=store"},
			subNavigationItem{Label: "設定檔", Href: "/apps?view=profiles"},
			subNavigationItem{Label: "指派", Href: "/apps?view=assignments"},
			subNavigationItem{Label: "安裝套件", Href: "/apps?view=artifacts"},
			subNavigationItem{Label: "套件擷取", Href: "/apps?view=fetch"},
			subNavigationItem{Label: "作業", Href: "/apps?view=operations"},
			subNavigationItem{Label: "部署", Href: "/deployments"},
		)
	case "deployments":
		current := "全部"
		if data.DeploymentList != nil && data.DeploymentList.CurrentLabel != "" {
			current = localizedDeploymentLabel(data.DeploymentList.CurrentLabel)
		}
		return makeSubNavigation("部署子選單", current,
			subNavigationItem{Label: "全部", Href: "/deployments"},
			subNavigationItem{Label: "進行中", Href: "/deployments?view=active"},
			subNavigationItem{Label: "已暫停 / 卡住", Href: "/deployments?view=stuck"},
			subNavigationItem{Label: "已完成", Href: "/deployments?view=finished"},
			subNavigationItem{Label: "新增部署", Href: "/deployments?view=new"},
		)
	case "deployments-detail":
		id := ""
		current := "概觀"
		if data.Deployment != nil {
			id = data.Deployment.Summary.DeploymentID
		} else if data.DeploymentActionReview != nil {
			id = data.DeploymentActionReview.Preview.Deployment.DeploymentID
			current = "動作"
		} else if data.Action != nil {
			id = detailIDFromBack(data.Action.Back, "/deployments/")
			if id != "" {
				current = "動作"
			}
		}
		if id == "" {
			return subNavigation{}
		}
		base := "/deployments/" + id
		return makeSubNavigation("部署詳細資料子選單", current,
			subNavigationItem{Label: "概觀", Href: base + "#deployment-overview"},
			subNavigationItem{Label: "監視與卡住", Href: base + "#stuck"},
			subNavigationItem{Label: "目標與工作單", Href: base + "#deployment-targets"},
			subNavigationItem{Label: "動作", Href: base + "#deployment-actions"},
			subNavigationItem{Label: "設定", Href: base + "#deployment-settings"},
		)
	case "updates":
		return makeSubNavigation("更新子選單", "更新概觀",
			subNavigationItem{Label: "更新概觀", Href: "/updates"},
			subNavigationItem{Label: "安裝套件", Href: "/updates#artifacts"},
			subNavigationItem{Label: "Canary", Href: "/updates#channel-canary"},
			subNavigationItem{Label: "Stable", Href: "/updates#channel-stable"},
		)
	case "reports":
		return reportsSubNavigation("報告總覽")
	case "changes":
		return reportsSubNavigation("變更")
	case "tickets":
		return reportsSubNavigation("票證使用量")
	case "enrollment-report":
		return reportsSubNavigation("註冊")
	case "software-report":
		return reportsSubNavigation("軟體清查")
	case "install-report":
		return reportsSubNavigation("安裝狀態")
	case "profile-report":
		return reportsSubNavigation("發佈與指派")
	case "audit":
		return reportsSubNavigation("稽核記錄")
	case "tenant-maintenance":
		return tenantSubNavigation("維護")
	case "tenant-data":
		return tenantSubNavigation("資料揭露")
	default:
		return subNavigation{}
	}
}

// reportsSubNavigation 是報告那一欄的選單。入口寫在一個地方，因為它們指的是
// 同一份清單：落地頁上列出來的報告，就是選單上的那幾個。
func reportsSubNavigation(current string) subNavigation {
	return makeSubNavigation("報告子選單", current,
		subNavigationItem{Label: "報告總覽", Href: "/reports"},
		subNavigationItem{Label: "變更", Href: "/reports/changes"},
		subNavigationItem{Label: "票證使用量", Href: "/reports/tickets"},
		subNavigationItem{Label: "註冊", Href: "/reports/enrollment"},
		subNavigationItem{Label: "軟體清查", Href: "/reports/software"},
		subNavigationItem{Label: "安裝狀態", Href: "/reports/install"},
		subNavigationItem{Label: "發佈與指派", Href: "/reports/profile"},
		subNavigationItem{Label: "稽核記錄", Href: "/audit"},
	)
}

// tenantSubNavigation 是租用戶那一欄的選單。保留期在維護那一頁設定，而它設定的
// 東西由揭露面講出來——兩頁講的是同一件事的兩半，所以指得到彼此。
func tenantSubNavigation(current string) subNavigation {
	return makeSubNavigation("租用戶子選單", current,
		subNavigationItem{Label: "維護", Href: "/tenant/maintenance"},
		subNavigationItem{Label: "資料揭露", Href: "/tenant/data"},
	)
}

func localizedDeploymentLabel(label string) string {
	switch label {
	case "Paused / Stuck":
		return "已暫停 / 卡住"
	default:
		return label
	}
}

func detailIDFromBack(back, prefix string) string {
	if !strings.HasPrefix(back, prefix) {
		return ""
	}
	id := strings.TrimPrefix(back, prefix)
	id, _, _ = strings.Cut(id, "?")
	id, _, _ = strings.Cut(id, "#")
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

func (s *Server) fail(w http.ResponseWriter, msg string, err error) {
	log.Printf("%s：%v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func short(s string, n int) string {
	if n < 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

func label(s state.State) string {
	switch s {
	case state.Online:
		return "在線"
	case state.Degraded:
		return "降級"
	case state.Unreachable:
		return "失聯"
	case state.NeverReported:
		return "從未報到"
	case state.IdentityConflict:
		return "身分衝突"
	}
	return string(s)
}

var funcs = template.FuncMap{
	"label":                  label,
	"deploymentBlockerLabel": operator.DeploymentBlockerLabel,
	"ruleLabel":              func(kind compliance.RuleKind) string { return compliance.RuleLabel(kind) },
	"ruleBound":              func(rule compliance.Rule) string { return compliance.RuleBound(rule) },
	"outcomeTone":            complianceOutcomeTone,
	"actionLabel":            func(kind compliance.ActionKind) string { return compliance.ActionLabel(kind) },
	"timelineSource": func(source operator.MachineTimelineSource) string {
		return operator.MachineTimelineSourceLabel(source)
	},
	"actionTone": complianceActionTone,
	"graceLabel": complianceGraceWords,
	"color":      func(s state.State) string { return s.Color() },
	"evidenceReplaced": func(issues []string) bool {
		for _, issue := range issues {
			if issue == "control_or_format_replaced" || issue == "invalid_utf8" {
				return true
			}
		}
		return false
	},
	"jobEvidenceLimitLabel": func(maxBytes int) string {
		if maxBytes >= 1024 && maxBytes%1024 == 0 {
			return fmt.Sprintf("%d KiB", maxBytes/1024)
		}
		return fmt.Sprintf("%d bytes", maxBytes)
	},
	"evidenceVerbatim": func(value operator.EvidenceText) bool {
		return !value.Truncated && len(value.Issues) == 0
	},
	// ⚠ 這個判斷只有一個地方寫：operator。樣板上自己列一次 "other_file"／"gone_file"
	// 的話，哪天多一種要人動手的狀態，畫面會少標一批格子而沒有人發現。
	"runtimeMisattributed": func(finding *operator.ToolRuntimeFinding) bool {
		return finding != nil && operator.ToolRuntimeMisattributed(finding.State)
	},
	"triBool": func(value *bool) string {
		if value == nil {
			return "Unknown"
		}
		if *value {
			return "是"
		}
		return "否"
	},
	"jobStateClass": func(s deploy.JobState) string {
		switch s {
		case deploy.Succeeded:
			return "st green"
		case deploy.Claimed, deploy.Running, deploy.Verifying:
			return "st blue"
		case deploy.Failed, deploy.LeaseExpired:
			return "st amber"
		case deploy.ManualIntervention:
			return "st red"
		case deploy.Rejected:
			return "st grey"
		default:
			return ""
		}
	},
	"terminalJobOutcome": func(s deploy.JobState) string {
		return operator.TerminalJobOutcome(s)
	},
	"jobLeaseLabel": func(status operator.JobLeaseStatus, expiresAt *time.Time, evaluatedAt time.Time) string {
		switch status {
		case operator.JobLeaseActive:
			if expiresAt == nil {
				return "租約狀態不完整"
			}
			remaining := expiresAt.Sub(evaluatedAt)
			if remaining < 0 {
				remaining = 0
			}
			return "租約剩 " + state.HumanDur(remaining)
		case operator.JobLeaseExpired:
			if expiresAt == nil {
				return "租約已過期"
			}
			return "租約已過期 " + state.HumanDur(evaluatedAt.Sub(*expiresAt))
		case operator.JobLeaseNone:
			return "終態，不使用租約"
		case operator.JobLeaseUnclaimed:
			return "尚未取得租約"
		default:
			return "租約未知"
		}
	},
	"short": short,
	"intOrDash": func(v *int) any {
		if v == nil {
			return "—"
		}
		return *v
	},
	"independentVerdictLabel": independentVerdictLabel,
	"independentVerdictClass": independentVerdictClass,
	"assignmentStateLabel":    assignmentStateLabel,
	"assignmentStateClass":    assignmentStateClass,
	"hubEventLabel": func(kind string) string {
		switch kind {
		case store.JobLeaseExpired:
			return "工作單租約過期"
		case store.JobTimeout:
			return "工作單逾時"
		case store.HubDeploymentBoundaryPaused:
			return "部署安全閘門暫停"
		case store.HubDeploymentAbandoned:
			return "部署已明確放棄"
		default:
			return kind
		}
	},

	// ago 講人話。「11 小時」比 "11h0m0s" 好讀，而這個產品的整個賣點
	// 就是讓人一眼看懂。
	// ⚠ HumanDur 會把負的 duration 取絕對值，所以方向必須在這裡決定：
	// 來源時鐘比 Hub 快的時候，那個時間戳是「之後」，不是「之前」。
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "從未"
		}
		d := time.Since(t)
		if d < 0 {
			return state.HumanDur(d) + "後"
		}
		return state.HumanDur(d) + "前"
	},
	"dur":        state.HumanDur,
	"secondsDur": func(seconds int64) time.Duration { return time.Duration(seconds) * time.Second },
	"bytes":      humanBytes,
	"sub":        func(a, b time.Time) time.Duration { return a.Sub(b) },

	"pct": func(free, total int64) int {
		if total <= 0 {
			return 0
		}
		return 100 - int(free*100/total)
	},

	"timeOrDash": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"restoreDrillStateLabel": func(value store.RestoreDrillState) string {
		switch value {
		case store.RestoreDrillQueued:
			return "已排隊"
		case store.RestoreDrillRunning:
			return "驗證中"
		case store.RestoreDrillSucceeded:
			return "成功"
		case store.RestoreDrillFailed:
			return "失敗"
		default:
			return string(value)
		}
	},
	"restoreDrillStateClass": func(value store.RestoreDrillState) string {
		switch value {
		case store.RestoreDrillQueued, store.RestoreDrillRunning:
			return "st blue"
		case store.RestoreDrillSucceeded:
			return "st green"
		case store.RestoreDrillFailed:
			return "st red"
		default:
			return "st grey"
		}
	},
	"restoreDrillPhaseLabel": func(value store.RestoreDrillPhase) string {
		switch value {
		case store.RestoreDrillPhaseQueued:
			return "等待驗證"
		case store.RestoreDrillPhaseVerifying:
			return "驗證備份副本"
		case store.RestoreDrillPhaseComplete:
			return "完成"
		default:
			return string(value)
		}
	},

	// deref 讓樣板能安全地用指標欄位。nil 回 nil，樣板端用 {{if}} 擋掉。
	"deref": func(v any) any {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return nil
			}
			return rv.Elem().Interface()
		}
		return v
	},

	// credLabel 把五態翻成人話。
	// ⚠ 沒有任何一個會翻成「已登入」或「正常」—— 那會把 unknown 洗成綠燈，
	// 而那正是這個專案最想避免的事。
	"credLabel": func(s model.CredStatus) string {
		switch s {
		case model.CredAbsent:
			return "沒有安裝"
		case model.CredConfigured:
			return "有憑證，還沒到期"
		case model.CredExpiresSoon:
			return "即將過期"
		case model.CredExpired:
			// ⚠ 這裡只講事實（到期時間過了），不講「要重新登入」。
			// 要不要人動手是判決的事：一張閒置的 8 小時票過期 2 小時，
			// 下次用到自己續 —— 「發現」那一段會說它還在寬限內、什麼時候才算卡住。
			// 狀態欄跟發現欄各說各話，人會相信比較嚇人的那個。
			return "已過期"
		case model.CredUnknown:
			return "狀態未知"
		case model.CredFailed:
			return "最近一次請求失敗"
		}
		return string(s)
	},
	// verifyLabel 回答「左邊那個狀態是怎麼得到的」。
	//
	// ⚠ 它存在的唯一理由，是讓「有憑證，還沒到期」不可能被讀成「登入正常」。
	// 那兩件事在本機檔案裡分不出來：一個被伺服器端踢掉的 session，
	// 它的 auth.json 跟一張好票逐位元組相同。
	//
	// ⚠ 空字串回「來源不明」，不回空字串。這些是這個欄位出現之前就存下來的
	// 舊觀測，它們的來源是真的不知道 —— 而畫面上一個空格會被讀成「沒問題」。
	"verifyLabel": func(m model.VerifyMethod) string {
		switch m {
		case model.VerifyFileParse:
			return "只讀了本機檔案"
		case model.VerifyLiveRequest:
			return "打了一個真實請求"
		case "":
			return "來源不明（舊觀測）"
		}
		return string(m)
	},

	// credColor 把憑證判決映成狀態欄的顏色。
	//
	// ⚠ 文字只陳述「已過期」，不負責叫人動手；顏色卻不是事實，而是判決。
	// 還在寬限內的票，頁首是綠燈、發現區是 amber 並標「不影響狀態」，
	// 狀態欄若再亮紅字，人仍會相信比較嚇人的那一個。一個會自己好的紅燈，
	// 三天內就會被人靜音，靜音之後真正卡住的票也沒有人看。
	//
	// ⚠ 這裡只做「判決 → 顏色」的對應，不准計算時間，也不准向 state 取規則。
	// 判決只在 state 發生；HTML 是 adapter，不得長出第二套對機隊狀態的解讀。
	"credColor": func(credential operator.MachineCredential) string {
		if credential.GraceStatus == operator.MachineCredentialGraceInGrace {
			return "amber"
		}
		switch credential.Status {
		case model.CredConfigured:
			return "green"
		case model.CredExpiresSoon, model.CredUnknown:
			return "amber"
		case model.CredExpired, model.CredFailed:
			return "red"
		}
		return "grey"
	},

	"sevColor": func(n int) string {
		switch {
		case n >= 3:
			return "red"
		case n == 2:
			return "amber"
		}
		return "grey"
	},
	"sevLabel": func(n int) string {
		switch {
		case n >= 3:
			return "嚴重"
		case n == 2:
			return "警告"
		default:
			return "資訊"
		}
	},
}

// independentVerdictLabel names which of the eight the verdict is. Six of them
// are neither a pass nor a rule failure, so each gets its own sentence about what
// the second producer's rows show. Every sentence attributes the claim to the
// producer that wrote it; none of them promotes a report into a conclusion
// about the job.
func independentVerdictLabel(verdict string) string {
	switch store.IndependentVerdict(verdict) {
	case store.IndependentAbsent:
		return "沒有第二個 producer 為這張工作單寫過證據"
	case store.IndependentProducerRevoked:
		return "寫過這些證據的 producer 都已撤銷"
	case store.IndependentDigestMismatch:
		return "第二個 producer 看到的 artifact digest 與這張工作單的不同"
	case store.IndependentReleaseMismatch:
		return "第二個 producer 看到的 OpenClaw 版本與這張工作單的不同"
	case store.IndependentReleaseUnreported:
		return "第二個 producer 沒有回報可與這張工作單比較的結構化 OpenClaw 版本"
	case store.IndependentStale:
		return "第二個 producer 的證據都在這張工作單結束前送達"
	case store.IndependentFailed:
		return "第二個 producer 回報至少一條規則失敗"
	case store.IndependentPassed:
		// ⚠ 不可以寫成「digest 與這張工作單相同」。沒有回報 digest 的列一樣會落到
		// passed，而那是「沒有比過」，不是「比過而且一樣」。把沒比過講成相同，
		// 等於把一盞燈調亮到它照不到的地方。
		return "第二個 producer 回報的規則全部通過，沒有一列的 digest 與這張工作單相衝突"
	default:
		return verdict
	}
}

// independentVerdictClass keeps the four non-pass outcomes visually distinct
// from both a pass and a failure: only passed is green, and only the two that
// contradict the executor's account are amber.
func independentVerdictClass(verdict string) string {
	switch store.IndependentVerdict(verdict) {
	case store.IndependentPassed:
		return "st green"
	case store.IndependentFailed, store.IndependentDigestMismatch, store.IndependentReleaseMismatch:
		return "st amber"
	case store.IndependentProducerRevoked, store.IndependentStale:
		return "st grey"
	default:
		return "st grey"
	}
}

// assignmentStateLabel names what is still owed. absent verdicts are the
// common case on a young fleet, and without these an operator cannot tell
// "nobody was asked" from "somebody was asked and has not answered".
func assignmentStateLabel(state string) string {
	switch state {
	case operator.JobAssignmentReported:
		return "已送出獨立證據"
	case operator.JobAssignmentProducerRevoked:
		return "指派的 verifier 已撤銷，不會再回報"
	case operator.JobAssignmentWaitingForJob:
		return "等這張工作單結束才會發出"
	case operator.JobAssignmentAwaitingReport:
		return "已發出，等它回報"
	default:
		return state
	}
}

func assignmentStateClass(state string) string {
	switch state {
	case operator.JobAssignmentReported:
		return "st green"
	case operator.JobAssignmentProducerRevoked:
		return "st amber"
	default:
		return "st grey"
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
