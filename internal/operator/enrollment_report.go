package operator

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 註冊報告：說好要納管的機器，來了沒有。
//
// 開一張票就是一個人明確說出「我打算納管這台」，從那一刻起它就在分母裡。今天這個
// Hub 記得那個宣告，也記得票有沒有被用掉，但沒有任何一頁把兩件事擺在一起講——
// 一台開了票、票過期了、機器從來沒出現的機器，在名冊上跟一台正常運作的機器長得
// 幾乎一樣，只差一盞燈。這份報告把每一列攤成「它走到哪一步、下一步該做什麼」。
//
// ⚠ 它不自己判斷一台機器有沒有報到過，也不自己判斷一張票過期了沒有。前者讀的是
// 機隊清單用的那一份投影，後者讀的是單機頁在用的那一條 SQL 判準。一份自己算一次
// 的報告會跟權威頁面漂開，而漂掉的樣子是同一台機器在兩頁上有兩個答案。
const EnrollmentReportSchemaVersion = 1

// MaxEnrollmentReportMachines 是這份報告一次讀得了幾列名冊。
//
// 名冊就是分母，一份少算的分母比沒有分母更糟：它看起來像是全部。超過就拒絕，
// 並且說出實際有幾列。
const MaxEnrollmentReportMachines = 2000

var ErrInvalidEnrollmentReport = errors.New("operator: invalid enrollment report request")

// EnrollmentStage 是名冊上一列走到哪一步。每一列剛好落在一個階段。
type EnrollmentStage string

const (
	// EnrollmentArrived：票用掉了，機器也報到過。
	EnrollmentArrived EnrollmentStage = "arrived"
	// EnrollmentCredentialed：票用掉了，但這台從來沒有報到過。
	EnrollmentCredentialed EnrollmentStage = "credentialed"
	// EnrollmentWaiting：票還有效，還沒被用掉。
	EnrollmentWaiting EnrollmentStage = "waiting"
	// EnrollmentExpired：票過期了，沒有人用它。
	EnrollmentExpired EnrollmentStage = "expired"
	// EnrollmentNoTicket：沒有票可以用，這台也從來沒報到過。
	EnrollmentNoTicket EnrollmentStage = "no_ticket"
	// EnrollmentRetired：已退役，離開分母。
	EnrollmentRetired EnrollmentStage = "retired"
)

var enrollmentStageOrder = []EnrollmentStage{
	EnrollmentArrived, EnrollmentCredentialed, EnrollmentWaiting,
	EnrollmentExpired, EnrollmentNoTicket, EnrollmentRetired,
}

// EnrollmentStages returns every stage in canonical order.
func EnrollmentStages() []EnrollmentStage {
	return append([]EnrollmentStage(nil), enrollmentStageOrder...)
}

type enrollmentStageShape struct {
	title    string
	meaning  string
	nextStep string
	// inDenominator 說這個階段算不算在分母裡。只有退役不算。
	inDenominator bool
	// arrived 說這個階段的機器已經來過了。
	arrived bool
}

var enrollmentStageShapes = map[EnrollmentStage]enrollmentStageShape{
	EnrollmentArrived: {
		title:         "已納管",
		meaning:       "票用掉了，這台也報到過。",
		nextStep:      "看健康狀態就好，註冊這一段已經走完。",
		inDenominator: true,
		arrived:       true,
	},
	EnrollmentCredentialed: {
		title:         "拿了憑證沒回來",
		meaning:       "票用掉了，agent 拿到憑證，但這台從來沒有報到過。",
		nextStep:      "到機器上看 agent 起來了沒有；憑證已經發出去，重開一張票不會解決這件事。",
		inDenominator: true,
	},
	EnrollmentWaiting: {
		title:         "等它來",
		meaning:       "票還有效，還沒有人用它。",
		nextStep:      "在票到期之前把 bootstrap 跑完；到期之後這張票就不能用了。",
		inDenominator: true,
	},
	EnrollmentExpired: {
		title:         "票過期沒用",
		meaning:       "票到期了，從頭到尾沒有人用它，這台也沒有報到過。",
		nextStep:      "還要納管就再開一張票；不打算納管了就退役，讓它離開分母。",
		inDenominator: true,
	},
	EnrollmentNoTicket: {
		title:         "沒有票可以用",
		meaning:       "名冊上有這台，但現在沒有任何還沒用掉的票，它也從來沒有報到過。",
		nextStep:      "還要納管就開一張票；不打算納管了就退役，讓它離開分母。",
		inDenominator: true,
	},
	EnrollmentRetired: {
		title:    "已退役",
		meaning:  "這台已經退役，不算分母；名冊列與歷史都留著。",
		nextStep: "要重新納管就先取消退役，再開一張票。",
	},
}

// EnrollmentStageTitle / Meaning / NextStep 是畫面上那三句。
func EnrollmentStageTitle(stage EnrollmentStage) string {
	return enrollmentStageShapes[stage].title
}

func EnrollmentStageMeaning(stage EnrollmentStage) string {
	return enrollmentStageShapes[stage].meaning
}

func EnrollmentStageNextStep(stage EnrollmentStage) string {
	return enrollmentStageShapes[stage].nextStep
}

// EnrollmentStageInDenominator 說這個階段算不算分母。
func EnrollmentStageInDenominator(stage EnrollmentStage) bool {
	return enrollmentStageShapes[stage].inDenominator
}

// EnrollmentStageArrived 說這個階段的機器已經來過了。
func EnrollmentStageArrived(stage EnrollmentStage) bool {
	return enrollmentStageShapes[stage].arrived
}

// EnrollmentRow 是名冊上的一列。
type EnrollmentRow struct {
	MachineID   string          `json:"machine_id"`
	DisplayName string          `json:"display_name"`
	Stage       EnrollmentStage `json:"stage"`
	StageTitle  string          `json:"stage_title"`
	// Meaning / NextStep 跟階段一對一，一起送出去讓每一個平面講同一句話。
	Meaning  string `json:"meaning"`
	NextStep string `json:"next_step"`
	// InDenominator 說這一列算不算分母。
	InDenominator bool `json:"in_denominator"`
	// DeclaredAt 是名冊列建立的時刻，也就是有人說出「我打算納管這台」的時刻。
	DeclaredAt time.Time  `json:"declared_at"`
	EnrolledAt *time.Time `json:"enrolled_at"`
	RetiredAt  *time.Time `json:"retired_at"`
	// LastCheckinReceivedAt 為 nil 表示這台從來沒有報到過。
	LastCheckinReceivedAt *time.Time `json:"last_checkin_received_at"`
	// TicketIssuedAt / TicketExpiresAt 來自最新的那一張還沒用掉的票。
	TicketIssuedAt  *time.Time `json:"ticket_issued_at"`
	TicketExpiresAt *time.Time `json:"ticket_expires_at"`
	// PendingTickets 是這台身上還沒用掉的票數；正常是 0 或 1。
	PendingTickets int `json:"pending_tickets"`
	// WaitedFor 是「宣告到現在」或「宣告到報到」有多久。
	WaitedFor time.Duration `json:"waited_for_nanoseconds"`
	// Issues 是這一列在投影時被改過的地方，跟機隊清單同一套。
	Issues []string `json:"issues"`
}

// EnrollmentStageCount 是一個階段有幾台。
type EnrollmentStageCount struct {
	Stage    EnrollmentStage `json:"stage"`
	Title    string          `json:"title"`
	Count    int             `json:"count"`
	Meaning  string          `json:"meaning"`
	NextStep string          `json:"next_step"`
}

// EnrollmentReport 回答一句話：說好要納管的機器，來了沒有。
type EnrollmentReport struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	// Registered 是名冊上的總列數，含退役。
	Registered int `json:"registered"`
	// Denominator 是未退役的列數，也就是分母。
	Denominator int `json:"denominator"`
	// Arrived 是分母裡報到過的台數；Owed 是分母裡還沒到的台數。
	Arrived int `json:"arrived"`
	Owed    int `json:"owed"`
	// Retired 是退役的列數。
	Retired int                    `json:"retired"`
	Stages  []EnrollmentStageCount `json:"stages"`
	Rows    []EnrollmentRow        `json:"rows"`
}

// EnrollmentReportPath / ExportPath 是這份報告的兩個網址。
func EnrollmentReportPath() string { return "/reports/enrollment" }

func EnrollmentReportExportPath() string { return "/reports/enrollment.csv" }

// EnrollmentReportFor 產生這份報告。
func (s *Service) EnrollmentReport(evaluatedAt time.Time) (EnrollmentReport, error) {
	if evaluatedAt.IsZero() {
		return EnrollmentReport{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidEnrollmentReport)
	}
	evaluatedAt = evaluatedAt.UTC()
	overview, err := s.store.Overview(evaluatedAt)
	if err != nil {
		return EnrollmentReport{}, err
	}
	summaries := machineSummariesFrom(overview, evaluatedAt)
	if len(summaries) > MaxEnrollmentReportMachines {
		return EnrollmentReport{}, fmt.Errorf(
			"%w: registry has %d rows, exceeding the %d rows this report can read at once",
			ErrInvalidEnrollmentReport, len(summaries), MaxEnrollmentReportMachines)
	}
	tickets, err := s.store.PendingEnrollmentTickets(evaluatedAt)
	if err != nil {
		return EnrollmentReport{}, err
	}

	report := EnrollmentReport{
		SchemaVersion: EnrollmentReportSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		Registered:    len(summaries),
		// ⚠ 空名冊是這份報告有專屬句子的設計狀態；留成 nil 會輸出 JSON null，讓 Hub 自己的 strict client 拒收整頁。
		Rows: []EnrollmentRow{},
	}
	counts := map[EnrollmentStage]int{}
	for _, summary := range summaries {
		row, err := enrollmentRowFor(summary, tickets[summary.MachineID], evaluatedAt)
		if err != nil {
			return EnrollmentReport{}, err
		}
		report.Rows = append(report.Rows, row)
		counts[row.Stage]++
		switch {
		case !row.InDenominator:
			report.Retired++
		case EnrollmentStageArrived(row.Stage):
			report.Denominator++
			report.Arrived++
		default:
			report.Denominator++
			report.Owed++
		}
	}
	for _, stage := range enrollmentStageOrder {
		shape := enrollmentStageShapes[stage]
		report.Stages = append(report.Stages, EnrollmentStageCount{
			Stage: stage, Title: shape.title, Count: counts[stage],
			Meaning: shape.meaning, NextStep: shape.nextStep,
		})
	}
	sortEnrollmentRows(report.Rows)
	return report, nil
}

// enrollmentRowFor 把一列名冊放進剛好一個階段。
//
// ⚠ 「報到過沒有」用的是機隊清單那一份投影裡的最後一次報到，不是這裡自己再查一次
// machine_checkins。
func enrollmentRowFor(summary MachineSummary, ticket store.PendingEnrollmentTicket,
	evaluatedAt time.Time,
) (EnrollmentRow, error) {
	row := EnrollmentRow{
		MachineID: summary.MachineID, DisplayName: summary.DisplayName,
		DeclaredAt: summary.CreatedAt.UTC(), EnrolledAt: summary.EnrolledAt,
		RetiredAt: summary.RetiredAt, LastCheckinReceivedAt: summary.LastCheckinReceivedAt,
		PendingTickets: ticket.Pending, Issues: append([]string(nil), summary.Issues...),
	}
	if row.Issues == nil {
		row.Issues = []string{}
	}
	if ticket.Pending > 0 {
		issued, expires := ticket.CreatedAt.UTC(), ticket.ExpiresAt.UTC()
		if !ticket.CreatedAt.IsZero() {
			row.TicketIssuedAt = &issued
		}
		if !ticket.ExpiresAt.IsZero() {
			row.TicketExpiresAt = &expires
		}
	}
	arrived := summary.LastCheckinReceivedAt != nil
	switch {
	case summary.RetiredAt != nil:
		row.Stage = EnrollmentRetired
	case summary.EnrolledAt != nil && arrived:
		row.Stage = EnrollmentArrived
	case summary.EnrolledAt != nil:
		row.Stage = EnrollmentCredentialed
	case arrived:
		// 報到過卻沒有兌換時刻：這條路在協定上走不到——報到要有 agent 憑證，
		// 而憑證是兌換那一刻才發的。與其挑一個階段把它蓋過去，不如停下來。
		return EnrollmentRow{}, fmt.Errorf(
			"%w: %s reported but registry has no redeemed time", ErrInvalidEnrollmentReport, summary.MachineID)
	case ticket.Pending > 0 && !ticket.NewestExpired:
		row.Stage = EnrollmentWaiting
	case ticket.Pending > 0:
		row.Stage = EnrollmentExpired
	default:
		row.Stage = EnrollmentNoTicket
	}
	shape := enrollmentStageShapes[row.Stage]
	row.StageTitle, row.Meaning, row.NextStep = shape.title, shape.meaning, shape.nextStep
	row.InDenominator = shape.inDenominator
	row.WaitedFor = enrollmentWaitedFor(row, evaluatedAt)
	return row, nil
}

// enrollmentWaitedFor 是「宣告要納管」到「它真的來了」之間的時間；還沒來的算到現在。
func enrollmentWaitedFor(row EnrollmentRow, evaluatedAt time.Time) time.Duration {
	until := evaluatedAt
	if row.LastCheckinReceivedAt != nil && row.EnrolledAt != nil {
		until = row.EnrolledAt.UTC()
	} else if row.RetiredAt != nil {
		until = row.RetiredAt.UTC()
	}
	if until.Before(row.DeclaredAt) {
		return 0
	}
	return until.Sub(row.DeclaredAt)
}

// sortEnrollmentRows 把還沒到的排在前面，其中等最久的排最前面。
//
// ⚠ 排序不是美觀問題：這份報告存在的理由是「誰還沒來」，把已納管的機器排在最上面
// 等於把答案藏在第二頁。
func sortEnrollmentRows(rows []EnrollmentRow) {
	rank := map[EnrollmentStage]int{}
	for index, stage := range []EnrollmentStage{
		EnrollmentExpired, EnrollmentNoTicket, EnrollmentCredentialed,
		EnrollmentWaiting, EnrollmentArrived, EnrollmentRetired,
	} {
		rank[stage] = index
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank[rows[i].Stage] != rank[rows[j].Stage] {
			return rank[rows[i].Stage] < rank[rows[j].Stage]
		}
		if rows[i].WaitedFor != rows[j].WaitedFor {
			return rows[i].WaitedFor > rows[j].WaitedFor
		}
		return rows[i].MachineID < rows[j].MachineID
	})
}

// EnrollmentReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
func EnrollmentReportCSV(report EnrollmentReport) ReportCSVDocument {
	document := ReportCSVDocument{
		Filename: "ai-intune-enrollment.csv",
		Columns: []ReportCSVColumn{
			{Key: "machine", Header: "機器"},
			{Key: "machine_id", Header: "machine_id"},
			{Key: "stage", Header: "走到哪一步"},
			{Key: "in_denominator", Header: "算分母"},
			{Key: "declared_at", Header: "宣告納管"},
			{Key: "enrolled_at", Header: "兌換票"},
			{Key: "last_checkin_received_at", Header: "最後一次報到"},
			{Key: "ticket_issued_at", Header: "票開出"},
			{Key: "ticket_expires_at", Header: "票到期"},
			{Key: "pending_tickets", Header: "未用票"},
			{Key: "retired_at", Header: "退役"},
			{Key: "next_step", Header: "下一步"},
		},
	}
	for _, row := range report.Rows {
		document.Rows = append(document.Rows, []string{
			row.DisplayName,
			row.MachineID,
			row.StageTitle,
			enrollmentCSVBool(row.InDenominator),
			ReportCSVTime(row.DeclaredAt),
			ReportCSVOptionalTime(row.EnrolledAt),
			ReportCSVOptionalTime(row.LastCheckinReceivedAt),
			ReportCSVOptionalTime(row.TicketIssuedAt),
			ReportCSVOptionalTime(row.TicketExpiresAt),
			fmt.Sprintf("%d", row.PendingTickets),
			ReportCSVOptionalTime(row.RetiredAt),
			row.NextStep,
		})
	}
	return document
}

func enrollmentCSVBool(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
