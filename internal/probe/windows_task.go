package probe

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const windowsAgentTaskUnit = "clawctl-agent.service"

// Task Scheduler TASK_STATE values.
const (
	windowsTaskStateUnknown  = 0
	windowsTaskStateDisabled = 1
	windowsTaskStateQueued   = 2
	windowsTaskStateReady    = 3
	windowsTaskStateRunning  = 4
)

type windowsTaskView struct {
	Found   bool
	State   int
	LastRun time.Time
}

func windowsObservedUnit(name string, view windowsTaskView, queryErr error) model.Unit {
	u := model.Unit{Name: name}
	if name != windowsAgentTaskUnit {
		u.Measured = true
		return u
	}
	if queryErr != nil {
		u.Reason = queryErr.Error()
		return u
	}
	u.Measured = true
	if !view.Found {
		return u
	}
	u.Present = true
	active, sub, ok := windowsTaskActiveState(view.State)
	if !ok {
		u.Reason = "unrecognized scheduled task state"
		return u
	}
	u.ActiveState = active
	u.SubState = sub
	if !view.LastRun.IsZero() {
		t := view.LastRun.UTC()
		u.ActiveEnterTimestamp = &t
	}
	return u
}

func windowsTaskActiveState(state int) (active, sub string, ok bool) {
	switch state {
	case windowsTaskStateRunning:
		return "active", "running", true
	case windowsTaskStateReady:
		return "inactive", "ready", true
	case windowsTaskStateDisabled:
		return "inactive", "disabled", true
	case windowsTaskStateQueued:
		return "activating", "queued", true
	default:
		return "", "", false
	}
}

func parseWindowsTaskReport(stdout string) (windowsTaskView, error) {
	var view windowsTaskView
	seenFound := false
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return windowsTaskView{}, errors.New("cannot parse scheduled task response")
		}
		switch key {
		case "found":
			switch value {
			case "1":
				view.Found = true
			case "0":
				view.Found = false
			default:
				return windowsTaskView{}, errors.New("cannot parse scheduled task response")
			}
			seenFound = true
		case "state":
			n, err := strconv.Atoi(value)
			if err != nil {
				return windowsTaskView{}, errors.New("cannot parse scheduled task response")
			}
			view.State = n
		case "lastrun":
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return windowsTaskView{}, errors.New("cannot parse scheduled task response")
			}
			if t.Year() >= 2000 {
				view.LastRun = t.UTC()
			}
		default:
			return windowsTaskView{}, errors.New("cannot parse scheduled task response")
		}
	}
	if !seenFound {
		return windowsTaskView{}, errors.New("cannot parse scheduled task response")
	}
	return view, nil
}
