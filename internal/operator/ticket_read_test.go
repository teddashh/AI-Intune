package operator

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestTicketReadUsesFixedWindowFilterAndSafeEvidence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evaluatedAt := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	machineID := "ticket-safe-machine"
	if err := st.UpsertMachine(store.Machine{
		MachineID: machineID, DisplayName: strings.Repeat("m", 300), Expected: true,
		CreatedAt: evaluatedAt.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	provider := "=provider\u0001"
	for index, measured := range []string{evaluatedAt.Add(-time.Hour).Format(time.RFC3339), "not-a-time"} {
		if _, err := st.DB().Exec(`INSERT INTO ticket_occupancy_observation
(observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,agent_id,process_alive,run_status,last_error_text,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "safe-ticket-"+string(rune('a'+index)), machineID, provider, measured,
			evaluatedAt.Add(-time.Duration(index+1)*time.Minute).Format(time.RFC3339), "run", strings.Repeat("a", 300),
			0, "ok", strings.Repeat("e", 2200), "cron_run_logs"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`INSERT INTO ticket_occupancy_observation
(observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,process_alive,source)
VALUES(?,?,?,?,?,?,0,?)`, "outside-ticket", machineID, "outside", evaluatedAt.Add(-40*24*time.Hour).Format(time.RFC3339),
		evaluatedAt.Add(-40*24*time.Hour).Format(time.RFC3339), "old", "cron_run_logs"); err != nil {
		t.Fatal(err)
	}

	providerRef := TicketProviderRef(provider)
	result, err := New(st).ListTicketsContext(t.Context(), TicketReadRequest{Days: 7, ProviderRef: providerRef}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Window.From.Equal(evaluatedAt.Add(-7*24*time.Hour)) || !result.Window.To.Equal(evaluatedAt) ||
		result.Total != 1 || result.MatchedTotal != 1 || len(result.Items) != 1 ||
		result.Items[0].ProviderRef != providerRef || result.Items[0].Runs != 2 ||
		result.Coverage.MalformedMeasuredAtRows != 1 || result.CandidateRows != 2 {
		t.Fatalf("result=%+v", result)
	}
	item := result.Items[0]
	if len(item.Provider.Issues) == 0 || !item.Machines.Items[0].Truncated || !item.Agents.Items[0].Truncated ||
		!item.Errors[0].Text.Truncated || item.Provider.Text == provider {
		t.Fatalf("unsafe evidence projection=%+v", item)
	}
	csv, err := ReportCSV(TicketCSV(result))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csv, "'=provider") || strings.Contains(csv, "\u0001") {
		t.Fatalf("unsafe CSV=%q", csv)
	}
}

func TestTicketReadRequestBounds(t *testing.T) {
	for _, request := range []TicketReadRequest{{Days: -1}, {Days: 31}, {ProviderRef: "sha256:short"}} {
		if err := ValidateTicketReadRequest(request); err == nil {
			t.Fatalf("request %+v was accepted", request)
		}
	}
	normalized, err := NormalizeTicketReadRequest(TicketReadRequest{})
	if err != nil || normalized.Days != DefaultTicketReadDays {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
}

func TestTheTicketDurationVocabularySaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Duration
		expected string
	}{
		{name: "剛好同一刻", input: 0, expected: "0 分鐘"},
		{name: "不到一小時", input: 45 * time.Minute, expected: "45 分鐘"},
		{name: "差一分鐘就一小時", input: 59 * time.Minute, expected: "59 分鐘"},
		{name: "剛好一小時", input: time.Hour, expected: "1 小時"},
		{name: "快滿兩天", input: 47 * time.Hour, expected: "47 小時"},
		{name: "剛好兩天", input: 48 * time.Hour, expected: "2 天"},
		{name: "三天", input: 72 * time.Hour, expected: "3 天"},
		// 這一格是匯出檔裡唯一會講出「agent 時鐘比 Hub 快」的地方，負數不可以被當成一段正的時間長度印出去。
		{name: "agent 的時間跑到 Hub 前面", input: -5 * time.Minute, expected: "agent 時間晚於 Hub 評估時間"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := humanTicketDuration(test.input); got != test.expected {
				t.Errorf("humanTicketDuration(%s) = %q, want %q", test.input, got, test.expected)
			}
		})
	}
}
