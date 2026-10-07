package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// EnrollmentReport reads which of the machines this Hub was told to manage have
// actually turned up.
//
// ⚠ 用戶端自己再把分母加一次。這份報告只回答一個問題——說好要納管的機器，來了
// 沒有——而那個答案就是幾個數字。一份數字自己對不起來的回應不是拿來顯示的東西，
// 是拿來拒收的：分母少算一台，畫面上就會出現一個不存在的「全部都到齊了」。
func (c *Client) EnrollmentReport(ctx context.Context) (operator.EnrollmentReport, error) {
	var out operator.EnrollmentReport
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/enrollment-report", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: enrollment report returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "enrollment report", &out); err != nil {
		return out, err
	}
	if err := validateEnrollmentReport(out); err != nil {
		return out, err
	}
	return out, nil
}

func validateEnrollmentReport(report operator.EnrollmentReport) error {
	if report.SchemaVersion != operator.EnrollmentReportSchemaVersion ||
		report.EvaluatedAt.IsZero() || report.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: enrollment report identity is inconsistent")
	}
	if report.Registered < 0 || report.Denominator < 0 || report.Arrived < 0 ||
		report.Owed < 0 || report.Retired < 0 {
		return errors.New("operator client: enrollment report has negative values")
	}
	stages := operator.EnrollmentStages()
	if len(report.Stages) != len(stages) {
		return fmt.Errorf("operator client: enrollment report has %d stages, this version recognizes %d stages",
			len(report.Stages), len(stages))
	}
	counted := map[operator.EnrollmentStage]int{}
	for index, stage := range report.Stages {
		if stage.Stage != stages[index] {
			return fmt.Errorf("operator client: enrollment report stage %d is %q, this version expects %q",
				index, stage.Stage, stages[index])
		}
		if stage.Count < 0 {
			return fmt.Errorf("operator client: enrollment stage %q count is negative", stage.Stage)
		}
		if err := validateEnrollmentStageSentences(stage.Stage, stage.Title,
			stage.Meaning, stage.NextStep); err != nil {
			return err
		}
		counted[stage.Stage] = stage.Count
	}
	seen := map[operator.EnrollmentStage]int{}
	ids := map[string]bool{}
	tally := operator.EnrollmentReport{}
	for _, row := range report.Rows {
		if err := validateEnrollmentRow(row); err != nil {
			return err
		}
		if ids[row.MachineID] {
			return fmt.Errorf("operator client: enrollment report has duplicate rows for %s", row.MachineID)
		}
		ids[row.MachineID] = true
		seen[row.Stage]++
		tally.Registered++
		switch {
		case !row.InDenominator:
			tally.Retired++
		case operator.EnrollmentStageArrived(row.Stage):
			tally.Denominator, tally.Arrived = tally.Denominator+1, tally.Arrived+1
		default:
			tally.Denominator, tally.Owed = tally.Denominator+1, tally.Owed+1
		}
	}
	// ⚠ 每一個數字都要逐列數過，不是只檢查它們彼此加得起來。一份把「還沒到」搬進
	// 「已到」的摘要照樣加得起來，而它在畫面上印的是「全部都到齊了」——那是這份
	// 報告唯一不能說錯的一句話。
	if tally.Registered != report.Registered || tally.Denominator != report.Denominator ||
		tally.Arrived != report.Arrived || tally.Owed != report.Owed ||
		tally.Retired != report.Retired {
		return fmt.Errorf(
			"operator client: enrollment report summary reports arrived %d, owed %d, denominator %d, retired %d, total %d, "+
				"counted by row arrived %d, owed %d, denominator %d, retired %d, total %d",
			report.Arrived, report.Owed, report.Denominator, report.Retired, report.Registered,
			tally.Arrived, tally.Owed, tally.Denominator, tally.Retired, tally.Registered)
	}
	for stage, want := range counted {
		if seen[stage] != want {
			return fmt.Errorf("operator client: %q summary reports %d machines, counted by row %d machines",
				stage, want, seen[stage])
		}
	}
	return nil
}

func validateEnrollmentRow(row operator.EnrollmentRow) error {
	if err := validateMachineClientText("enrollment machine_id", row.MachineID, 256); err != nil {
		return err
	}
	if err := validateMachineClientText("enrollment display_name", row.DisplayName, 256); err != nil {
		return err
	}
	if err := validateEnrollmentStageSentences(row.Stage, row.StageTitle,
		row.Meaning, row.NextStep); err != nil {
		return err
	}
	if row.InDenominator != operator.EnrollmentStageInDenominator(row.Stage) {
		return fmt.Errorf("operator client: %s denominator flag does not match stage %q", row.MachineID, row.Stage)
	}
	if (row.RetiredAt != nil) == row.InDenominator {
		return fmt.Errorf("operator client: %s retired=%v but in_denominator=%v",
			row.MachineID, row.RetiredAt != nil, row.InDenominator)
	}
	if row.DeclaredAt.IsZero() || row.DeclaredAt.Location() != time.UTC {
		return fmt.Errorf("operator client: %s lacks usable declared timestamp", row.MachineID)
	}
	if row.PendingTickets < 0 || row.WaitedFor < 0 {
		return fmt.Errorf("operator client: %s ticket count or wait duration is negative", row.MachineID)
	}
	if row.PendingTickets == 0 && (row.TicketIssuedAt != nil || row.TicketExpiresAt != nil) {
		return fmt.Errorf("operator client: %s indicates no pending tickets, but provides ticket timestamp", row.MachineID)
	}
	for name, value := range map[string]*time.Time{
		"enrolled_at": row.EnrolledAt, "retired_at": row.RetiredAt,
		"last_checkin_received_at": row.LastCheckinReceivedAt,
		"ticket_issued_at":         row.TicketIssuedAt, "ticket_expires_at": row.TicketExpiresAt,
	} {
		if value != nil && (value.IsZero() || value.Location() != time.UTC) {
			return fmt.Errorf("operator client: %s %s is not a usable UTC timestamp", row.MachineID, name)
		}
	}
	return nil
}

// validateEnrollmentStageSentences 釘住「階段」與「它講的那三句話」是同一件事。
//
// 對面的 Hub 對某一個階段有第二種說法時，畫面上會出現一個看起來合理、其實指錯
// 下一步的句子——而操作員就是照那一句決定要重開票還是去機器上看 agent。
func validateEnrollmentStageSentences(stage operator.EnrollmentStage,
	title, meaning, nextStep string,
) error {
	// ⚠ 這一句擋的是「名冊上有一列，它的階段這個版本不認得」。逐階段的台數只對得到
	// 這個版本認得的那幾個，一列落在認不得的階段上就會從那個比對裡整個消失。
	if operator.EnrollmentStageTitle(stage) == "" {
		return fmt.Errorf("operator client: enrollment stage %q is unrecognized by this version", stage)
	}
	if title != operator.EnrollmentStageTitle(stage) ||
		meaning != operator.EnrollmentStageMeaning(stage) ||
		nextStep != operator.EnrollmentStageNextStep(stage) {
		return fmt.Errorf("operator client: enrollment stage %q sentences do not match this version", stage)
	}
	return nil
}
