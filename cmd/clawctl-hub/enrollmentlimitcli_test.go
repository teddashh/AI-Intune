package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func runEnrollmentLimitCLI(t *testing.T, base string, deps machineCommandDeps, argv ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runEnrollmentLimitCommandWithDeps(t.Context(),
		append([]string{"--hub-url", base}, argv...), &out, &errOut, deps)
	if err != nil {
		return out.String() + errOut.String(), err
	}
	return out.String(), nil
}

func enrollmentLimitCLIFixture(t *testing.T) (string, machineCommandDeps) {
	t.Helper()
	f, _ := reportFixture(t)
	return reportCLIServer(t, f)
}

// CLI 上的那一份要跟畫面回答同一句話：這個 Hub 還收不收得下一台。少了「上限算的
// 是什麼」，操作員看到「還可以再納管 3 台」會以為撤一張票就會變成 4 台。
func TestEnrollmentLimitCLIPrintsWhatTheLimitCountsAndWhatItBlocks(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	text, err := runEnrollmentLimitCLI(t, base, deps)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	for _, want := range []string{"沒有設註冊上限", "名冊上", "已退役", "退役才會"} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "上限 0") {
		t.Fatalf("沒設上限被講成上限 0：\n%s", text)
	}
}

func TestEnrollmentLimitCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	body, err := runEnrollmentLimitCLI(t, base, deps, "--json")
	if err != nil {
		t.Fatalf("err=%v\n%s", err, body)
	}
	var page operator.EnrollmentLimitResult
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if page.SchemaVersion != operator.EnrollmentLimitSchemaVersion || page.Headline == "" {
		t.Fatalf("page=%+v", page)
	}
}

// --preview 不可以改到任何東西。一個「預覽」會寫入的命令，會讓人不敢用它。
func TestEnrollmentLimitCLIPreviewChangesNothing(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	text, err := runEnrollmentLimitCLI(t, base, deps, "--set", "4", "--reason", "acceptance", "--preview")
	if err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	if !strings.Contains(text, "沒有套用") {
		t.Fatalf("預覽沒講出它沒有套用：\n%s", text)
	}
	after, err := runEnrollmentLimitCLI(t, base, deps)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, after)
	}
	if !strings.Contains(after, "沒有設註冊上限") {
		t.Fatalf("預覽寫進去了：\n%s", after)
	}
}

// 設上限、讀回來、擋住開票，是同一條路上的三件事。
func TestEnrollmentLimitCLISetsALimitThatThenBlocksIssuance(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	current, err := f.store.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	text, err := runEnrollmentLimitCLI(t, base, deps,
		"--set", strconv.Itoa(current.InDenominator), "--reason", "acceptance")
	if err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	if !strings.Contains(text, "現在開不了新的票") {
		t.Fatalf("設到跟現在一樣多，卻沒說開不了票：\n%s", text)
	}
	preview, err := f.store.PreviewOperatorEnrollToken("blocked", 3600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ApplyOperatorEnrollToken(store.OperatorEnrollTokenCreateRequest{
		DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
		PreviewDigest: preview.PreviewDigest, Reason: "should be refused",
		IdempotencyKey: "cli-blocked", RequestDigest: "sha256:cli-blocked",
		Audit: store.AuditEntry{SourceAddr: "100.64.0.10"},
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeEnrollmentLimitReached {
		t.Fatalf("CLI 設的上限沒有擋住開票：%v", err)
	}
}

// 取消上限之後，讀回來那一頁還是要說得出誰拿掉的、為什麼。
func TestEnrollmentLimitCLIClearSaysWhoRemovedIt(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	if text, err := runEnrollmentLimitCLI(t, base, deps, "--set", "9", "--reason", "先設一個"); err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	if text, err := runEnrollmentLimitCLI(t, base, deps, "--clear", "--reason", "不再限制台數"); err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	text, err := runEnrollmentLimitCLI(t, base, deps)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, text)
	}
	for _, want := range []string{"沒有設註冊上限", "不再限制台數", "上次是"} {
		if !strings.Contains(text, want) {
			t.Fatalf("取消之後少了 %q：\n%s", want, text)
		}
	}
}

func TestEnrollmentLimitCLIRefusesWhatItCannotRun(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	for name, argv := range map[string][]string{
		"--set 跟 --clear 一起": {"--set", "4", "--clear", "--reason", "r"},
		"--set 不是數字":         {"--set", "四", "--reason", "r"},
		"--set 是負數":          {"--set", "-1", "--reason", "r"},
		"--set 大到沒有意義":       {"--set", "10001", "--reason", "r"},
		// ⚠ 這幾個 Atoi 讀得出來。少了 canonical 那一關，"04" 會跟 "4" 變成兩個
		// 講不出差別的請求，而它們的 idempotency digest 不一樣。
		"--set 前面補零":        {"--set", "04", "--reason", "r"},
		"--set 帶正號":         {"--set", "+4", "--reason", "r"},
		"--set 前後有空白":       {"--set", " 4", "--reason", "r"},
		"改上限沒有理由":           {"--set", "4"},
		"理由前後有空白":           {"--set", "4", "--reason", " r "},
		"只讀卻給了理由":           {"--reason", "r"},
		"只讀卻要預覽":            {"--preview"},
		"--preview 配 retry": {"--set", "4", "--reason", "r", "--preview", "--idempotency-key", "k"},
		"retry 只給一半":        {"--set", "4", "--reason", "r", "--idempotency-key", "k"},
		"retry 少了 --expected-revision": {"--set", "4", "--reason", "r",
			"--idempotency-key", "k", "--preview-digest", "sha256:x"},
		"retry 少了 --preview-digest": {"--set", "4", "--reason", "r",
			"--idempotency-key", "k", "--expected-revision", "0"},
		"多餘的 positional": {"extra"},
	} {
		if text, err := runEnrollmentLimitCLI(t, base, deps, argv...); err == nil {
			t.Errorf("%s：被接受了\n%s", name, text)
		}
	}
}

// ⚠ 這裡要確認它是**在本地**停下來的，不是送出去之後被回應擋下來。一個「反正
// 送出去會被擋」的 CLI，會把一句講得清楚的錯誤換成一次網路往返加一句看不懂的話
// ——而且那次不該發生的寫入請求已經到了 Hub 門口。
func TestEnrollmentLimitCLIStopsLocallyInsteadOfLettingTheHubRefuse(t *testing.T) {
	base, deps := enrollmentLimitCLIFixture(t)
	for name, argv := range map[string][]string{
		"retry 少了 --expected-revision": {"--set", "4", "--reason", "r",
			"--idempotency-key", "k", "--preview-digest", "sha256:x"},
		"改上限沒有理由": {"--set", "4"},
		"理由前後有空白": {"--set", "4", "--reason", " r "},
	} {
		text, err := runEnrollmentLimitCLI(t, base, deps, argv...)
		if err == nil {
			t.Errorf("%s：照跑了\n%s", name, text)
			continue
		}
		if !strings.HasPrefix(err.Error(), "enrollment-limit:") {
			t.Errorf("%s：不是 CLI 自己擋下來的：%v", name, err)
		}
	}
	text, err := runEnrollmentLimitCLI(t, base, deps, "--set", "4", "--reason", "r",
		"--idempotency-key", "k", "--preview-digest", "sha256:x")
	if err == nil || !strings.Contains(err.Error(), "expected-revision") {
		t.Fatalf("沒講出缺的是哪一個：%v\n%s", err, text)
	}
}
