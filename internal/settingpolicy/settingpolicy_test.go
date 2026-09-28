package settingpolicy

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func valid() Settings {
	return Settings{SchemaVersion: SchemaVersion,
		CheckinIntervalSeconds: 120, ObservationIntervalSeconds: 600}
}

// Verdict 只有常數、沒有宣告清單（沒有 Verdicts()），所以這張表是手寫的，新增常數時要自己回來加一列。
func TestTheSettingVerdictVocabularySaysTheseExactWords(t *testing.T) {
	labels := map[Verdict]string{
		VerdictApplied:       "已套用",
		VerdictPending:       "下次報到時套用",
		VerdictMismatch:      "機器回報的設定不是 Hub 指派的",
		VerdictUnknown:       "機器還沒回報設定",
		VerdictNeverReported: "機器從未報到",
		// 不認得的判決 token 刻意原樣回傳，讓操作員看見實際收到的值。
		Verdict("nope"): "nope",
	}

	verdicts := make([]Verdict, 0, len(labels))
	for verdict := range labels {
		verdicts = append(verdicts, verdict)
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i] < verdicts[j] })

	for _, verdict := range verdicts {
		if got, want := Label(verdict), labels[verdict]; got != want {
			t.Errorf("判決 token %q 拿到的句子是 %q，期望的句子是 %q", verdict, got, want)
		}
	}
}

// The defaults the Hub shipped as constants are still the defaults, so making
// settings assignable did not quietly re-pace a fleet nobody re-assigned.
func TestDefaultsMatchTheIntervalsTheHubShippedAsConstants(t *testing.T) {
	d := Defaults()
	if d.CheckinIntervalSeconds != 120 || d.ObservationIntervalSeconds != 600 {
		t.Fatalf("預設值變了：%+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("預設值自己不合法：%v", err)
	}
}

func TestValidateRefusesValuesTheAgentCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"心跳太快":        {func(s *Settings) { s.CheckinIntervalSeconds = 5 }, "30–3600"},
		"心跳太慢":        {func(s *Settings) { s.CheckinIntervalSeconds = 99999 }, "30–3600"},
		"觀測太快":        {func(s *Settings) { s.ObservationIntervalSeconds = 10 }, "60–86400"},
		"觀測太慢":        {func(s *Settings) { s.ObservationIntervalSeconds = 999999 }, "60–86400"},
		"觀測比心跳密":      {func(s *Settings) { s.CheckinIntervalSeconds, s.ObservationIntervalSeconds = 600, 120 }, "不可小於"},
		"schema 版本不對": {func(s *Settings) { s.SchemaVersion = 99 }, "只認得"},
	} {
		s := valid()
		tc.mutate(&s)
		err := s.Validate()
		if err == nil {
			t.Errorf("%s：應該被拒絕", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：訊息沒有說出範圍或原因：%v", name, err)
		}
	}
}

// A field the operator typed and the Hub dropped would show them a policy that
// is not the one they wrote.
func TestParseRefusesUnknownFieldsAndTrailingDocuments(t *testing.T) {
	if _, err := Parse([]byte(`{"schema_version":1,"checkin_interval_seconds":120,
	 "observation_interval_seconds":600,"jobs_enabled":true}`)); err == nil {
		t.Error("多了一個 Hub 不認得的欄位卻被接受")
	}
	if _, err := Parse([]byte(`{"schema_version":1,"checkin_interval_seconds":120,
	 "observation_interval_seconds":600}{"schema_version":1}`)); err == nil {
		t.Error("兩份文件黏在一起卻被接受")
	}
	got, err := Parse([]byte(`{"schema_version":1,"checkin_interval_seconds":300,
	 "observation_interval_seconds":900}`))
	if err != nil {
		t.Fatalf("合法文件被拒：%v", err)
	}
	if got.CheckinIntervalSeconds != 300 || got.ObservationIntervalSeconds != 900 {
		t.Fatalf("解出來的值不對：%+v", got)
	}
}

// TestEverySettingRefusalNamesOnlyTheFieldThatBrokeIt 確認拒絕訊息不會指錯欄位。
// 六個拒絕出口共用同一個 ErrInvalid，operator 又全映成同一個
// SETTING_POLICY_INVALID，句子原樣進入 operator 的 HTTP body，因此這句話是唯一能
// 說出該改哪個欄位的資訊。既有測試只斷言範圍或原因，從不檢查點名的欄位，讓三種
// 換名突變都能全樹通過；這裡刻意不重複斷言範圍。第 4 列還要比位置，因為對調兩個
// 欄位名後仍會同時含有兩個名字，只有釘住名字排在自己的值前面才能抓到。Parse 解碼
// 失敗的文字來自 encoding/json，例如 unknown field，這裡刻意不釘，以免釘死標準庫
// 的措辭。
func TestEverySettingRefusalNamesOnlyTheFieldThatBrokeIt(t *testing.T) {
	tests := []struct {
		name        string
		reject      func() error
		mustName    []string
		mustNotName []string
	}{
		{
			name: "schema 版本不對",
			reject: func() error {
				s := valid()
				s.SchemaVersion = 99
				return s.Validate()
			},
			mustName:    []string{"schema_version"},
			mustNotName: []string{"checkin_interval_seconds", "observation_interval_seconds"},
		},
		{
			name: "心跳超出範圍",
			reject: func() error {
				s := valid()
				s.CheckinIntervalSeconds = 5
				return s.Validate()
			},
			mustName:    []string{"checkin_interval_seconds"},
			mustNotName: []string{"schema_version", "observation_interval_seconds"},
		},
		{
			name: "觀測超出範圍",
			reject: func() error {
				s := valid()
				s.ObservationIntervalSeconds = 10
				return s.Validate()
			},
			mustName:    []string{"observation_interval_seconds"},
			mustNotName: []string{"schema_version", "checkin_interval_seconds"},
		},
		{
			name: "觀測比心跳密",
			reject: func() error {
				s := valid()
				s.CheckinIntervalSeconds = 601
				s.ObservationIntervalSeconds = 121
				return s.Validate()
			},
			mustName:    []string{"observation_interval_seconds", "checkin_interval_seconds"},
			mustNotName: []string{"schema_version"},
		},
		{
			name: "檔案裡有多份 JSON 文件",
			reject: func() error {
				_, err := Parse([]byte(`{"schema_version":1,"checkin_interval_seconds":120,
					"observation_interval_seconds":600}{"schema_version":1}`))
				return err
			},
			mustNotName: []string{
				"schema_version", "checkin_interval_seconds", "observation_interval_seconds",
			},
		},
	}

	for _, tc := range tests {
		err := tc.reject()
		if err == nil {
			t.Errorf("%s：這個輸入本來就該被拒絕，但 Validate／Parse 收下了它。", tc.name)
			continue
		}
		msg := err.Error()
		for _, field := range tc.mustName {
			if !strings.Contains(msg, field) {
				t.Errorf("%s：拒絕訊息沒有點名真正壞掉的欄位 %q，完整錯誤是 %v。",
					tc.name, field, err)
			}
		}
		for _, field := range tc.mustNotName {
			if strings.Contains(msg, field) {
				t.Errorf("%s：這句話會叫操作員去改一個沒有壞掉的欄位 %q，完整錯誤是 %v。",
					tc.name, field, err)
			}
		}
		if tc.name == "觀測比心跳密" {
			obsField := strings.Index(msg, "observation_interval_seconds")
			obsValue := strings.Index(msg, "121")
			chkField := strings.Index(msg, "checkin_interval_seconds")
			chkValue := strings.Index(msg, "601")
			if obsField < 0 || obsValue < 0 || chkField < 0 || chkValue < 0 ||
				!(obsField < obsValue && obsValue < chkField && chkField < chkValue) {
				t.Errorf("%s：欄位名與它的值對不起來，操作員會以為是另一個欄位超出範圍，完整錯誤是 %v。",
					tc.name, err)
			}
		}
	}
}

func TestCanonicalBytesAndDigestAreStable(t *testing.T) {
	a, err := valid().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// A second value built field-by-field in a different order must produce the
	// same bytes, or the digest would depend on how the struct was assembled.
	other := Settings{}
	other.ObservationIntervalSeconds = 600
	other.SchemaVersion = SchemaVersion
	other.CheckinIntervalSeconds = 120
	b, err := other.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical bytes 不穩定：\n%s\n%s", a, b)
	}
	if MustDigest(valid()) != MustDigest(other) {
		t.Fatal("同樣的設定算出不同的 digest")
	}
	changed := valid()
	changed.CheckinIntervalSeconds = 121
	if MustDigest(changed) == MustDigest(valid()) {
		t.Fatal("改了值 digest 卻沒變")
	}
	if _, err := Digest(Settings{}); err == nil {
		t.Error("不合法的設定竟然算得出 digest")
	}
	var round Settings
	if err := json.Unmarshal(a, &round); err != nil || round != valid() {
		t.Fatalf("canonical bytes 解不回同一個值：%v %+v", err, round)
	}
}

// Precedence is the whole point: somebody decided something about this one
// machine, and a later fleet-wide assignment must not take it back silently.
func TestResolvePrefersTheMachineOverItsChannelOverDefaults(t *testing.T) {
	machine := valid()
	machine.CheckinIntervalSeconds = 60
	channel := valid()
	channel.CheckinIntervalSeconds = 300
	rows := []Assignment{
		{Scope: ScopeChannel, ScopeID: "canary", PolicyID: "p-chan", Revision: 2,
			Digest: MustDigest(channel), Settings: channel, AssignedAt: "2026-09-12T10:00:00Z"},
		{Scope: ScopeMachine, ScopeID: "m1", PolicyID: "p-machine", Revision: 1,
			Digest: MustDigest(machine), Settings: machine, AssignedAt: "2026-09-11T10:00:00Z"},
	}
	got := Resolve("m1", "canary", rows)
	if got.Source != SourceMachine || got.PolicyID != "p-machine" {
		t.Fatalf("比較新的 channel 派工蓋過了機器自己的指派：%+v", got)
	}
	if got.Settings.CheckinIntervalSeconds != 60 {
		t.Fatalf("解出來的值不對：%+v", got.Settings)
	}

	if got := Resolve("m2", "canary", rows); got.Source != SourceChannel || got.PolicyID != "p-chan" {
		t.Fatalf("沒有機器指派時應該吃 channel：%+v", got)
	}
	if got := Resolve("m2", "stable", rows); got.Source != SourceDefault {
		t.Fatalf("channel 對不上時應該吃預設值：%+v", got)
	}
	// A machine with no channel must not accidentally match an assignment made
	// against the empty channel string.
	blank := []Assignment{{Scope: ScopeChannel, ScopeID: "", PolicyID: "p-blank", Revision: 1,
		Digest: MustDigest(channel), Settings: channel, AssignedAt: "2026-09-12T10:00:00Z"}}
	if got := Resolve("m3", "", blank); got.Source != SourceDefault {
		t.Fatalf("沒有 channel 的機器吃到了空字串 channel 的指派：%+v", got)
	}
	// Defaults carry no policy identity: there is no operator decision to cite.
	d := Resolve("m9", "", nil)
	if d.PolicyID != "" || d.Revision != 0 || d.Digest != "" {
		t.Fatalf("預設值不該帶 policy 身分：%+v", d)
	}
}

func TestResolveTakesTheNewestAssignmentInsideOneScope(t *testing.T) {
	older, newerS := valid(), valid()
	older.CheckinIntervalSeconds = 60
	newerS.CheckinIntervalSeconds = 90
	rows := []Assignment{
		{Scope: ScopeMachine, ScopeID: "m1", PolicyID: "old", Revision: 1,
			Digest: MustDigest(older), Settings: older, AssignedAt: "2026-09-10T00:00:00Z"},
		{Scope: ScopeMachine, ScopeID: "m1", PolicyID: "new", Revision: 1,
			Digest: MustDigest(newerS), Settings: newerS, AssignedAt: "2026-09-12T00:00:00Z"},
	}
	if got := Resolve("m1", "", rows); got.PolicyID != "new" {
		t.Fatalf("同一個 scope 裡沒有取最新的：%+v", got)
	}
	// Same instant, higher revision wins, so two writes in one second still order.
	rows[0].AssignedAt, rows[1].AssignedAt = "2026-09-12T00:00:00Z", "2026-09-12T00:00:00Z"
	rows[0].Revision = 7
	if got := Resolve("m1", "", rows); got.PolicyID != "old" || got.Revision != 7 {
		t.Fatalf("同一秒的兩筆沒有用 revision 決勝：%+v", got)
	}
}

// Worst evidence first. A machine that cannot be measured is never applied.
func TestJudgeNeverCallsAnUnmeasuredMachineApplied(t *testing.T) {
	eff := MustDigest(valid())
	behind := valid()
	behind.CheckinIntervalSeconds = 240
	known := []string{eff, MustDigest(behind)}

	for name, tc := range map[string]struct {
		report Report
		want   Verdict
	}{
		"從未報到":   {Report{EverCheckedIn: false, ReportedDigest: eff}, VerdictNeverReported},
		"報到但沒回報": {Report{EverCheckedIn: true}, VerdictUnknown},
		"回報同一份":  {Report{EverCheckedIn: true, ReportedDigest: eff}, VerdictApplied},
		"回報舊的版本": {Report{EverCheckedIn: true, ReportedDigest: MustDigest(behind)}, VerdictPending},
		"回報沒見過的": {Report{EverCheckedIn: true, ReportedDigest: "sha256:deadbeef"}, VerdictMismatch},
	} {
		if got := Judge(eff, known, tc.report); got != tc.want {
			t.Errorf("%s：判決 = %q，want %q", name, got, tc.want)
		}
	}
}

func TestSortAssignmentsPutsMachineScopeFirst(t *testing.T) {
	rows := []Assignment{
		{Scope: ScopeChannel, ScopeID: "stable", AssignedAt: "2026-09-12T00:00:00Z"},
		{Scope: ScopeMachine, ScopeID: "m2", AssignedAt: "2026-09-12T00:00:00Z"},
		{Scope: ScopeMachine, ScopeID: "m1", AssignedAt: "2026-09-12T00:00:00Z"},
		{Scope: ScopeChannel, ScopeID: "canary", AssignedAt: "2026-09-12T00:00:00Z"},
	}
	SortAssignments(rows)
	got := ""
	for _, r := range rows {
		got += string(r.Scope) + ":" + r.ScopeID + " "
	}
	if got != "machine:m1 machine:m2 channel:canary channel:stable " {
		t.Fatalf("排序不對：%s", got)
	}
}
