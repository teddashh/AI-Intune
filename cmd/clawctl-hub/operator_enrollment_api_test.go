package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func decodeEnrollmentReport(t *testing.T, body []byte) operator.EnrollmentReport {
	t.Helper()
	var report operator.EnrollmentReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestOperatorEnrollmentReportAPIAnswersWhoNeverArrived(t *testing.T) {
	f, _ := reportFixture(t)
	// 一台開了票、從來沒來的機器。正式環境現在就有這麼一列。
	if _, _, err := f.store.CreateEnrollTokenFor("never-came", 0); err != nil {
		t.Fatal(err)
	}
	rec := reportGet(t, f, "/v1/operator/enrollment-report")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", rec.Code,
			rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	report := decodeEnrollmentReport(t, rec.Body.Bytes())
	if report.SchemaVersion != operator.EnrollmentReportSchemaVersion {
		t.Fatalf("schema_version=%d", report.SchemaVersion)
	}
	if report.Arrived+report.Owed != report.Denominator ||
		report.Denominator+report.Retired != report.Registered ||
		len(report.Rows) != report.Registered {
		t.Fatalf("分母對不起來：%+v", report)
	}
	if report.Owed < 1 {
		t.Fatalf("有一台從來沒來，報告卻說每一台都到了：%+v", report)
	}
	owed := 0
	for _, row := range report.Rows {
		if row.InDenominator && !operator.EnrollmentStageArrived(row.Stage) {
			owed++
			if row.NextStep == "" {
				t.Errorf("%s 還沒到，卻沒有下一步", row.DisplayName)
			}
		}
	}
	if owed != report.Owed {
		t.Errorf("逐列數出 %d 台還沒到，摘要說 %d 台", owed, report.Owed)
	}
}

// 這份報告只讀「現在」，沒有任何範圍好挑。收下一個它不認得的 query，等於默默
// 忽略呼叫端真正要的東西。
func TestOperatorEnrollmentReportAPIRejectsAnyQuery(t *testing.T) {
	f, _ := reportFixture(t)
	for _, target := range []string{
		"/v1/operator/enrollment-report?days=7",
		"/v1/operator/enrollment-report?stage=expired",
		"/v1/operator/enrollment-report?",
	} {
		if rec := reportGet(t, f, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}

// 註冊報告是 view：它只講名冊上已經有的東西，不動任何一列。
func TestTheEnrollmentReportNeedsOnlyTheCapabilityItDeclares(t *testing.T) {
	for _, route := range []string{
		"GET /reports/enrollment", "GET /reports/enrollment.csv",
		"GET /v1/operator/enrollment-report",
	} {
		policy, ok := operatorRoutePolicies[route]
		if !ok {
			t.Fatalf("%s 沒有 operator capability policy", route)
		}
		if policy.Permission.String() != "view" {
			t.Errorf("%s 要 %s，但它只讀不寫", route, policy.Permission)
		}
	}
}
