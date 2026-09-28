package operatorclient

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineTimelineClientAcceptsEmptyEntries(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("開啟時間軸測試資料庫失敗：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const machineID = "machine-empty-timeline"
	if err := st.UpsertMachine(store.Machine{
		MachineID: machineID, DisplayName: "empty-timeline", Expected: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("建立時間軸測試機器失敗：%v", err)
	}
	evaluatedAt := time.Now().UTC().Add(365 * 24 * time.Hour)
	result, err := operator.New(st).MachineTimeline(operator.MachineTimelineRequest{
		MachineID: machineID, Days: 7,
	}, evaluatedAt)
	if err != nil {
		t.Fatalf("建立空時間軸失敗：%v", err)
	}
	if len(result.Entries) != 0 {
		t.Fatalf("時間窗應落在所有資料之後，但 entries 實際長度=%d", len(result.Entries))
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("編碼空時間軸失敗：%v", err)
	}
	client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
	})
	if _, err := client.MachineTimeline(t.Context(), machineID, 7); err != nil {
		t.Fatalf("空 entries 的時間軸被 client 拒收：%v", err)
	}
}
