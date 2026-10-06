package maintenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// Summary is one fleet-disk-clean/v1 line. Unknown fields are refused.
type Summary struct {
	Schema         string                     `json:"schema"`
	Version        string                     `json:"version"`
	Scope          string                     `json:"scope"`
	Host           string                     `json:"host"`
	User           string                     `json:"user"`
	Mode           string                     `json:"mode"`
	TS             string                     `json:"ts"`
	DurationS      int                        `json:"duration_s"`
	ConfigDigest   string                     `json:"config_digest"`
	Disk           SummaryDisk                `json:"disk"`
	CandidateBytes int64                      `json:"candidate_bytes"`
	FreedBytes     int64                      `json:"freed_bytes"`
	Categories     map[string]SummaryCategory `json:"categories"`
	Attention      string                     `json:"attention"`
	Weekly         *SummaryWeekly             `json:"weekly"`
	Root           *SummaryRoot               `json:"root"`
}

type SummaryDisk struct {
	Mount            string `json:"mount"`
	PctBefore        int    `json:"pct_before"`
	PctAfter         int    `json:"pct_after"`
	AvailBytesBefore int64  `json:"avail_bytes_before"`
	AvailBytesAfter  int64  `json:"avail_bytes_after"`
}

type SummaryCategory struct {
	Mode           string `json:"mode"`
	Status         string `json:"status"`
	CandidateBytes int64  `json:"candidate_bytes"`
	FreedBytes     int64  `json:"freed_bytes"`
	Items          int    `json:"items"`
	Note           string `json:"note"`
}

type SummaryWeekly struct {
	TS        string        `json:"ts"`
	Partial   bool          `json:"partial"`
	DurationS int           `json:"duration_s"`
	TopDirs   []SummaryPath `json:"top_dirs"`
	Backups   []SummaryPath `json:"backups"`
}

type SummaryPath struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// SummaryRoot is the read-only embedded root-timer summary. The user run
// does not produce it and the Hub does not treat it as its own verdict.
type SummaryRoot struct {
	TS             string                         `json:"ts"`
	Mode           string                         `json:"mode"`
	FreedBytes     int64                          `json:"freed_bytes"`
	CandidateBytes int64                          `json:"candidate_bytes"`
	Attention      string                         `json:"attention"`
	ConfigDigest   string                         `json:"config_digest"`
	Stale          bool                           `json:"stale"`
	Categories     map[string]SummaryRootCategory `json:"categories"`
}

type SummaryRootCategory struct {
	Status         string `json:"status"`
	CandidateBytes int64  `json:"candidate_bytes"`
	FreedBytes     int64  `json:"freed_bytes"`
}

// ParseSummary decodes one JSON line. A trailing newline is allowed.
// A second JSON value, an unknown field, or a document over MaxSummaryBytes
// is refused.
func ParseSummary(raw []byte) (Summary, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return Summary{}, errors.New("maintenance: summary is empty")
	}
	if len(raw) > MaxSummaryBytes {
		return Summary{}, fmt.Errorf("maintenance: summary exceeds %d bytes", MaxSummaryBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Summary
	if err := dec.Decode(&s); err != nil {
		return Summary{}, fmt.Errorf("maintenance: summary JSON: %s", err.Error())
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Summary{}, errors.New("maintenance: summary has trailing data")
	}
	if err := s.validate(); err != nil {
		return Summary{}, err
	}
	return s, nil
}

// ParsedTS is the summary timestamp in UTC. The Hub uses its own received_at
// for staleness; this value is the script's clock.
func (s Summary) ParsedTS() (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s.TS)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func (s Summary) validate() error {
	if s.Schema != SummarySchema {
		return fmt.Errorf("maintenance: summary schema must be %s", SummarySchema)
	}
	if !versionRE.MatchString(s.Version) {
		return errors.New("maintenance: summary version must look like 1.1.0")
	}
	if s.Scope != ScopeUser && s.Scope != ScopeRoot {
		return errors.New("maintenance: summary scope must be user or root")
	}
	if s.Mode != "dry-run" && s.Mode != "apply" && s.Mode != "mixed" {
		return errors.New("maintenance: summary mode must be dry-run, apply, or mixed")
	}
	if err := plainName("host", s.Host); err != nil {
		return err
	}
	if err := plainName("user", s.User); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, s.TS); err != nil {
		return errors.New("maintenance: summary ts must be RFC3339")
	}
	if s.DurationS < 0 {
		return errors.New("maintenance: summary duration_s must be >= 0")
	}
	if s.ConfigDigest != "none(defaults)" && !digestRE.MatchString(s.ConfigDigest) {
		return errors.New("maintenance: summary config_digest must be sha256:<64 hex> or none(defaults)")
	}
	if s.Disk.Mount == "" || len(s.Disk.Mount) > 128 || strings.ContainsAny(s.Disk.Mount, "\n\r") {
		return errors.New("maintenance: summary disk.mount is missing or too long")
	}
	if s.Disk.PctBefore < 0 || s.Disk.PctBefore > 100 || s.Disk.PctAfter < 0 || s.Disk.PctAfter > 100 {
		return errors.New("maintenance: summary disk percent must be between 0 and 100")
	}
	if s.Disk.AvailBytesBefore < 0 || s.Disk.AvailBytesAfter < 0 || s.CandidateBytes < 0 || s.FreedBytes < 0 {
		return errors.New("maintenance: summary byte counts must be >= 0")
	}
	if s.Categories == nil {
		return errors.New("maintenance: summary categories is required")
	}
	if len(s.Categories) > 16 {
		return errors.New("maintenance: summary categories has too many entries")
	}
	for name, cat := range s.Categories {
		if !protectToken.MatchString(name) || strings.HasPrefix(name, "-") {
			return fmt.Errorf("maintenance: summary category %q is not a name", name)
		}
		if cat.Mode != "dry-run" && cat.Mode != "apply" {
			return fmt.Errorf("maintenance: summary category %s mode is invalid", name)
		}
		switch cat.Status {
		case "ok", "skipped", "error", "busy":
		default:
			return fmt.Errorf("maintenance: summary category %s status is invalid", name)
		}
		if cat.CandidateBytes < 0 || cat.FreedBytes < 0 || cat.Items < 0 {
			return fmt.Errorf("maintenance: summary category %s counts must be >= 0", name)
		}
		if len(cat.Note) > 300 {
			return fmt.Errorf("maintenance: summary category %s note exceeds 300 bytes", name)
		}
	}
	if len(s.Attention) > 4000 {
		return errors.New("maintenance: summary attention exceeds 4000 bytes")
	}
	if s.Scope == ScopeRoot && (s.Weekly != nil || s.Root != nil) {
		return errors.New("maintenance: a root summary does not embed weekly or root")
	}
	if s.Weekly != nil {
		if s.Weekly.DurationS < 0 {
			return errors.New("maintenance: summary weekly duration_s must be >= 0")
		}
		if len(s.Weekly.TopDirs) > 5 || len(s.Weekly.Backups) > 10 {
			return errors.New("maintenance: summary weekly lists are too long")
		}
	}
	if s.Root != nil {
		if s.Root.Categories == nil {
			return errors.New("maintenance: embedded root categories is required")
		}
		if len(s.Root.Attention) > 4000 {
			return errors.New("maintenance: embedded root attention exceeds 4000 bytes")
		}
	}
	return nil
}

func plainName(field, value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("maintenance: summary %s must be 1-128 characters", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("maintenance: summary %s contains a control character", field)
		}
	}
	return nil
}
