package operator

import "time"

const (
	DailyReportSchemaVersion = 1
	DefaultDailyReportWindow = 24 * time.Hour
	MaxDailyReportWindow     = 30 * 24 * time.Hour
	MaxDailyReportBodyBytes  = 64 << 10
)

// DailyReportResult is the exact notification body the running Hub renders at
// one evaluation instant. Reading it does not send a notification or write a
// notification receipt.
type DailyReportResult struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	Since         time.Time `json:"since"`
	WindowSeconds int64     `json:"window_seconds"`
	Body          string    `json:"body"`
}
