// Package settingpolicy holds the closed set of agent settings the Hub can
// assign, and the pure resolution that turns assignments into the values one
// machine actually runs under.
//
// 設定是**封閉的**。開一個任意 key/value map 會讓操作員寫得出沒有任何東西
// 會讀的設定 —— 那正是 docs/INTUNE-COVERAGE-MATRIX.md 說的空殼。這裡每一個
// 欄位都對應 check-in 回應裡真的會下發、agent 真的會照做的值。
package settingpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion is the settings document version. The Hub refuses a document
// it does not recognise rather than best-effort parsing it.
const SchemaVersion = 1

// Product defaults. These are what a machine with no assignment runs under,
// and they are the values the Hub shipped as constants before settings became
// assignable.
const (
	DefaultCheckinIntervalSeconds     = 120
	DefaultObservationIntervalSeconds = 600
)

// Bounds. A check-in slower than the stale threshold would make every machine
// look dead; faster than 30s spends the fleet's quota on heartbeats.
const (
	MinCheckinIntervalSeconds     = 30
	MaxCheckinIntervalSeconds     = 3600
	MinObservationIntervalSeconds = 60
	MaxObservationIntervalSeconds = 86400
)

// Settings is one complete, self-contained set of values. Every field is
// required: a partially specified document would make "what is this machine
// running under" depend on merge order.
type Settings struct {
	SchemaVersion              int `json:"schema_version"`
	CheckinIntervalSeconds     int `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int `json:"observation_interval_seconds"`
}

// Defaults returns the values a machine runs under with no assignment.
func Defaults() Settings {
	return Settings{
		SchemaVersion:              SchemaVersion,
		CheckinIntervalSeconds:     DefaultCheckinIntervalSeconds,
		ObservationIntervalSeconds: DefaultObservationIntervalSeconds,
	}
}

var ErrInvalid = errors.New("settingpolicy: invalid settings")

// Validate reports the first reason this document cannot be published. The
// message is operator copy: it says the bound and the value that broke it.
func (s Settings) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version 是 %d，這個 Hub 只認得 %d",
			ErrInvalid, s.SchemaVersion, SchemaVersion)
	}
	if s.CheckinIntervalSeconds < MinCheckinIntervalSeconds ||
		s.CheckinIntervalSeconds > MaxCheckinIntervalSeconds {
		return fmt.Errorf("%w: checkin_interval_seconds 是 %d，可用範圍是 %d–%d 秒",
			ErrInvalid, s.CheckinIntervalSeconds, MinCheckinIntervalSeconds, MaxCheckinIntervalSeconds)
	}
	if s.ObservationIntervalSeconds < MinObservationIntervalSeconds ||
		s.ObservationIntervalSeconds > MaxObservationIntervalSeconds {
		return fmt.Errorf("%w: observation_interval_seconds 是 %d，可用範圍是 %d–%d 秒",
			ErrInvalid, s.ObservationIntervalSeconds, MinObservationIntervalSeconds, MaxObservationIntervalSeconds)
	}
	// ⚠ 觀測比心跳還密沒有意義：observation_age 是靠心跳帶回來的，心跳之間
	// 多量幾次，Hub 也只會看到最後一次。允許它只會讓機器白燒 CPU。
	if s.ObservationIntervalSeconds < s.CheckinIntervalSeconds {
		return fmt.Errorf("%w: observation_interval_seconds (%d) 不可小於 checkin_interval_seconds (%d)；"+
			"觀測結果是靠心跳帶回來的",
			ErrInvalid, s.ObservationIntervalSeconds, s.CheckinIntervalSeconds)
	}
	return nil
}

// Canonical returns the byte form used for digests and storage. Field order is
// fixed by the struct, so the same settings always produce the same bytes.
func (s Settings) Canonical() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// Parse decodes a stored or submitted document. Unknown fields are refused:
// silently dropping a field an operator typed would show them a policy that
// is not the one they wrote.
func Parse(raw []byte) (Settings, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var s Settings
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("%w: %s", ErrInvalid, firstLine(err.Error()))
	}
	if dec.More() {
		return Settings{}, fmt.Errorf("%w: 檔案裡有多份 JSON 文件", ErrInvalid)
	}
	if err := s.Validate(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- resolution

// Scope is where an assignment was made. Only these two exist, matching the
// desired_state rule: a machine, or a channel. No arbitrary tags.
type Scope string

const (
	ScopeMachine Scope = "machine"
	ScopeChannel Scope = "channel"
)

// Assignment is one published policy pinned to one scope.
type Assignment struct {
	Scope      Scope    `json:"scope"`
	ScopeID    string   `json:"scope_id"`
	PolicyID   string   `json:"policy_id"`
	Revision   int64    `json:"policy_revision"`
	Digest     string   `json:"policy_digest"`
	Settings   Settings `json:"settings"`
	AssignedAt string   `json:"assigned_at"`
}

// Source says which assignment produced the values a machine runs under.
type Source string

const (
	// SourceDefault: nothing is assigned; the machine runs product defaults.
	SourceDefault Source = "default"
	// SourceChannel: the machine's channel carries an assignment.
	SourceChannel Source = "channel"
	// SourceMachine: the machine itself carries an assignment.
	SourceMachine Source = "machine"
)

// Effective is the answer to "what is this machine running under, and why".
type Effective struct {
	Settings Settings `json:"settings"`
	Source   Source   `json:"source"`
	// PolicyID/Revision/Digest are empty for SourceDefault: there is no policy
	// to point at, and inventing one would make the console claim an operator
	// decision that nobody made.
	PolicyID string `json:"policy_id,omitempty"`
	Revision int64  `json:"policy_revision,omitempty"`
	Digest   string `json:"policy_digest,omitempty"`
}

// Resolve picks the values one machine runs under.
//
// Precedence is machine over channel over default, and it is strict: a machine
// assignment is a decision somebody made about this one machine, so a later
// channel-wide assignment must not silently take it back.
func Resolve(machineID, channel string, assignments []Assignment) Effective {
	var machineHit, channelHit *Assignment
	for i := range assignments {
		a := &assignments[i]
		switch a.Scope {
		case ScopeMachine:
			if a.ScopeID == machineID && (machineHit == nil || newer(*a, *machineHit)) {
				machineHit = a
			}
		case ScopeChannel:
			if channel != "" && a.ScopeID == channel && (channelHit == nil || newer(*a, *channelHit)) {
				channelHit = a
			}
		}
	}
	switch {
	case machineHit != nil:
		return Effective{Settings: machineHit.Settings, Source: SourceMachine,
			PolicyID: machineHit.PolicyID, Revision: machineHit.Revision, Digest: machineHit.Digest}
	case channelHit != nil:
		return Effective{Settings: channelHit.Settings, Source: SourceChannel,
			PolicyID: channelHit.PolicyID, Revision: channelHit.Revision, Digest: channelHit.Digest}
	default:
		return Effective{Settings: Defaults(), Source: SourceDefault}
	}
}

// newer breaks ties inside one scope by assignment time, then by policy
// revision, so two assignments written in the same second still order.
func newer(a, b Assignment) bool {
	if a.AssignedAt != b.AssignedAt {
		return a.AssignedAt > b.AssignedAt
	}
	return a.Revision > b.Revision
}

// SortAssignments puts assignments in a stable display order: machine scope
// first, then channel, each by scope id.
func SortAssignments(rows []Assignment) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope == ScopeMachine
		}
		if rows[i].ScopeID != rows[j].ScopeID {
			return rows[i].ScopeID < rows[j].ScopeID
		}
		return rows[i].AssignedAt > rows[j].AssignedAt
	})
}
