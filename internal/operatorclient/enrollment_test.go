package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// enrollmentClientReport 造一份六個階段都有人的報告，數字自己對得起來。
func enrollmentClientReport(now time.Time) operator.EnrollmentReport {
	declared := now.Add(-72 * time.Hour)
	enrolled := now.Add(-71 * time.Hour)
	checkin := now.Add(-3 * time.Minute)
	issued := now.Add(-2 * time.Hour)
	expires := now.Add(2 * time.Hour)
	expired := now.Add(-30 * time.Hour)
	retired := now.Add(-24 * time.Hour)

	report := operator.EnrollmentReport{
		SchemaVersion: operator.EnrollmentReportSchemaVersion, EvaluatedAt: now,
	}
	for index, stage := range operator.EnrollmentStages() {
		row := operator.EnrollmentRow{
			MachineID:     string(stage),
			DisplayName:   string(stage),
			Stage:         stage,
			StageTitle:    operator.EnrollmentStageTitle(stage),
			Meaning:       operator.EnrollmentStageMeaning(stage),
			NextStep:      operator.EnrollmentStageNextStep(stage),
			InDenominator: operator.EnrollmentStageInDenominator(stage),
			DeclaredAt:    declared.Add(time.Duration(index) * time.Minute),
			WaitedFor:     now.Sub(declared),
			Issues:        []string{},
		}
		switch stage {
		case operator.EnrollmentArrived:
			row.EnrolledAt, row.LastCheckinReceivedAt = &enrolled, &checkin
			row.WaitedFor = enrolled.Sub(declared)
		case operator.EnrollmentCredentialed:
			row.EnrolledAt = &enrolled
		case operator.EnrollmentWaiting:
			row.TicketIssuedAt, row.TicketExpiresAt = &issued, &expires
			row.PendingTickets = 1
		case operator.EnrollmentExpired:
			row.TicketIssuedAt, row.TicketExpiresAt = &expired, &expired
			row.PendingTickets = 1
		case operator.EnrollmentRetired:
			row.EnrolledAt, row.RetiredAt = &enrolled, &retired
			row.LastCheckinReceivedAt = &checkin
		}
		report.Rows = append(report.Rows, row)
		report.Registered++
		switch {
		case !row.InDenominator:
			report.Retired++
		case operator.EnrollmentStageArrived(stage):
			report.Denominator, report.Arrived = report.Denominator+1, report.Arrived+1
		default:
			report.Denominator, report.Owed = report.Denominator+1, report.Owed+1
		}
	}
	for _, stage := range operator.EnrollmentStages() {
		report.Stages = append(report.Stages, operator.EnrollmentStageCount{
			Stage: stage, Title: operator.EnrollmentStageTitle(stage), Count: 1,
			Meaning:  operator.EnrollmentStageMeaning(stage),
			NextStep: operator.EnrollmentStageNextStep(stage),
		})
	}
	return report
}

func TestTheEnrollmentClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, enrollmentClientReport(now))
	report, err := client.EnrollmentReport(t.Context())
	if err != nil {
		t.Fatalf("一致的註冊報告被拒絕：%v", err)
	}
	if *asked != "/v1/operator/enrollment-report" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
	if report.Registered != len(operator.EnrollmentStages()) ||
		report.Arrived+report.Owed != report.Denominator {
		t.Fatalf("report=%+v", report)
	}
}

// 這份報告的價值全在於那幾個數字。一份分母自己對不起來的回應——說全部都到齊了、
// 少送一列、把退役算進分母——不是拿來顯示的東西，是拿來拒收的。
func TestTheEnrollmentClientRefusesAReportThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.EnrollmentReport){
		"沒講 schema 版本": func(r *operator.EnrollmentReport) { r.SchemaVersion++ },
		"沒有讀取時刻":       func(r *operator.EnrollmentReport) { r.EvaluatedAt = time.Time{} },
		"讀取時刻不是 UTC": func(r *operator.EnrollmentReport) {
			r.EvaluatedAt = r.EvaluatedAt.In(time.FixedZone("CST", 8*3600))
		},
		"說全部都到齊了": func(r *operator.EnrollmentReport) {
			r.Arrived, r.Owed = r.Denominator, 0
		},
		"退役被算進分母":  func(r *operator.EnrollmentReport) { r.Denominator += r.Retired; r.Retired = 0 },
		"名冊列數對不起來": func(r *operator.EnrollmentReport) { r.Registered++ },
		"少送一列":     func(r *operator.EnrollmentReport) { r.Rows = r.Rows[1:] },
		"有負數":      func(r *operator.EnrollmentReport) { r.Owed, r.Arrived = -1, r.Arrived+1 },
		"兩列同一台": func(r *operator.EnrollmentReport) {
			r.Rows[1].MachineID = r.Rows[0].MachineID
		},
		"階段少一個": func(r *operator.EnrollmentReport) { r.Stages = r.Stages[1:] },
		"階段順序被換過": func(r *operator.EnrollmentReport) {
			r.Stages[0], r.Stages[1] = r.Stages[1], r.Stages[0]
		},
		"逐階段加起來跟名冊對不上": func(r *operator.EnrollmentReport) { r.Stages[0].Count++ },
		"摘要說的台數跟逐列數的不一樣": func(r *operator.EnrollmentReport) {
			r.Stages[0].Count, r.Stages[1].Count = 2, 0
		},
		"階段的台數是負的": func(r *operator.EnrollmentReport) {
			r.Stages[0].Count, r.Stages[1].Count = -1, 3
		},
		"不認得的階段": func(r *operator.EnrollmentReport) {
			r.Rows[0].Stage, r.Stages[0].Stage = "pending", "pending"
		},
		"不認得的階段連句子都不給": func(r *operator.EnrollmentReport) {
			r.Rows[0].Stage, r.Rows[0].StageTitle = "pending", ""
			r.Rows[0].Meaning, r.Rows[0].NextStep = "", ""
			r.Stages[0].Stage, r.Stages[0].Title = "pending", ""
			r.Stages[0].Meaning, r.Stages[0].NextStep = "", ""
		},
		// 摘要跟著那一列一起改，所以只有「階段」與「算不算分母」的綁定擋得住它。
		"退役的階段卻說算分母": func(r *operator.EnrollmentReport) {
			for index := range r.Rows {
				if r.Rows[index].Stage == operator.EnrollmentRetired {
					r.Rows[index].RetiredAt, r.Rows[index].InDenominator = nil, true
					r.Retired, r.Denominator, r.Owed = r.Retired-1, r.Denominator+1, r.Owed+1
					return
				}
			}
		},
		// 多出來的那一列落在這個版本認不得的階段上，逐階段的比對看不到它。
		"名冊上多一列，階段這個版本不認得": func(r *operator.EnrollmentReport) {
			extra := r.Rows[0]
			extra.MachineID, extra.DisplayName = "ghost", "ghost"
			extra.Stage, extra.StageTitle, extra.Meaning, extra.NextStep = "pending", "", "", ""
			retired := r.EvaluatedAt.Add(-time.Hour)
			extra.RetiredAt, extra.InDenominator = &retired, false
			extra.LastCheckinReceivedAt, extra.TicketIssuedAt, extra.TicketExpiresAt = nil, nil, nil
			extra.PendingTickets = 0
			r.Rows = append(r.Rows, extra)
			r.Registered, r.Retired = r.Registered+1, r.Retired+1
		},
		"階段的意思換了一種說法": func(r *operator.EnrollmentReport) {
			r.Stages[0].Meaning += "（大概）"
		},
		"階段的下一步換了一種說法": func(r *operator.EnrollmentReport) {
			r.Stages[0].NextStep = "再等等看。"
		},
		"列上的標題跟階段對不上": func(r *operator.EnrollmentReport) {
			r.Rows[0].StageTitle = operator.EnrollmentStageTitle(operator.EnrollmentRetired)
		},
		"列上的下一步跟階段對不上": func(r *operator.EnrollmentReport) {
			r.Rows[0].NextStep = operator.EnrollmentStageNextStep(operator.EnrollmentWaiting)
		},
		"分母旗標跟階段對不上": func(r *operator.EnrollmentReport) {
			r.Rows[0].InDenominator = !r.Rows[0].InDenominator
		},
		"退役了卻說算分母": func(r *operator.EnrollmentReport) {
			retired := r.EvaluatedAt.Add(-time.Hour)
			r.Rows[0].RetiredAt = &retired
		},
		"沒有宣告時刻": func(r *operator.EnrollmentReport) { r.Rows[0].DeclaredAt = time.Time{} },
		"宣告時刻不是 UTC": func(r *operator.EnrollmentReport) {
			r.Rows[0].DeclaredAt = r.Rows[0].DeclaredAt.In(time.FixedZone("CST", 8*3600))
		},
		"報到時刻不是 UTC": func(r *operator.EnrollmentReport) {
			for index := range r.Rows {
				if r.Rows[index].LastCheckinReceivedAt != nil {
					local := r.Rows[index].LastCheckinReceivedAt.In(time.FixedZone("CST", 8*3600))
					r.Rows[index].LastCheckinReceivedAt = &local
					return
				}
			}
		},
		"等待時間是負的": func(r *operator.EnrollmentReport) { r.Rows[0].WaitedFor = -time.Hour },
		"票數是負的":   func(r *operator.EnrollmentReport) { r.Rows[0].PendingTickets = -1 },
		"說沒有未用票卻給了票的時刻": func(r *operator.EnrollmentReport) {
			for index := range r.Rows {
				if r.Rows[index].PendingTickets == 0 {
					issued := r.EvaluatedAt.Add(-time.Hour)
					r.Rows[index].TicketIssuedAt = &issued
					return
				}
			}
		},
		"機器名稱裡有終端機跳脫序列": func(r *operator.EnrollmentReport) {
			r.Rows[0].DisplayName = "samplehub1\x1b[2J"
		},
	} {
		report := enrollmentClientReport(now)
		edit(&report)
		if _, err := complianceClientServer(t, report).EnrollmentReport(t.Context()); err == nil {
			t.Errorf("%s：自相矛盾的註冊報告被接受了", name)
		}
	}
}
