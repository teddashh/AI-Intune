package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// MachineActions reads "what can I do to this machine right now".
//
// 用戶端自己再驗一次目錄的自洽性。目錄的價值全在於它跟寫入路徑講的是同一件事，
// 所以一份自相矛盾的目錄（說可用卻又附著理由、退役中卻還給退役）不是拿來顯示的
// 東西，是拿來拒收的。
func (c *Client) MachineActions(ctx context.Context, machineID string) (operator.MachineActionCatalogue, error) {
	var out operator.MachineActionCatalogue
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, "/actions", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: machine actions returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "machine actions", &out); err != nil {
		return out, err
	}
	if err := validateMachineActionCatalogue(out, machineID); err != nil {
		return out, err
	}
	return out, nil
}

var machineActionBlockers = map[operator.MachineActionBlocker]bool{
	operator.MachineActionBlockerRetired:              true,
	operator.MachineActionBlockerNeverReported:        true,
	operator.MachineActionBlockerExecutionUnknown:     true,
	operator.MachineActionBlockerExecutionDisabled:    true,
	operator.MachineActionBlockerActiveJobs:           true,
	operator.MachineActionBlockerNoPendingToken:       true,
	operator.MachineActionBlockerNoConnectAddress:     true,
	operator.MachineActionBlockerTerminalNotLinked:    true,
	operator.MachineActionBlockerTerminalLimitReached: true,
}

func validateMachineActionCatalogue(result operator.MachineActionCatalogue, machineID string) error {
	if result.SchemaVersion != operator.MachineActionsSchemaVersion || result.MachineID != machineID ||
		strings.TrimSpace(result.DisplayName) == "" || result.EvaluatedAt.IsZero() ||
		result.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: machine action catalogue identity 不一致")
	}
	if result.State != store.MachineLifecycleActive && result.State != store.MachineLifecycleRetired {
		return fmt.Errorf("operator client: machine action catalogue state %q 不是 active 或 retired", result.State)
	}
	if err := validateMachineClientText("machine actions display_name", result.DisplayName, 256); err != nil {
		return err
	}
	if result.Available < 0 || result.Blocked < 0 || result.Available+result.Blocked != len(result.Actions) {
		return errors.New("operator client: machine action catalogue 的可用與被擋張數跟清單對不起來")
	}
	order := map[operator.MachineActionKind]int{}
	for i, kind := range operator.MachineActionKinds() {
		order[kind] = i
	}
	previous := -1
	seen := map[operator.MachineActionKind]bool{}
	available := 0
	for _, action := range result.Actions {
		position, known := order[action.Kind]
		if !known || seen[action.Kind] {
			return fmt.Errorf("operator client: machine action %q 不認得或重複", action.Kind)
		}
		if position <= previous {
			return errors.New("operator client: machine action catalogue 沒有照固定順序排列")
		}
		previous, seen[action.Kind] = position, true
		if err := validateMachineAction(action); err != nil {
			return err
		}
		if action.Available {
			available++
		}
	}
	if available != result.Available {
		return errors.New("operator client: machine action catalogue 說的可用張數跟逐項對不起來")
	}
	// 生命週期的兩個方向只有一個會改變什麼。目錄給了跟現狀相反的那一個，就是在
	// 講一件按下去不會發生的事。
	if result.State == store.MachineLifecycleRetired && seen[operator.MachineActionRetire] ||
		result.State == store.MachineLifecycleActive && seen[operator.MachineActionRestore] {
		return errors.New("operator client: machine action catalogue 提供的生命週期方向與現狀不符")
	}
	return nil
}

func validateMachineAction(action operator.MachineAction) error {
	if strings.TrimSpace(action.Label) == "" || strings.TrimSpace(action.Effect) == "" {
		return fmt.Errorf("operator client: machine action %q 少了名稱或後果", action.Kind)
	}
	switch action.Surface {
	case operator.MachineActionSurfaceOperatorAPI:
		if action.Method == "" || !strings.HasPrefix(action.Path, "/v1/operator/machines/{id}") ||
			action.PreviewPath != "" && !strings.HasPrefix(action.PreviewPath, "/v1/operator/machines/{id}") {
			return fmt.Errorf("operator client: machine action %q 的 operator API 路徑不完整", action.Kind)
		}
	case operator.MachineActionSurfaceWeb:
		if action.Method != "" || action.Path != "" || action.PreviewPath != "" {
			return fmt.Errorf("operator client: machine action %q 的 web surface 帶了 operator API 路徑", action.Kind)
		}
	default:
		return fmt.Errorf("operator client: machine action %q 的 surface 是 %q", action.Kind, action.Surface)
	}
	switch action.Capability {
	case operator.MachineActionCapabilityOperate, operator.MachineActionCapabilityAdmin:
	default:
		return fmt.Errorf("operator client: machine action %q 的 capability 是 %q", action.Kind, action.Capability)
	}
	switch action.Confirm {
	case operator.MachineActionConfirmNone, operator.MachineActionConfirmDisplayName:
	default:
		return fmt.Errorf("operator client: machine action %q 的確認方式是 %q", action.Kind, action.Confirm)
	}
	if action.Available {
		if action.Blocker != "" || action.Situation != "" || action.NextStep != "" {
			return fmt.Errorf("operator client: machine action %q 說可用卻附了擋住的理由", action.Kind)
		}
		return nil
	}
	if !machineActionBlockers[action.Blocker] {
		return fmt.Errorf("operator client: machine action %q 的 blocker %q 不認得", action.Kind, action.Blocker)
	}
	if strings.TrimSpace(action.Situation) == "" {
		return fmt.Errorf("operator client: machine action %q 被擋住卻沒說現在是什麼狀況", action.Kind)
	}
	if action.NextStep != operator.MachineActionBlockerNextStep(action.Blocker) {
		return fmt.Errorf("operator client: machine action %q 的下一步跟 blocker %q 對不起來",
			action.Kind, action.Blocker)
	}
	if err := validateMachineClientText("machine action situation", action.Situation, 512); err != nil {
		return err
	}
	return nil
}
