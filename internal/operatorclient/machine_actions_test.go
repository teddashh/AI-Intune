package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func machineActionCatalogue(now time.Time) operator.MachineActionCatalogue {
	return operator.MachineActionCatalogue{
		SchemaVersion: operator.MachineActionsSchemaVersion, EvaluatedAt: now,
		MachineID: "m1", DisplayName: "samplehub1", State: store.MachineLifecycleActive,
		Available: 1, Blocked: 1,
		Actions: []operator.MachineAction{
			{
				Kind: operator.MachineActionConnect, Label: "連到這台的 BAT",
				Effect: "取得位址並留紀錄。", Capability: operator.MachineActionCapabilityOperate,
				Surface:   operator.MachineActionSurfaceWeb,
				Confirm:   operator.MachineActionConfirmNone,
				Available: true,
			},
			{
				Kind: operator.MachineActionRetire, Label: "退役",
				Effect: "退出分母。", Capability: operator.MachineActionCapabilityAdmin,
				Surface: operator.MachineActionSurfaceOperatorAPI,
				Confirm: operator.MachineActionConfirmDisplayName,
				Method:  "PUT", Path: "/v1/operator/machines/{id}/lifecycle",
				PreviewPath: "/v1/operator/machines/{id}/lifecycle-preview",
				Available:   false, Blocker: operator.MachineActionBlockerActiveJobs,
				Situation: "這台還有 2 張未結束的工作單。",
				NextStep:  operator.MachineActionBlockerNextStep(operator.MachineActionBlockerActiveJobs),
			},
		},
	}
}

func TestMachineActionsClientAcceptsACoherentCatalogue(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	catalogue, err := complianceClientServer(t, machineActionCatalogue(now)).
		MachineActions(t.Context(), "m1")
	if err != nil {
		t.Fatalf("一致的目錄被拒絕：%v", err)
	}
	if len(catalogue.Actions) != 2 || catalogue.Available != 1 || catalogue.Blocked != 1 {
		t.Fatalf("catalogue=%+v", catalogue)
	}
}

func TestMachineActionsClientRefusesACatalogueThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.MachineActionCatalogue){
		"問的是另一台":   func(c *operator.MachineActionCatalogue) { c.MachineID = "m2" },
		"可用張數對不起來": func(c *operator.MachineActionCatalogue) { c.Available = 2 },
		"總數對得起來但可用被擋分錯": func(c *operator.MachineActionCatalogue) {
			c.Available, c.Blocked = 2, 0
		},
		"blocker 不認得而且也沒下一步": func(c *operator.MachineActionCatalogue) {
			c.Actions[1].Blocker, c.Actions[1].NextStep = "whatever", ""
		},
		"可用卻附著理由": func(c *operator.MachineActionCatalogue) {
			c.Actions[0].Blocker = operator.MachineActionBlockerRetired
		},
		"被擋卻不說現在怎樣": func(c *operator.MachineActionCatalogue) { c.Actions[1].Situation = "" },
		"下一步跟 blocker 不符": func(c *operator.MachineActionCatalogue) {
			c.Actions[1].NextStep = operator.MachineActionBlockerNextStep(operator.MachineActionBlockerRetired)
		},
		"blocker 不認得":               func(c *operator.MachineActionCatalogue) { c.Actions[1].Blocker = "whatever" },
		"沒有後果那句話":                   func(c *operator.MachineActionCatalogue) { c.Actions[0].Effect = "" },
		"路徑不是這台機器的":                 func(c *operator.MachineActionCatalogue) { c.Actions[0].Path = "/v1/operator/deployments" },
		"surface 不認得":               func(c *operator.MachineActionCatalogue) { c.Actions[0].Surface = "terminal" },
		"web surface 帶了方法":          func(c *operator.MachineActionCatalogue) { c.Actions[0].Method = "GET" },
		"operator API surface 沒有方法": func(c *operator.MachineActionCatalogue) { c.Actions[1].Method = "" },
		"capability 不認得":            func(c *operator.MachineActionCatalogue) { c.Actions[0].Capability = "root" },
		"同一個動作列兩次":                  func(c *operator.MachineActionCatalogue) { c.Actions[1].Kind = operator.MachineActionConnect },
		"順序被打亂": func(c *operator.MachineActionCatalogue) {
			c.Actions[0], c.Actions[1] = c.Actions[1], c.Actions[0]
		},
		"服役中卻給恢復管理": func(c *operator.MachineActionCatalogue) {
			c.Actions[1].Kind = operator.MachineActionRestore
		},
		"退役了卻給退役":     func(c *operator.MachineActionCatalogue) { c.State = store.MachineLifecycleRetired },
		"state 不認得":   func(c *operator.MachineActionCatalogue) { c.State = "paused" },
		"schema 版本不對": func(c *operator.MachineActionCatalogue) { c.SchemaVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			body := machineActionCatalogue(now)
			edit(&body)
			if _, err := complianceClientServer(t, body).MachineActions(t.Context(), "m1"); err == nil {
				t.Fatalf("%s 沒有被拒絕", name)
			}
		})
	}
}
