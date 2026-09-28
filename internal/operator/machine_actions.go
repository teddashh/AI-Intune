package operator

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 裝置動作目錄。
//
// 一台機器身上能做的每一件事，本來就各自有 preview 與寫入路徑；缺的是一個
// 「這台**現在**能做什麼、做了會怎樣、什麼卡住了」的單一答案。目錄就是那個答案。
//
// ⚠ 目錄**不自己重算**寫入路徑的固定 blocker。lifecycle 讀
// PreviewOperatorMachineLifecycle、診斷讀 PreviewOperatorDiagnosticNoop、撤票讀
// PreviewOperatorEnrollTokenRevocation、連線讀 MachineConnect。重新命名與名冊備註只有填入新值後
// 才有格式、重複或 stale 可判，所以目錄只提供入口，讓各自的專用 preview 判該次輸入。
// 同一個固定 blocker 寫兩套算法，遲早會出現「目錄說可以、按下去被拒絕」。
//
// 目錄也不重讀已經在畫面上的東西：detail、connect、lifecycle 由呼叫端傳進來，
// 所以目錄講的那台機器，就是同一頁其他面板講的那台機器。

const MachineActionsSchemaVersion = 4

// MachineActionKind is one named thing an operator can do to one machine.
type MachineActionKind string

const (
	MachineActionConnect           MachineActionKind = "connect"
	MachineActionDiagnosticNoop    MachineActionKind = "diagnostic_noop"
	MachineActionRename            MachineActionKind = "rename"
	MachineActionNotes             MachineActionKind = "notes"
	MachineActionChannel           MachineActionKind = "channel"
	MachineActionRevokeEnrollToken MachineActionKind = "revoke_enrollment_token"
	MachineActionRetire            MachineActionKind = "retire"
	MachineActionRestore           MachineActionKind = "restore"
)

// MachineActionCapability is the permission an operator must hold to run one
// action. An action whose capability the caller does not hold is left out of
// the catalogue entirely; it is not shown as something they may not touch.
type MachineActionCapability string

const (
	MachineActionCapabilityOperate MachineActionCapability = "operate"
	MachineActionCapabilityAdmin   MachineActionCapability = "admin"
)

// MachineActionSurface 表示操作員從哪一個介面執行動作。
type MachineActionSurface string

const (
	MachineActionSurfaceOperatorAPI MachineActionSurface = "operator-api"
	MachineActionSurfaceWeb         MachineActionSurface = "web"
)

// MachineActionConfirm is what the operator must supply at the write step.
type MachineActionConfirm string

const (
	MachineActionConfirmNone        MachineActionConfirm = "none"
	MachineActionConfirmDisplayName MachineActionConfirm = "display_name"
)

// MachineActionBlocker is the typed reason one action is unavailable.
type MachineActionBlocker string

const (
	MachineActionBlockerRetired           MachineActionBlocker = "machine_retired"
	MachineActionBlockerNeverReported     MachineActionBlocker = "machine_never_reported"
	MachineActionBlockerExecutionUnknown  MachineActionBlocker = "agent_execution_unknown"
	MachineActionBlockerExecutionDisabled MachineActionBlocker = "agent_execution_disabled"
	MachineActionBlockerActiveJobs        MachineActionBlocker = "nonterminal_jobs"
	MachineActionBlockerNoPendingToken    MachineActionBlocker = "enrollment_token_not_pending"
	MachineActionBlockerNoConnectAddress  MachineActionBlocker = "connect_address_unavailable"
)

var machineActionOrder = map[MachineActionKind]int{
	MachineActionConnect:           0,
	MachineActionDiagnosticNoop:    1,
	MachineActionRename:            2,
	MachineActionNotes:             3,
	MachineActionChannel:           4,
	MachineActionRevokeEnrollToken: 5,
	MachineActionRetire:            6,
	MachineActionRestore:           7,
}

type machineActionShape struct {
	label       string
	effect      string
	capability  MachineActionCapability
	surface     MachineActionSurface
	method      string
	path        string
	previewPath string
	confirm     MachineActionConfirm
}

// machineActionShapes 是每個動作固定的一半：名稱、後果、權限與執行介面。
// 只有可用性會隨機器改變。
var machineActionShapes = map[MachineActionKind]machineActionShape{
	MachineActionConnect: {
		label:      "連到這台的 BAT",
		effect:     "取得這台的 bat-server 位址並留下一筆動作紀錄；機器本身不變。",
		capability: MachineActionCapabilityOperate,
		surface:    MachineActionSurfaceWeb,
		confirm:    MachineActionConfirmNone,
	},
	MachineActionDiagnosticNoop: {
		label:      "開一張診斷工作單",
		effect:     "開一張不改任何設定的工作單，走完整條派工與回報路徑。",
		capability: MachineActionCapabilityOperate,
		surface:    MachineActionSurfaceOperatorAPI,
		method:     "POST", path: "/v1/operator/machines/{id}/diagnostic-noop-jobs",
		previewPath: "/v1/operator/machines/{id}/diagnostic-noop-preview",
		confirm:     MachineActionConfirmDisplayName,
	},
	MachineActionRename: {
		label:       "重新命名",
		effect:      "改 Hub 名冊名稱與名稱型 expectations；machine ID、agent 與機器上的 hostname 都不變。",
		capability:  MachineActionCapabilityAdmin,
		surface:     MachineActionSurfaceOperatorAPI,
		method:      "PUT",
		path:        "/v1/operator/machines/{id}/display-name",
		previewPath: "/v1/operator/machines/{id}/display-name-preview",
		confirm:     MachineActionConfirmDisplayName,
	},
	MachineActionNotes: {
		label:       "編輯名冊備註",
		effect:      "更新 Hub 名冊中的人工備註；機器設定與 agent 不變。",
		capability:  MachineActionCapabilityAdmin,
		surface:     MachineActionSurfaceOperatorAPI,
		method:      "PUT",
		path:        "/v1/operator/machines/{id}/notes",
		previewPath: "/v1/operator/machines/{id}/notes-preview",
		confirm:     MachineActionConfirmDisplayName,
	},
	MachineActionChannel: {
		label:      "指派部署通道",
		effect:     "改變這台會領到哪一批部署；已在跑的工作單不受影響。",
		capability: MachineActionCapabilityAdmin,
		surface:    MachineActionSurfaceOperatorAPI,
		method:     "PUT", path: "/v1/operator/machines/{id}/channel",
		confirm: MachineActionConfirmDisplayName,
	},
	MachineActionRevokeEnrollToken: {
		label:      "撤銷註冊票",
		effect:     "讓還沒兌換的註冊票失效；名冊列與已發出的 agent 憑證都不動。",
		capability: MachineActionCapabilityAdmin,
		surface:    MachineActionSurfaceOperatorAPI,
		method:     "POST", path: "/v1/operator/machines/{id}/enrollment-token/revocations",
		previewPath: "/v1/operator/machines/{id}/enrollment-token/revocation-preview",
		confirm:     MachineActionConfirmNone,
	},
	MachineActionRetire: {
		label:      "退役",
		effect:     "退出分母、拒絕這台的 agent 驗證；名冊、歷史、通道與憑證都保留。",
		capability: MachineActionCapabilityAdmin,
		surface:    MachineActionSurfaceOperatorAPI,
		method:     "PUT", path: "/v1/operator/machines/{id}/lifecycle",
		previewPath: "/v1/operator/machines/{id}/lifecycle-preview",
		confirm:     MachineActionConfirmDisplayName,
	},
	MachineActionRestore: {
		label:      "恢復管理",
		effect:     "放回分母、恢復這台的 agent 驗證；退役期間的歷史都還在。",
		capability: MachineActionCapabilityAdmin,
		surface:    MachineActionSurfaceOperatorAPI,
		method:     "PUT", path: "/v1/operator/machines/{id}/lifecycle",
		previewPath: "/v1/operator/machines/{id}/lifecycle-preview",
		confirm:     MachineActionConfirmDisplayName,
	},
}

// MachineActionKinds returns every kind in catalogue order.
func MachineActionKinds() []MachineActionKind {
	out := make([]MachineActionKind, 0, len(machineActionOrder))
	for kind := range machineActionOrder {
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool { return machineActionOrder[out[i]] < machineActionOrder[out[j]] })
	return out
}

// MachineActionLabel is the operator-facing name of one action.
func MachineActionLabel(kind MachineActionKind) string {
	return machineActionShapes[kind].label
}

// MachineActionEffect is the sentence that says what running it does.
func MachineActionEffect(kind MachineActionKind) string {
	return machineActionShapes[kind].effect
}

// MachineActionBlockerNextStep is the one thing that makes a blocked action
// available again. A blocker with nothing to do returns "".
func MachineActionBlockerNextStep(blocker MachineActionBlocker) string {
	switch blocker {
	case MachineActionBlockerRetired:
		return "先恢復管理，這台才會回到分母裡。"
	case MachineActionBlockerNeverReported:
		return "等它第一次報到。"
	case MachineActionBlockerExecutionUnknown:
		return "等下一次報到帶回 agent 的工作單開關。"
	case MachineActionBlockerExecutionDisabled:
		return "在那台上開啟 agent 的工作單執行。"
	case MachineActionBlockerActiveJobs:
		return "等未結束的工作單收尾，或到工作單頁處理掉。"
	case MachineActionBlockerNoConnectAddress:
		return "等 agent 回報 bat-server 的監聽位址。"
	default:
		return ""
	}
}

// MachineAction is one action's fixed identity plus its state on one machine.
type MachineAction struct {
	Kind        MachineActionKind       `json:"kind"`
	Label       string                  `json:"label"`
	Effect      string                  `json:"effect"`
	Capability  MachineActionCapability `json:"capability"`
	Surface     MachineActionSurface    `json:"surface"`
	Confirm     MachineActionConfirm    `json:"confirm"`
	Method      string                  `json:"method,omitempty"`
	Path        string                  `json:"path,omitempty"`
	PreviewPath string                  `json:"preview_path,omitempty"`
	Available   bool                    `json:"available"`
	// Blocker, Situation and NextStep are present exactly when Available is
	// false. Situation says what is true now; NextStep says what to do, and is
	// empty when the blocker is not something the operator has to clear.
	Blocker   MachineActionBlocker `json:"blocker,omitempty"`
	Situation string               `json:"situation,omitempty"`
	NextStep  string               `json:"next_step,omitempty"`
}

// MachineActionCatalogue is every action this operator can run on one machine.
type MachineActionCatalogue struct {
	SchemaVersion int                   `json:"schema_version"`
	EvaluatedAt   time.Time             `json:"evaluated_at"`
	MachineID     string                `json:"machine_id"`
	DisplayName   string                `json:"display_name"`
	State         MachineLifecycleState `json:"state"`
	Available     int                   `json:"available"`
	Blocked       int                   `json:"blocked"`
	Actions       []MachineAction       `json:"actions"`
}

// MachineActionGrant is what the caller may actually do.
type MachineActionGrant struct {
	Operate bool `json:"operate"`
	Admin   bool `json:"admin"`
}

func (g MachineActionGrant) holds(capability MachineActionCapability) bool {
	switch capability {
	case MachineActionCapabilityOperate:
		return g.Operate
	case MachineActionCapabilityAdmin:
		return g.Admin
	default:
		return false
	}
}

// MachineActionsRequest carries the reads the catalogue projects. Detail,
// Connect and Lifecycle are required and must be about the same machine: the
// catalogue never re-reads them, so what it says is exactly what the caller
// already put on the rest of the page.
type MachineActionsRequest struct {
	Detail    MachineDetailResult
	Connect   MachineConnectResult
	Lifecycle MachineLifecycleReadResult
	Granted   MachineActionGrant
}

// MachineActions answers "what can I do to this machine right now".
// MachineActionsReachable 說這個 grant 碰不碰得到任何一個動作。
//
// 單機的子選單只有在「動作」那一節真的存在時才指得過去，而那一節存不存在就是這個
// 問題的答案。讓選單自己再判一次 capability 會漂，而漂掉的樣子是一個指向不存在段落
// 的連結——操作員按下去看到的是一頁沒有他要的東西的畫面。
func MachineActionsReachable(grant MachineActionGrant) bool {
	for _, kind := range MachineActionKinds() {
		if grant.holds(machineActionShapes[kind].capability) {
			return true
		}
	}
	return false
}

func (s *Service) MachineActions(req MachineActionsRequest) (MachineActionCatalogue, error) {
	machineID := req.Detail.Item.MachineID
	if machineID == "" || req.Detail.EvaluatedAt.IsZero() ||
		req.Connect.MachineID != machineID || req.Lifecycle.MachineID != machineID {
		return MachineActionCatalogue{}, errors.New("operator: machine action source identity is inconsistent")
	}
	catalogue := MachineActionCatalogue{
		SchemaVersion: MachineActionsSchemaVersion,
		EvaluatedAt:   req.Detail.EvaluatedAt.UTC(),
		MachineID:     machineID,
		DisplayName:   req.Lifecycle.DisplayName,
		State:         req.Lifecycle.State,
		Actions:       make([]MachineAction, 0, len(machineActionShapes)),
	}
	verdicts, err := s.machineActionVerdicts(req)
	if err != nil {
		return MachineActionCatalogue{}, err
	}
	for _, kind := range MachineActionKinds() {
		verdict, present := verdicts[kind]
		shape := machineActionShapes[kind]
		if !present || !req.Granted.holds(shape.capability) {
			continue
		}
		action := MachineAction{
			Kind: kind, Label: shape.label, Effect: shape.effect,
			Capability: shape.capability, Surface: shape.surface, Confirm: shape.confirm,
			Method: shape.method, Path: shape.path, PreviewPath: shape.previewPath,
			Available: verdict.blocker == "",
		}
		if !action.Available {
			action.Blocker = verdict.blocker
			action.Situation = verdict.situation
			action.NextStep = MachineActionBlockerNextStep(verdict.blocker)
			catalogue.Blocked++
		} else {
			catalogue.Available++
		}
		catalogue.Actions = append(catalogue.Actions, action)
	}
	return catalogue, nil
}

// machineActionVerdict is one action's availability, taken from the preview
// that guards its write path. An empty blocker means that preview said yes.
type machineActionVerdict struct {
	blocker   MachineActionBlocker
	situation string
}

func (s *Service) machineActionVerdicts(req MachineActionsRequest) (map[MachineActionKind]machineActionVerdict, error) {
	machineID := req.Detail.Item.MachineID
	retired := req.Lifecycle.State == MachineLifecycleStateRetired
	out := map[MachineActionKind]machineActionVerdict{}

	// 連線：MachineConnect 自己已經判過有沒有可用的座標。
	out[MachineActionConnect] = machineActionVerdict{}
	if !req.Connect.Available {
		out[MachineActionConnect] = machineActionVerdict{
			blocker:   MachineActionBlockerNoConnectAddress,
			situation: "這台還沒給出可以連過去的 bat-server 位址。",
		}
	}

	// 診斷工作單：blocker 只跟機器狀態有關，跟操作員之後填的 timeout 無關，
	// 所以目錄用預設 timeout 問到的答案，對任何合法 timeout 都成立。
	diagnostic, err := s.store.PreviewOperatorDiagnosticNoop(machineID,
		store.OperatorDiagnosticNoopDefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("operator: machine action diagnostic preview: %w", err)
	}
	out[MachineActionDiagnosticNoop] = machineActionVerdict{}
	for _, blocker := range diagnostic.Blockers {
		verdict, err := diagnosticNoopActionVerdict(blocker, diagnostic.ActiveJobCount)
		if err != nil {
			return nil, err
		}
		out[MachineActionDiagnosticNoop] = verdict
		break
	}

	// 重新命名不依 lifecycle 或 agent 狀態：它只改名冊標籤。新名稱本身的格式、
	// 重複與 stale 判定要等操作員填值後，由 rename preview 負責。
	out[MachineActionRename] = machineActionVerdict{}

	// 名冊備註同樣不依 lifecycle 或 agent 狀態；內容與 stale 判定由專用 preview 負責。
	out[MachineActionNotes] = machineActionVerdict{}

	// 通道：寫入路徑對已退役的機器一律拒絕。
	out[MachineActionChannel] = machineActionVerdict{}
	if retired {
		out[MachineActionChannel] = machineActionVerdict{
			blocker: MachineActionBlockerRetired, situation: machineRetiredSituation(req.Lifecycle),
		}
	}

	// 撤票：沒有待兌換的票時，preview 自己就拒絕。
	out[MachineActionRevokeEnrollToken] = machineActionVerdict{}
	if _, err := s.store.PreviewOperatorEnrollTokenRevocation(machineID); err != nil {
		if !errors.Is(err, store.ErrEnrollTokenNotPending) {
			return nil, fmt.Errorf("operator: machine action enrollment token preview: %w", err)
		}
		out[MachineActionRevokeEnrollToken] = machineActionVerdict{
			blocker:   MachineActionBlockerNoPendingToken,
			situation: "這台沒有還沒兌換的註冊票。",
		}
	}

	// 生命週期：兩個方向只有一個是動作，另一個是空操作。目錄只列會改變什麼的
	// 那一個，剩下的交給同一份 preview 判。
	desired := MachineLifecycleStateRetired
	kind := MachineActionRetire
	if retired {
		desired, kind = MachineLifecycleStateActive, MachineActionRestore
	}
	revision := req.Lifecycle.LifecycleRevision
	lifecycle, err := s.store.PreviewOperatorMachineLifecycle(store.OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: desired, ExpectedRevision: &revision,
	})
	if err != nil {
		return nil, fmt.Errorf("operator: machine action lifecycle preview: %w", err)
	}
	out[kind] = machineActionVerdict{}
	for _, blocker := range lifecycle.Blockers {
		switch blocker {
		case store.MachineLifecycleBlockerNonterminalJobs:
			out[kind] = machineActionVerdict{
				blocker:   MachineActionBlockerActiveJobs,
				situation: activeJobsSituation(lifecycle.ActiveJobCount),
			}
		default:
			return nil, fmt.Errorf("operator: unrecognized machine lifecycle blocker %q", string(blocker))
		}
		break
	}
	return out, nil
}

func diagnosticNoopActionVerdict(blocker store.OperatorDiagnosticNoopBlocker, activeJobs int64) (machineActionVerdict, error) {
	switch blocker {
	case store.OperatorDiagnosticNoopBlockerRetired:
		return machineActionVerdict{
			blocker: MachineActionBlockerRetired, situation: "這台已經退役。",
		}, nil
	case store.OperatorDiagnosticNoopBlockerNeverReported:
		return machineActionVerdict{
			blocker: MachineActionBlockerNeverReported, situation: "這台從來沒有報到過。",
		}, nil
	case store.OperatorDiagnosticNoopBlockerExecutionUnknown:
		return machineActionVerdict{
			blocker:   MachineActionBlockerExecutionUnknown,
			situation: "Hub 還不知道這台的 agent 有沒有開工作單執行。",
		}, nil
	case store.OperatorDiagnosticNoopBlockerExecutionDisabled:
		return machineActionVerdict{
			blocker:   MachineActionBlockerExecutionDisabled,
			situation: "這台的 agent 關掉了工作單執行。",
		}, nil
	case store.OperatorDiagnosticNoopBlockerNonterminalJob:
		return machineActionVerdict{
			blocker: MachineActionBlockerActiveJobs, situation: activeJobsSituation(activeJobs),
		}, nil
	default:
		return machineActionVerdict{},
			fmt.Errorf("operator: unrecognized diagnostic noop blocker %q", string(blocker))
	}
}

func machineRetiredSituation(lifecycle MachineLifecycleReadResult) string {
	if lifecycle.RetiredAt == nil {
		return "這台已經退役。"
	}
	return "這台在 " + lifecycle.RetiredAt.Local().Format("2006-01-02 15:04") + " 退役。"
}

func activeJobsSituation(count int64) string {
	return fmt.Sprintf("這台還有 %d 張未結束的工作單。", count)
}
