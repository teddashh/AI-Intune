package main

import (
	"strings"
	"testing"
	"time"
)

func TestDailyReportIncludesEveryActiveMachineInDenominator(t *testing.T) {
	f := newJobsFixture(t, "managed-machine")
	observer := enrollViaHTTP(t, f.mux, f.store, "observer-outside-denominator")
	if _, err := f.store.DB().Exec(`UPDATE machine_registry SET expected=0 WHERE machine_id=?`, observer.id); err != nil {
		t.Fatal(err)
	}

	body, err := (&hub{store: f.store}).buildReport(jobsTestNow, jobsTestNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "observer-outside-denominator") {
		t.Fatalf("daily report omitted an active machine because of legacy expected=false:\n%s", body)
	}
	if !strings.Contains(body, "0/2 報到") {
		t.Fatalf("daily report denominator/reporting header drifted:\n%s", body)
	}
}
