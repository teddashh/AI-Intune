package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// 一張剛過期、還在等它自己續回來的短命票，不進早報；過了一個壽命還沒續，才進。
//
// ⚠ 早報答的是「今天要動什麼手」。2026-09-05 samplehub1 的 claude 一天內「過期」
// 兩輪、每輪一小時、然後自己好 —— 那種票天天上早報，早報三天內就會被靜音。
// 判斷用的是 state.CredGrace，跟詳細頁的判決同一支；這裡守的是
// buildReport 沒有自己另外寫一份規則（PHASE1 §5.13：四個地方各寫一次同一條
// 規則，就是四個各自會忘記的地方）。
func TestMorningReportSkipsExpiredCredentialStillInGrace(t *testing.T) {
	mux, st := emptyMetricsFixture(t)
	h := &hub{store: st}
	m := enrollViaHTTP(t, mux, st, "samplehub1")
	postCheckin(t, mux, m.token, http.StatusOK)

	// Keep the whole synthetic ledger before the report evaluation instant.
	// postCheckin uses the real clock; moving the frozen report one minute ahead
	// leaves deterministic room for both transitions without sleeping.
	now := time.Now().UTC().Add(time.Minute)
	observe := func(expiredFor time.Duration, reconciledAt time.Time) {
		mt := now.Add(-expiredFor - 8*time.Hour) // 壽命 8 小時的票
		exp := now.Add(-expiredFor)
		b := model.ObservationBatch{MeasuredAt: now.Add(-time.Minute)}
		b.Credentials = []model.Credential{{
			Provider: "claude", Status: model.CredExpired, ExpiresAt: &exp, FileMTime: &mt,
		}}
		if err := st.RecordObservation(m.id, b, now.Add(-time.Minute)); err != nil {
			t.Fatalf("observation: %v", err)
		}
		// 跟服務一樣走一次對帳，狀態史才有「什麼時候開始的」——
		// 否則早報會把它讀成從西元元年就壞著，收成 still broken since，看不到細節。
		// ⚠ 不呼叫 h.reconcile()：它用真正的 wall clock；race 跑慢時可能把
		// transition 寫在這個 fixture 的 frozen now 之後，讓 buildReport(now)
		// 合理地看不到它。明確傳 evaluation instant 才能測我們真正要守的規則。
		if err := st.ReconcileFleet(reconciledAt); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	observe(90*time.Minute, now.Add(-2*time.Second)) // 過期 1.5 小時：寬限內
	rep, err := h.buildReport(now, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if strings.Contains(rep, "claude") {
		t.Errorf("寬限內的過期票不該上早報：\n%s", rep)
	}

	observe(9*time.Hour, now.Add(-time.Second)) // 過期 9 小時：超過一個壽命
	rep, err = h.buildReport(now, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(rep, "claude") {
		t.Errorf("過了寬限還沒續回來的票一定要上早報：\n%s", rep)
	}
}
