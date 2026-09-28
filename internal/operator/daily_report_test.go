package operator

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Strict clients reject unknown fields, so the wire shape and schema version
// are one contract. A future field must make this list and the version move in
// the same change.
func TestDailyReportWireShapeIsPinnedToSchemaVersionOne(t *testing.T) {
	if DailyReportSchemaVersion != 1 {
		t.Fatalf("daily report schema=%d；欄位清單跟它必須一起動", DailyReportSchemaVersion)
	}
	if DefaultDailyReportWindow != 24*time.Hour || MaxDailyReportWindow != 30*24*time.Hour ||
		MaxDailyReportBodyBytes != 64<<10 {
		t.Fatalf("daily report bounds=%s/%s/%d", DefaultDailyReportWindow,
			MaxDailyReportWindow, MaxDailyReportBodyBytes)
	}
	typeOf := reflect.TypeOf(DailyReportResult{})
	got := make([]string, 0, typeOf.NumField())
	for index := 0; index < typeOf.NumField(); index++ {
		got = append(got, strings.Split(typeOf.Field(index).Tag.Get("json"), ",")[0])
	}
	want := []string{"schema_version", "evaluated_at", "since", "window_seconds", "body"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("daily report 欄位是 %v，schema v1 認得的是 %v", got, want)
	}
}
