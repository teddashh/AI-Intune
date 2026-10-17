package model

import (
	"errors"
	"regexp"
	"time"
)

// OOMKill carries attribution only, never raw logs, command lines or environment.
// Missing process/unit means the source did not establish that attribution.
type OOMKill struct {
	Time          time.Time `json:"time"`
	Source        string    `json:"source"`
	Unit          string    `json:"unit,omitempty"`
	Process       string    `json:"process,omitempty"`
	Count         uint64    `json:"count,omitempty"`
	PressureAvg10 *float64  `json:"pressure_avg10,omitempty"`
}

var ErrInvalidOOM = errors.New("invalid OOM evidence")

var oomName = regexp.MustCompile(`^[a-zA-Z0-9_.:@-]{1,128}$`)

func OOMName(s string) string {
	if s != "." && s != ".." && oomName.MatchString(s) {
		return s
	}
	return ""
}
func ValidOOM(e OOMKill, now time.Time) bool {
	return (e.Source == "oomd" || e.Source == "kernel" || e.Source == "memcg") && !e.Time.IsZero() && !e.Time.After(now.Add(5*time.Minute)) && !e.Time.Before(now.Add(-24*time.Hour)) && e.Unit == OOMName(e.Unit) && e.Process == OOMName(e.Process) && (e.PressureAvg10 == nil || (*e.PressureAvg10 >= 0 && *e.PressureAvg10 <= 100))
}
