package rollout

import (
	"reflect"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestNodeSatisfiesRange(t *testing.T) {
	expr := ">=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0"
	for _, tc := range []struct {
		version    string
		matched    bool
		understood bool
	}{
		{"v22.22.1", false, true},
		{"v22.22.3", true, true},
		{"v24.15.0", true, true},
		{"v23.1.0", false, true},
		{"v24.15.0", false, false}, // expression 會在下面換成看不懂的語法
	} {
		got, understood := NodeSatisfiesRange(tc.version, expr)
		if tc.understood == false {
			got, understood = NodeSatisfiesRange(tc.version, "^24.0.0")
		}
		if got != tc.matched || understood != tc.understood {
			t.Errorf("%s = (%v,%v)，預期 (%v,%v)", tc.version, got, understood, tc.matched, tc.understood)
		}
	}
	if got, understood := NodeSatisfiesRange("v22.22.2", ">=22.19.0"); !understood || !got {
		t.Errorf("22.22.2 應符合 >=22.19.0，得到 (%v,%v)", got, understood)
	}
}

func TestPlanSeparatesExclusionsAndBatches(t *testing.T) {
	members := []MachineFacts{
		{MachineID: "u", DisplayName: "unreachable", Reachable: false, NodeVersion: "24.15.0"},
		{MachineID: "b", DisplayName: "beta", Reachable: true, NodeVersion: "22.22.2"},
		{MachineID: "a", DisplayName: "alpha", Reachable: true, NodeVersion: "24.15.0"},
		{MachineID: "x", DisplayName: "conflict", Reachable: true, NodeVersion: "24.15.0", Conflict: true},
		{MachineID: "n", DisplayName: "node-unknown", Reachable: true},
	}
	got := Plan(members, ">=22.22.3 <23 || >=24.15.0 <25", 1)
	if got.Impact != 2 || got.Conflicts != 1 || got.MissingPackages != 1 || got.UnknownNodes != 1 || got.Unreachable != 1 {
		t.Fatalf("計數混在一起：%+v", got)
	}
	if got.Summary() != "影響 2 台，衝突 1，缺套件 1，unreachable 1，node 版本未知 1" {
		t.Fatalf("預覽句子不符：%q", got.Summary())
	}
	want := []Target{
		{MachineID: "a", DisplayName: "alpha", NodeVersion: "24.15.0", Reachable: true, BatchNo: 1},
		{MachineID: "b", DisplayName: "beta", NodeVersion: "22.22.2", Reachable: true, ExcludedReason: ExcludedMissingPackage},
		{MachineID: "x", DisplayName: "conflict", NodeVersion: "24.15.0", Reachable: true, ExcludedReason: ExcludedConflict},
		{MachineID: "n", DisplayName: "node-unknown", Reachable: true, ExcludedReason: ExcludedUnknownNode},
		{MachineID: "u", DisplayName: "unreachable", NodeVersion: "24.15.0", BatchNo: 2},
	}
	// 全部逐台清單照 display_name；只有沒排除的項目消耗批次位置。
	if !reflect.DeepEqual(got.Targets, want) {
		t.Fatalf("targets = %#v\nwant %#v", got.Targets, want)
	}
}

// 一台被合規性動作停發工作單的機器，開單給它只會卡在那裡直到租約過期，而操作員
// 讀到的會是「租約過期」——一個不是真正原因的原因。所以它先被排除，而且要排在
// 衝突與版本之前：那些說法對這台機器都是假的。
func TestPlanExcludesMachinesACompliancePolicyIsWithholdingWorkFrom(t *testing.T) {
	got := Plan([]MachineFacts{
		{MachineID: "a", DisplayName: "alpha", Reachable: true, NodeVersion: "24.15.0"},
		{MachineID: "b", DisplayName: "blocked", Reachable: true, NodeVersion: "24.15.0", Noncompliant: true},
		{MachineID: "c", DisplayName: "blocked-and-conflicting", Reachable: true,
			NodeVersion: "24.15.0", Noncompliant: true, Conflict: true},
		{MachineID: "d", DisplayName: "blocked-old-node", Reachable: true,
			NodeVersion: "20.0.0", Noncompliant: true},
	}, ">=24.15.0 <25", 5)
	if got.Impact != 1 || got.Noncompliant != 3 || got.Conflicts != 0 || got.MissingPackages != 0 {
		t.Fatalf("計數混在一起：%+v", got)
	}
	for _, target := range got.Targets[1:] {
		if target.ExcludedReason != ExcludedNoncompliant {
			t.Fatalf("%s 被講成 %q", target.DisplayName, target.ExcludedReason)
		}
		if target.BatchNo != 0 {
			t.Fatalf("%s 被排進第 %d 批", target.DisplayName, target.BatchNo)
		}
	}
	if got.Summary() != "影響 1 台，衝突 0，缺套件 0，unreachable 0，合規性停發工作單 3" {
		t.Fatalf("預覽句子不符：%q", got.Summary())
	}
	// 沒有機器被停發時，這句話不該出現 —— 機隊沒指派原則就不必讀到合規性字樣。
	clean := Plan([]MachineFacts{{MachineID: "a", DisplayName: "alpha",
		Reachable: true, NodeVersion: "24.15.0"}}, ">=24.15.0 <25", 5)
	if strings.Contains(clean.Summary(), "合規性") {
		t.Fatalf("沒有被停發的機器卻提到合規性：%q", clean.Summary())
	}
}

func TestPlanUnreadableEnginesIsUnknownNotCompatible(t *testing.T) {
	got := Plan([]MachineFacts{{MachineID: "a", DisplayName: "a", Reachable: true, NodeVersion: "24.15.0"}}, "^24.0.0", 5)
	if got.Impact != 0 || got.UnknownNodes != 1 || got.Targets[0].ExcludedReason != ExcludedUnknownNode {
		t.Fatalf("看不懂 engines 不准放行：%+v", got)
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []JobState
		next   bool
		want   Action
		stuck  []string
	}{
		{"還在跑", []JobState{{MachineID: "a", State: deploy.Running}}, true, Wait, nil},
		{"有失敗先停", []JobState{{MachineID: "a", State: deploy.Succeeded}, {MachineID: "b", State: deploy.Failed}}, true, Pause, []string{"b"}},
		{"全成功開下一批", []JobState{{MachineID: "a", State: deploy.Succeeded}}, true, OpenNext, nil},
		{"最後一批全成功", []JobState{{MachineID: "a", State: deploy.Succeeded}}, false, Finish, nil},
		{"拒絕也是停", []JobState{{MachineID: "x", State: deploy.Rejected}}, false, Pause, []string{"x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.states, tc.next)
			if got.Action != tc.want || !reflect.DeepEqual(got.StuckMachineIDs, tc.stuck) {
				t.Fatalf("Decide = %+v", got)
			}
		})
	}
}
