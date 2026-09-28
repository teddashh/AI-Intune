package web

import (
	"github.com/teddashh/AI-Intune/internal/operator"
)

// 單機頁的動作目錄。
//
// 這一節是索引，不是第二套判斷：每一列的可用與否都直接來自
// operator.MachineActions，跟 JSON 與 CLI 看到的是同一份。畫面只多做一件事——
// 把可用的動作連到這一頁上真的擺著那張表單的地方。

// machineActionsView 是單機頁「動作」那一節。
type machineActionsView struct {
	Catalogue operator.MachineActionCatalogue
	Rows      []machineActionRow
}

// machineActionRow 是目錄的一列加上這一頁上那張表單的位置。
type machineActionRow struct {
	operator.MachineAction
	Anchor string
}

// machineActionAnchors 把每一個動作對到這一頁上放著它表單的那個段落。動作目錄
// 一旦列出某件事可以做，操作員就必須在同一頁按得到它。
var machineActionAnchors = map[operator.MachineActionKind]string{
	operator.MachineActionConnect:           "action-connect",
	operator.MachineActionDiagnosticNoop:    "action-diagnostic",
	operator.MachineActionRename:            "action-rename",
	operator.MachineActionNotes:             "action-notes",
	operator.MachineActionChannel:           "action-channel",
	operator.MachineActionRevokeEnrollToken: "action-enrollment",
	operator.MachineActionRetire:            "action-lifecycle",
	operator.MachineActionRestore:           "action-lifecycle",
}

func machineActionsViewFrom(catalogue operator.MachineActionCatalogue) *machineActionsView {
	if len(catalogue.Actions) == 0 {
		return nil
	}
	view := &machineActionsView{Catalogue: catalogue, Rows: make([]machineActionRow, 0, len(catalogue.Actions))}
	for _, action := range catalogue.Actions {
		view.Rows = append(view.Rows, machineActionRow{
			MachineAction: action, Anchor: machineActionAnchors[action.Kind],
		})
	}
	return view
}
