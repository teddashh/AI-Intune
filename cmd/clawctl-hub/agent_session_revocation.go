package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	// agentSessionRevocationInterval bounds how long a retired machine can keep a
	// live terminal. Five seconds is short enough for revocation. An idle fleet
	// costs exactly one bounded statement per tick.
	agentSessionRevocationInterval = 5 * time.Second

	// agentSessionAttachGrace is how long an open ledger row may wait to become
	// a route before this loop closes it. Thirty seconds is longer than the
	// tick and the one-second timestamp, so a slow page load is not closed and
	// then revoked on the next tick, and short enough that four abandoned opens
	// do not lock a machine out for long.
	agentSessionAttachGrace = 30 * time.Second
)

type agentSessionStateStore interface {
	RetiredMachineIDs([]string) ([]string, error)
	UnauthorizedAgentSessionIDs([]string) ([]string, error)
	CloseAgentSessionsByID([]string, string) (int, error)
	CloseUnattachedAgentSessions([]string, time.Time, string) (int, error)
}

type agentSessionStartupStore interface {
	CloseAllOpenAgentSessions(string) (int, error)
}

func sweepOpenAgentSessionsOnStartup(st agentSessionStartupStore, logf func(string, ...any)) error {
	closed, err := st.CloseAllOpenAgentSessions(store.AgentSessionCloseReasonHubRestart)
	if err != nil {
		return err
	}
	logf("Hub 已重新啟動：%d 個先前仍開著的終端工作階段已關閉", closed)
	return nil
}

func (h *hub) agentSessionRevocationLoop(ctx context.Context) {
	ticker := time.NewTicker(agentSessionRevocationInterval)
	defer ticker.Stop()
	runAgentSessionRevocationLoop(ctx, ticker.C, h.store, h.agentLinks, log.Printf)
}

func runAgentSessionRevocationLoop(ctx context.Context, ticks <-chan time.Time, st agentSessionStateStore,
	links *agentlink.Registry, logf func(string, ...any),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticks:
			// The next tick is not read until this check finishes, so checks can
			// never overlap even when a database operation runs long.
			if err := reconcileAgentSessionState(st, links, t, logf); err != nil {
				logf("無法確認終端連線目前是否仍獲授權：本次檢查未完成；將於 %s 後重試：%v",
					agentSessionRevocationInterval, err)
			}
		}
	}
}

func reconcileAgentSessionState(st agentSessionStateStore, links *agentlink.Registry, tick time.Time,
	logf func(string, ...any),
) error {
	snapshot := links.Snapshot()
	attached := make([]string, 0, len(snapshot.Sessions))
	for sessionID := range snapshot.Sessions {
		attached = append(attached, sessionID)
	}
	sort.Strings(attached)

	// One snapshot per tick. A socket can attach in the moment this sweep
	// closes its row. The next tick's authorization check removes that route,
	// because UnauthorizedAgentSessionIDs returns routed sessions whose rows
	// are not open.
	var sweepErr error
	closed, err := st.CloseUnattachedAgentSessions(
		attached, tick.Add(-agentSessionAttachGrace), store.AgentSessionCloseReasonNeverAttached)
	if err != nil {
		sweepErr = fmt.Errorf("close unattached terminal sessions: %w", err)
	} else if closed > 0 {
		logf("終端開啟後沒有連上：%d 個工作階段已關閉", closed)
	}
	if len(snapshot.Machines) == 0 && len(snapshot.Sessions) == 0 {
		return sweepErr
	}

	retiredMachineIDs, err := st.RetiredMachineIDs(snapshot.Machines)
	if err != nil {
		return errors.Join(sweepErr, fmt.Errorf("check connected machine status: %w", err))
	}
	remaining := make(map[string]struct{}, len(snapshot.Sessions))
	for sessionID := range snapshot.Sessions {
		remaining[sessionID] = struct{}{}
	}
	for _, machineID := range retiredMachineIDs {
		closedSessionIDs := links.CloseMachine(machineID, agentlink.ReasonSessionRevoked)
		for _, sessionID := range closedSessionIDs {
			delete(remaining, sessionID)
		}
		if _, err := st.CloseAgentSessionsByID(closedSessionIDs, store.AgentSessionCloseReasonMachineRetired); err != nil {
			logf("已退役機器的終端連線已關閉，但工作階段狀態未能寫入資料庫；請修復資料庫：%v", err)
		}
	}

	remainingSessionIDs := make([]string, 0, len(remaining))
	for sessionID := range remaining {
		remainingSessionIDs = append(remainingSessionIDs, sessionID)
	}
	sort.Strings(remainingSessionIDs)
	unauthorizedSessionIDs, err := st.UnauthorizedAgentSessionIDs(remainingSessionIDs)
	if err != nil {
		return errors.Join(sweepErr, fmt.Errorf("check terminal session authorization: %w", err))
	}
	links.CloseSessions(unauthorizedSessionIDs, agentlink.ReasonSessionRevoked)
	return sweepErr
}
