package web

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// lifecyclePageView is a deliberately small registry projection for the
// Devices > Lifecycle submenu. Mutation facts are not guessed here: every row
// enters the canonical preview service before an operator can confirm it.
type lifecyclePageView struct {
	Rows    []lifecyclePageRow
	Active  int
	Retired int
}

type lifecyclePageRow struct {
	MachineID     string
	DisplayName   string
	State         string
	Revision      int64
	Channel       string
	Enrolled      bool
	RetiredAt     *time.Time
	DesiredState  string
	DesiredLabel  string
	PreviewButton string
}

func (s *Server) lifecycle(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	result, err := s.operator.ListMachines(now)
	if err != nil {
		s.fail(w, "讀取機器生命週期失敗", err)
		return
	}
	overview, ok := result.StoreOverview()
	if !ok {
		s.fail(w, "讀取機器生命週期失敗", errors.New("operator machine overview bridge unavailable"))
		return
	}
	view := &lifecyclePageView{
		Rows:   make([]lifecyclePageRow, 0, len(overview.Machines)+len(overview.Retired)),
		Active: len(overview.Machines), Retired: len(overview.Retired),
	}
	for _, item := range overview.Machines {
		view.Rows = append(view.Rows, lifecycleWebRow(item.Machine, false))
	}
	for _, machine := range overview.Retired {
		view.Rows = append(view.Rows, lifecycleWebRow(machine, true))
	}
	sort.SliceStable(view.Rows, func(i, j int) bool {
		if view.Rows[i].State != view.Rows[j].State {
			return view.Rows[i].State < view.Rows[j].State
		}
		if view.Rows[i].DisplayName != view.Rows[j].DisplayName {
			return view.Rows[i].DisplayName < view.Rows[j].DisplayName
		}
		return view.Rows[i].MachineID < view.Rows[j].MachineID
	})
	s.render(w, r, "lifecycle.html", page{
		Title: "裝置生命週期", Nav: "machines-lifecycle",
		Now: now.Local().Format("2006-01-02 15:04"), Lifecycle: view,
	})
}

func lifecycleWebRow(machine store.Machine, retired bool) lifecyclePageRow {
	row := lifecyclePageRow{
		MachineID: machine.MachineID, DisplayName: machine.DisplayName,
		State: string(store.MachineLifecycleActive), Revision: machine.LifecycleRevision,
		Channel:  machine.Channel,
		Enrolled: machine.EnrolledAt != nil, RetiredAt: machine.RetiredAt,
		DesiredState: string(store.MachineLifecycleRetired), DesiredLabel: "retired",
		PreviewButton: "預覽退役",
	}
	if retired {
		row.State = string(store.MachineLifecycleRetired)
		row.DesiredState = string(store.MachineLifecycleActive)
		row.DesiredLabel = "active"
		row.PreviewButton = "預覽恢復管理"
	}
	return row
}
