package operator

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

// 一台機器的事件時間軸。
//
// 這一頁存在的理由是：同一台機器的故事目前散在四個地方——名冊說它什麼時候進來、
// 健康判定說它什麼時候變壞、工作單說派了什麼給它、稽核說有人對它做過什麼。要回答
// 「這台是從哪一刻開始不對的」得開四頁再自己在腦子裡照時間排一次，而那一次排序
// 沒有人檢查得到。
//
// ⚠ 這裡不新增任何一次查詢。每一列都來自別的頁面已經在用的那個讀取器，所以時間軸
// 說某張工作單什麼時候收尾，跟工作單頁說的是同一個時刻。自己重查一次就會漂，而漂
// 掉的樣子是兩頁對同一件事講不同的時間——操作員會相信先看到的那一個。
const MachineTimelineSchemaVersion = 1

const (
	DefaultMachineTimelineDays = 7
	MaxMachineTimelineDays     = 30

	// 每一個來源一次讀這麼多。時間軸不再自己截斷合併後的清單：讀進來的每一列都
	// 會出現，而每一個來源各自說清楚它在這段期間裡有沒有讀完。
	machineTimelineSourceLimit = store.MaxAuditReadPageSize
)

var ErrInvalidMachineTimeline = errors.New("operator: invalid machine timeline request")

// MachineTimelineSource 是一次讀取。完不完整是每一個來源各自的性質，所以「讀到
// 幾列」與「有沒有讀完」都掛在來源上，不是掛在合併後的總數上。
type MachineTimelineSource string

const (
	MachineTimelineSourceRegistry MachineTimelineSource = "registry"
	MachineTimelineSourceState    MachineTimelineSource = "state"
	MachineTimelineSourceJob      MachineTimelineSource = "job"
	MachineTimelineSourceAction   MachineTimelineSource = "action"
)

// MachineTimelineKind 是一列。它比來源細：同一次工作單讀取會生出開單與收尾兩種列。
type MachineTimelineKind string

const (
	MachineTimelineEnrolled       MachineTimelineKind = "enrolled"
	MachineTimelineRetired        MachineTimelineKind = "retired"
	MachineTimelineStateEntered   MachineTimelineKind = "state_entered"
	MachineTimelineJobOpened      MachineTimelineKind = "job_opened"
	MachineTimelineJobFinished    MachineTimelineKind = "job_finished"
	MachineTimelineOperatorAction MachineTimelineKind = "operator_action"
)

var machineTimelineSourceOf = map[MachineTimelineKind]MachineTimelineSource{
	MachineTimelineEnrolled:       MachineTimelineSourceRegistry,
	MachineTimelineRetired:        MachineTimelineSourceRegistry,
	MachineTimelineStateEntered:   MachineTimelineSourceState,
	MachineTimelineJobOpened:      MachineTimelineSourceJob,
	MachineTimelineJobFinished:    MachineTimelineSourceJob,
	MachineTimelineOperatorAction: MachineTimelineSourceAction,
}

var machineTimelineSourceOrder = []MachineTimelineSource{
	MachineTimelineSourceRegistry,
	MachineTimelineSourceState,
	MachineTimelineSourceJob,
	MachineTimelineSourceAction,
}

// MachineTimelineSources 是來源的固定順序。每一個讀時間軸的面都照這個順序交代
// 它讀了什麼，所以兩個面不會把同一份讀取講成不同的排法。
func MachineTimelineSources() []MachineTimelineSource {
	return append([]MachineTimelineSource(nil), machineTimelineSourceOrder...)
}

// MachineTimelineSourceOf 說一種列來自哪一個來源。認不得的列沒有來源。
func MachineTimelineSourceOf(kind MachineTimelineKind) (MachineTimelineSource, bool) {
	source, known := machineTimelineSourceOf[kind]
	return source, known
}

// MachineTimelineSourceLabel 是操作員看到的來源名字。
func MachineTimelineSourceLabel(source MachineTimelineSource) string {
	switch source {
	case MachineTimelineSourceRegistry:
		return "名冊"
	case MachineTimelineSourceState:
		return "健康判定"
	case MachineTimelineSourceJob:
		return "工作單"
	case MachineTimelineSourceAction:
		return "操作員動作"
	default:
		return string(source)
	}
}

// MachineTimelineSourceEvidence 說這個來源的列是誰寫下的。
func MachineTimelineSourceEvidence(source MachineTimelineSource) string {
	switch source {
	case MachineTimelineSourceRegistry:
		return "名冊自己記下的兩個時刻：進名冊與退役。"
	case MachineTimelineSourceState:
		return "Hub 自己判定這台進入哪一個狀態，以及判定的理由。"
	case MachineTimelineSourceJob:
		return "派給這台的工作單什麼時候開、什麼時候收尾。"
	case MachineTimelineSourceAction:
		return "有人透過 Hub 對這台做過什麼。"
	default:
		return ""
	}
}

// MachineTimelineIncompleteNextStep 是某個來源沒讀完時的下一步。
func MachineTimelineIncompleteNextStep(source MachineTimelineSource) string {
	return "把範圍縮短再看一次，" + MachineTimelineSourceLabel(source) + "才讀得完這段期間。"
}

type MachineTimelineWindow struct {
	Days int       `json:"days"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// MachineTimelineSourceRead 是一個來源在這段期間的交代。
//
// Complete 只有一種成立方式：這次讀取要嘛把它手上的東西讀完了，要嘛讀到的最舊
// 一列已經在窗開始之前。兩個都不成立就表示窗的前半段還有沒讀到的列，而一個不說
// 這件事的總數，會被當成「這段期間就只發生了這些」。
type MachineTimelineSourceRead struct {
	Source    MachineTimelineSource `json:"source"`
	Label     string                `json:"label"`
	Evidence  string                `json:"evidence"`
	Count     int                   `json:"count"`
	Complete  bool                  `json:"complete"`
	NextStep  string                `json:"next_step,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
}

// MachineTimelineEntry 是一列。Href 只在那一列真的有自己的證據頁時才有。
type MachineTimelineEntry struct {
	At      time.Time             `json:"at"`
	Kind    MachineTimelineKind   `json:"kind"`
	Source  MachineTimelineSource `json:"source"`
	Summary string                `json:"summary"`
	Detail  string                `json:"detail,omitempty"`
	Href    string                `json:"href,omitempty"`
}

type MachineTimelineResult struct {
	SchemaVersion int                         `json:"schema_version"`
	EvaluatedAt   time.Time                   `json:"evaluated_at"`
	MachineID     string                      `json:"machine_id"`
	DisplayName   string                      `json:"display_name"`
	Window        MachineTimelineWindow       `json:"window"`
	Total         int                         `json:"total"`
	Complete      bool                        `json:"complete"`
	Sources       []MachineTimelineSourceRead `json:"sources"`
	Entries       []MachineTimelineEntry      `json:"entries"`
}

type MachineTimelineRequest struct {
	MachineID string
	Days      int
}

// NormalizeMachineTimelineRequest fills in the default window and refuses one
// the Hub cannot read.
func NormalizeMachineTimelineRequest(request MachineTimelineRequest) (MachineTimelineRequest, error) {
	if request.MachineID == "" {
		return MachineTimelineRequest{}, fmt.Errorf("%w: machine id is required", ErrInvalidMachineTimeline)
	}
	if request.Days == 0 {
		request.Days = DefaultMachineTimelineDays
	}
	if request.Days < 1 || request.Days > MaxMachineTimelineDays {
		return MachineTimelineRequest{}, fmt.Errorf("%w: days 是 %d，可用範圍是 1–%d",
			ErrInvalidMachineTimeline, request.Days, MaxMachineTimelineDays)
	}
	return request, nil
}

// MachineTimeline merges what the Hub itself recorded about one machine.
func (s *Service) MachineTimeline(request MachineTimelineRequest, evaluatedAt time.Time) (MachineTimelineResult, error) {
	normalized, err := NormalizeMachineTimelineRequest(request)
	if err != nil {
		return MachineTimelineResult{}, err
	}
	if evaluatedAt.IsZero() {
		return MachineTimelineResult{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidMachineTimeline)
	}
	evaluatedAt = evaluatedAt.UTC()
	// 稽核的時間篩選只收整秒，而窗的兩端必須是同一種東西，否則「時間軸說的期間」
	// 與「稽核真的讀的期間」會差一個不到一秒的縫。往上取整讓評估的那一刻一定在
	// 窗裡面。
	to := evaluatedAt.Truncate(time.Second)
	if to.Before(evaluatedAt) {
		to = to.Add(time.Second)
	}
	window := MachineTimelineWindow{
		Days: normalized.Days,
		From: to.Add(-time.Duration(normalized.Days) * 24 * time.Hour),
		To:   to,
	}

	detail, err := s.MachineDetail(normalized.MachineID, evaluatedAt)
	if err != nil {
		return MachineTimelineResult{}, err
	}
	audit, err := s.ListAudit(AuditListRequest{
		MachineID: normalized.MachineID, From: &window.From, To: &window.To,
		Denials: AuditDenialsAll, Limit: machineTimelineSourceLimit,
	}, evaluatedAt)
	if err != nil {
		return MachineTimelineResult{}, err
	}
	jobs, err := s.ListJobs(JobListRequest{
		MachineID: normalized.MachineID, Limit: machineTimelineSourceLimit,
	}, evaluatedAt)
	if err != nil {
		return MachineTimelineResult{}, err
	}

	result := MachineTimelineResult{
		SchemaVersion: MachineTimelineSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		MachineID:     detail.Item.MachineID,
		DisplayName:   detail.Item.DisplayName,
		Window:        window,
		Complete:      true,
		// ⚠ 空時間軸由 Total=0 與各來源的 Count、Complete 明確表達；留成 nil 會輸出 JSON null，讓 Hub 自己的 strict client 拒收整頁。
		Entries: []MachineTimelineEntry{},
	}
	if result.MachineID != normalized.MachineID {
		return MachineTimelineResult{}, errors.New("operator: machine timeline read a different machine")
	}

	reads := map[MachineTimelineSource]*MachineTimelineSourceRead{}
	for _, source := range machineTimelineSourceOrder {
		reads[source] = &MachineTimelineSourceRead{
			Source: source, Label: MachineTimelineSourceLabel(source),
			Evidence: MachineTimelineSourceEvidence(source), Complete: true,
		}
	}

	add := func(at time.Time, kind MachineTimelineKind, summary, detail, href string) {
		if at.Before(window.From) || at.After(window.To) {
			return
		}
		source := machineTimelineSourceOf[kind]
		result.Entries = append(result.Entries, MachineTimelineEntry{
			At: at.UTC(), Kind: kind, Source: source,
			Summary: summary, Detail: detail, Href: href,
		})
		reads[source].Count++
	}

	// 名冊：進名冊與退役是名冊自己記下的兩個時刻。
	if detail.Item.EnrolledAt != nil {
		add(*detail.Item.EnrolledAt, MachineTimelineEnrolled, "進了名冊", "", "")
	}
	if detail.Item.RetiredAt != nil {
		add(*detail.Item.RetiredAt, MachineTimelineRetired, "退役", "", "")
	}

	// 健康判定：每一段狀態的起點。
	oldestState := time.Time{}
	for _, span := range detail.StateHistory.Items {
		if oldestState.IsZero() || span.EnteredAt.Before(oldestState) {
			oldestState = span.EnteredAt
		}
		add(span.EnteredAt, MachineTimelineStateEntered, "判定為 "+string(span.State), span.Reason.Text, "")
	}
	reads[MachineTimelineSourceState].Truncated = detail.StateHistory.Truncated
	reads[MachineTimelineSourceState].Complete = machineTimelineSourceComplete(
		detail.StateHistory.Truncated, oldestState, window.From)

	// 工作單：開單與收尾。
	oldestJob := time.Time{}
	for _, job := range jobs.Items {
		if oldestJob.IsZero() || job.CreatedAt.Before(oldestJob) {
			oldestJob = job.CreatedAt
		}
		href := "/jobs/" + job.JobID
		resource := job.ResourceKind + "：" + job.ResourceID
		add(job.CreatedAt, MachineTimelineJobOpened, "開了一張工作單", resource, href)
		if job.TerminalAt != nil {
			add(*job.TerminalAt, MachineTimelineJobFinished,
				"工作單"+machineTimelineJobOutcome(job.State), resource, href)
		}
	}
	reads[MachineTimelineSourceJob].Truncated = jobs.NextCursor != nil
	reads[MachineTimelineSourceJob].Complete = machineTimelineSourceComplete(
		jobs.NextCursor != nil, oldestJob, window.From)

	// 操作員動作：稽核已經照這台機器與這段期間篩過了。
	oldestAction := time.Time{}
	for _, event := range audit.Items {
		// 稽核讀取已經照時間篩過，所以每一列都有時間。一列沒有時間卻通過了時間
		// 篩選，表示讀取器跟它自己的條件對不起來——那種矛盾要停下來，不能挑一個
		// 時刻把它放上去。
		if event.At == nil {
			return MachineTimelineResult{}, fmt.Errorf(
				"operator: audit row %d passed a time filter without a time", event.AuditID)
		}
		at := event.At.UTC()
		if oldestAction.IsZero() || at.Before(oldestAction) {
			oldestAction = at
		}
		add(at, MachineTimelineOperatorAction, machineTimelineActionSummary(event),
			machineTimelineActionDetail(event), machineTimelineActionHref(normalized.MachineID, event.Action))
	}
	reads[MachineTimelineSourceAction].Truncated = audit.NextCursor != nil
	reads[MachineTimelineSourceAction].Complete = machineTimelineSourceComplete(
		audit.NextCursor != nil, oldestAction, window.From)

	sort.SliceStable(result.Entries, func(i, j int) bool {
		if !result.Entries[i].At.Equal(result.Entries[j].At) {
			return result.Entries[i].At.After(result.Entries[j].At)
		}
		return machineTimelineKindOrder(result.Entries[i].Kind) < machineTimelineKindOrder(result.Entries[j].Kind)
	})
	result.Total = len(result.Entries)
	for _, source := range machineTimelineSourceOrder {
		read := reads[source]
		if !read.Complete {
			read.NextStep = MachineTimelineIncompleteNextStep(source)
			result.Complete = false
		}
		result.Sources = append(result.Sources, *read)
	}
	return result, nil
}

// machineTimelineSourceComplete says whether this window has anything the read
// could not reach. A read that returned everything it had is complete; so is
// one whose oldest row already predates the window.
func machineTimelineSourceComplete(truncated bool, oldest, from time.Time) bool {
	if !truncated {
		return true
	}
	return !oldest.IsZero() && !oldest.After(from)
}

func machineTimelineKindOrder(kind MachineTimelineKind) int {
	switch kind {
	case MachineTimelineEnrolled:
		return 0
	case MachineTimelineRetired:
		return 1
	case MachineTimelineStateEntered:
		return 2
	case MachineTimelineOperatorAction:
		return 3
	case MachineTimelineJobOpened:
		return 4
	default:
		return 5
	}
}

func machineTimelineJobOutcome(jobState deploy.JobState) string {
	switch jobState {
	case deploy.Succeeded:
		return "成功"
	case deploy.Failed:
		return "失敗"
	case deploy.Rejected:
		return "被拒絕"
	case deploy.LeaseExpired:
		return "租約過期"
	case deploy.ManualIntervention:
		return "需要人介入"
	default:
		return "收尾（" + string(jobState) + "）"
	}
}

// machineTimelineActionSummary uses the same name for an action that the audit
// page uses. A second vocabulary for the same rows would let the two pages
// disagree about what happened.
func machineTimelineActionSummary(event AuditEvent) string {
	if event.Action == "" {
		return "動作無法判讀"
	}
	return event.Action
}

func machineTimelineActionDetail(event AuditEvent) string {
	outcome := "結果未記錄"
	if event.OutcomeKnown() {
		outcome = string(*event.Outcome)
	}
	who := event.WhoLabel()
	if who == "" {
		return outcome
	}
	return outcome + "，" + who
}

// machineTimelineActionHref 指向這一列自己的證據：稽核頁篩到這台機器的這個動作。
// 認不得的動作沒有辦法當篩選條件，就只篩機器。
func machineTimelineActionHref(machineID, action string) string {
	values := url.Values{}
	values.Set("machine_id", machineID)
	if store.IsKnownAuditAction(store.AuditAction(action)) {
		values.Set("action", action)
	}
	return "/audit?" + values.Encode()
}

// MachineTimelineCSV 是這條時間軸的匯出。
//
// 匯出的就是這個範圍裡讀到的全部——時間軸不再截斷合併後的清單，所以檔案裡的列數
// 就是畫面上的列數。檔名帶機器代號而不是顯示名稱：顯示名稱是上游文字，會出現在
// 檔名裡就會出現在別人的檔案系統裡。
func MachineTimelineCSV(result MachineTimelineResult) ReportCSVDocument {
	doc := ReportCSVDocument{
		Filename: "ai-intune-timeline-" + result.MachineID + ".csv",
		Columns: []ReportCSVColumn{
			{Key: "at", Header: "時間（UTC）"},
			{Key: "source", Header: "來源"},
			{Key: "kind", Header: "種類"},
			{Key: "summary", Header: "發生了什麼"},
			{Key: "detail", Header: "說明"},
			{Key: "evidence", Header: "證據"},
		},
	}
	for _, entry := range result.Entries {
		doc.Rows = append(doc.Rows, []string{
			ReportCSVTime(entry.At), MachineTimelineSourceLabel(entry.Source), string(entry.Kind),
			entry.Summary, entry.Detail, entry.Href,
		})
	}
	return doc
}

// MachineTimelineExportPath is where one machine's timeline export lives.
func MachineTimelineExportPath(machineID string) string {
	return "/machines/" + machineID + "/timeline.csv"
}

// MachineTimelinePath is where one machine's timeline lives.
func MachineTimelinePath(machineID string) string {
	return "/machines/" + machineID + "/timeline"
}
