package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

type revocationTestLink struct {
	mu      sync.Mutex
	reasons []string
}

func (*revocationTestLink) Send(agentrelay.Downstream) error { return nil }

func (l *revocationTestLink) Close(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reasons = append(l.reasons, reason)
}

func (l *revocationTestLink) closeReasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reasons...)
}

type revocationTestSink struct {
	mu     sync.Mutex
	frames []agentrelay.Upstream
}

func (s *revocationTestSink) Deliver(frame agentrelay.Upstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
	return nil
}

func (s *revocationTestSink) delivered() []agentrelay.Upstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentrelay.Upstream(nil), s.frames...)
}

type revocationFixture struct {
	hub        *hub
	machineID  string
	agentToken string
}

func newRevocationFixture(t *testing.T) revocationFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, enrollmentToken, err := st.CreateEnrollTokenFor("revocation-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, agentToken, err := st.RedeemEnrollToken(enrollmentToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion,
		Hostname:      "revocation-machine",
		UnixUser:      "tester",
		OS:            "linux",
		Arch:          "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry
		SET assigned_user_id=?,assigned_user_login=? WHERE machine_id=?`,
		"user-1", "operator@example.com", machineID); err != nil {
		t.Fatal(err)
	}
	return revocationFixture{
		hub:        &hub{store: st, agentLinks: agentlink.New()},
		machineID:  machineID,
		agentToken: agentToken,
	}
}

func (f revocationFixture) openLedgerSession(t *testing.T, sessionID string) store.AgentSessionResult {
	t.Helper()
	result, err := f.hub.store.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID:                sessionID,
		MachineID:                f.machineID,
		OperatorTailnetUserID:    "user-1",
		OperatorTailnetUserLogin: "operator@example.com",
		IdempotencyKey:           "open-" + sessionID,
		RequestDigest:            "sha256:open-" + sessionID,
		Audit:                    store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func discardRevocationLog(string, ...any) {}

func TestStartupSweepClosesOpenRowsAndReportsCount(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "startup-a")
	f.openLedgerSession(t, "startup-b")
	var output strings.Builder

	if err := sweepOpenAgentSessionsOnStartup(f.hub.store, func(format string, args ...any) {
		fmt.Fprintf(&output, format, args...)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "2") {
		t.Fatalf("startup report %q does not contain closed count", output.String())
	}
	open, err := f.hub.store.ListOpenAgentSessionsForMachine(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("startup left open sessions: %+v", open)
	}
}

type failingStartupSweep struct {
	calls int
	err   error
}

func (s *failingStartupSweep) CloseAllOpenAgentSessions(reason string) (int, error) {
	s.calls++
	if reason != store.AgentSessionCloseReasonHubRestart {
		return 0, fmt.Errorf("reason = %q", reason)
	}
	return 0, s.err
}

func TestFailingStartupSweepPreventsServing(t *testing.T) {
	wantErr := errors.New("database unavailable")
	st := &failingStartupSweep{err: wantErr}
	served := false
	if err := sweepOpenAgentSessionsOnStartup(st, discardRevocationLog); err == nil {
		served = true
	} else if !errors.Is(err, wantErr) {
		t.Fatalf("startup error = %v, want %v", err, wantErr)
	}
	if served || st.calls != 1 {
		t.Fatalf("served=%t sweep calls=%d, want false and 1", served, st.calls)
	}
}

func TestRevocationClosesMachineRetiredDirectlyThroughStore(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "retired-session")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	sink := &revocationTestSink{}
	if err := f.hub.agentLinks.OpenSession("retired-session", f.machineID, sink); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.store.RetireMachine(f.machineID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, time.Now(), discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if got := f.hub.agentLinks.Snapshot(); len(got.Machines) != 0 || len(got.Sessions) != 0 {
		t.Fatalf("live state after retirement = %+v", got)
	}
	if got := link.closeReasons(); !reflect.DeepEqual(got, []string{agentlink.ReasonSessionRevoked.String()}) {
		t.Fatalf("link close reasons = %q", got)
	}
	assertRevokedFrame(t, sink, "retired-session")
}

func TestRevocationClosesClosedRowWhileMachineStaysActive(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "closed-row")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	sink := &revocationTestSink{}
	if err := f.hub.agentLinks.OpenSession("closed-row", f.machineID, sink); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.store.CloseAgentSessionsByID(
		[]string{"closed-row"}, store.AgentSessionCloseReasonAgentConnectionEnded); err != nil {
		t.Fatal(err)
	}

	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, time.Now(), discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	got := f.hub.agentLinks.Snapshot()
	if !reflect.DeepEqual(got.Machines, []string{f.machineID}) || len(got.Sessions) != 0 {
		t.Fatalf("live state after row close = %+v", got)
	}
	if len(link.closeReasons()) != 0 {
		t.Fatalf("active machine link was closed: %q", link.closeReasons())
	}
	assertRevokedFrame(t, sink, "closed-row")
}

func TestRevocationClosesRegisteredSessionWithoutRow(t *testing.T) {
	f := newRevocationFixture(t)
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	sink := &revocationTestSink{}
	if err := f.hub.agentLinks.OpenSession("missing-row", f.machineID, sink); err != nil {
		t.Fatal(err)
	}

	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, time.Now(), discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if len(f.hub.agentLinks.Snapshot().Sessions) != 0 {
		t.Fatal("registered session without a row remained live")
	}
	if len(link.closeReasons()) != 0 {
		t.Fatalf("active machine link was closed: %q", link.closeReasons())
	}
	assertRevokedFrame(t, sink, "missing-row")
}

func TestRevocationLeavesAuthorizedSessionsAloneAcrossSeveralTicks(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "authorized")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	sink := &revocationTestSink{}
	if err := f.hub.agentLinks.OpenSession("authorized", f.machineID, sink); err != nil {
		t.Fatal(err)
	}

	for range 4 {
		if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, time.Now(), discardRevocationLog); err != nil {
			t.Fatal(err)
		}
	}
	got := f.hub.agentLinks.Snapshot()
	if got.Sessions["authorized"] != f.machineID || !reflect.DeepEqual(got.Machines, []string{f.machineID}) {
		t.Fatalf("authorized live state changed: %+v", got)
	}
	if len(link.closeReasons()) != 0 || len(sink.delivered()) != 0 {
		t.Fatalf("authorized connection was disturbed: link=%q frames=%+v", link.closeReasons(), sink.delivered())
	}
}

type countingSessionStateStore struct {
	retiredCalls      int
	unauthorizedCalls int
	closeCalls        int
	unattachedCalls   int
	unattachedIDs     []string
	unattachedAt      time.Time
	unattachedReason  string
	retiredErrors     []error
	retired           []string
}

func (s *countingSessionStateStore) RetiredMachineIDs([]string) ([]string, error) {
	s.retiredCalls++
	if len(s.retiredErrors) != 0 {
		err := s.retiredErrors[0]
		s.retiredErrors = s.retiredErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return append([]string(nil), s.retired...), nil
}

func (s *countingSessionStateStore) UnauthorizedAgentSessionIDs([]string) ([]string, error) {
	s.unauthorizedCalls++
	return []string{}, nil
}

func (s *countingSessionStateStore) CloseAgentSessionsByID([]string, string) (int, error) {
	s.closeCalls++
	return 0, nil
}

func (s *countingSessionStateStore) CloseUnattachedAgentSessions(attached []string, openedAtOrBefore time.Time, reason string) (int, error) {
	s.unattachedCalls++
	s.unattachedIDs = append([]string(nil), attached...)
	s.unattachedAt = openedAtOrBefore
	s.unattachedReason = reason
	return 0, nil
}

func TestIdleRevocationSweepsUnattachedSessionsOnly(t *testing.T) {
	st := &countingSessionStateStore{}
	tick := time.Date(2026, 9, 25, 4, 5, 6, 123456789, time.FixedZone("UTC+8", 8*3600))
	if err := reconcileAgentSessionState(st, agentlink.New(), tick, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if st.unattachedCalls != 1 || st.retiredCalls != 0 || st.unauthorizedCalls != 0 || st.closeCalls != 0 {
		t.Fatalf("idle calls: sweep=%d retired=%d unauthorized=%d close=%d",
			st.unattachedCalls, st.retiredCalls, st.unauthorizedCalls, st.closeCalls)
	}
	if len(st.unattachedIDs) != 0 || st.unattachedReason != store.AgentSessionCloseReasonNeverAttached ||
		!st.unattachedAt.Equal(tick.Add(-agentSessionAttachGrace)) {
		t.Fatalf("idle sweep attached=%v reason=%q cutoff=%s", st.unattachedIDs, st.unattachedReason, st.unattachedAt)
	}
}

type retrySessionStateStore struct {
	calls              int
	retiredCalled      chan int
	unauthorizedCalled chan struct{}
}

func (s *retrySessionStateStore) RetiredMachineIDs([]string) ([]string, error) {
	s.calls++
	s.retiredCalled <- s.calls
	if s.calls == 1 {
		return nil, errors.New("temporary read failure")
	}
	return []string{"machine"}, nil
}

func (s *retrySessionStateStore) UnauthorizedAgentSessionIDs([]string) ([]string, error) {
	s.unauthorizedCalled <- struct{}{}
	return []string{}, nil
}

func (*retrySessionStateStore) CloseAgentSessionsByID([]string, string) (int, error) {
	return 0, nil
}

func (*retrySessionStateStore) CloseUnattachedAgentSessions([]string, time.Time, string) (int, error) {
	return 0, nil
}

func sendRevocationTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("revocation loop stopped reading ticks")
	}
}

func receiveRevocationEvent[T any](t *testing.T, events <-chan T, missingBehavior string) T {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal(missingBehavior)
		var zero T
		return zero
	}
}

func TestStoreErrorInOneTickDoesNotPreventNextTickRevocation(t *testing.T) {
	links := agentlink.New()
	link := &revocationTestLink{}
	if _, err := links.Attach("machine", link); err != nil {
		t.Fatal(err)
	}
	st := &retrySessionStateStore{
		retiredCalled:      make(chan int),
		unauthorizedCalled: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentSessionRevocationLoop(ctx, ticks, st, links, discardRevocationLog)
	}()

	sendRevocationTick(t, ticks)
	if call := receiveRevocationEvent(t, st.retiredCalled, "revocation loop did not check retired machines after the first tick"); call != 1 {
		t.Fatalf("first tick call = %d", call)
	}
	if len(links.Snapshot().Machines) != 1 {
		t.Fatal("failed tick changed the live link")
	}
	sendRevocationTick(t, ticks)
	if call := receiveRevocationEvent(t, st.retiredCalled, "revocation loop did not check retired machines after the next tick"); call != 2 {
		t.Fatalf("second tick call = %d", call)
	}
	receiveRevocationEvent(t, st.unauthorizedCalled, "revocation loop did not check unauthorized sessions after the successful tick")
	if len(links.Snapshot().Machines) != 0 {
		t.Fatalf("next tick did not revoke: snapshot=%+v", links.Snapshot())
	}
	cancel()
	receiveRevocationEvent(t, done, "revocation loop did not stop after cancellation")
}

func TestRetiredBetweenAuthenticationAndAttachKeepsNoLiveLink(t *testing.T) {
	f := newRevocationFixture(t)

	// This is an ordering test for the race window, not a scheduler-driven race:
	// authentication finishes first, retirement commits second, and only then
	// the already-authenticated request performs its attach and post-attach check.
	machineID, err := f.hub.store.AuthenticateAgent(f.agentToken)
	if err != nil || machineID != f.machineID {
		t.Fatalf("AuthenticateAgent() = %q, %v", machineID, err)
	}
	if err := f.hub.store.RetireMachine(f.machineID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	link := &revocationTestLink{}
	attached, err := f.hub.attachAgentTerminalLink(machineID, link)
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		t.Fatal("retired machine remained attached")
	}
	if got := f.hub.agentLinks.Snapshot(); len(got.Machines) != 0 || len(got.Sessions) != 0 {
		t.Fatalf("retired machine live state = %+v", got)
	}
	if got := link.closeReasons(); !reflect.DeepEqual(got, []string{agentlink.ReasonSessionRevoked.String()}) {
		t.Fatalf("link close reasons = %q", got)
	}
}

func TestDatabaseErrorDuringAttachLeavesNoLiveLink(t *testing.T) {
	f := newRevocationFixture(t)
	if err := f.hub.store.Close(); err != nil {
		t.Fatal(err)
	}

	link := &revocationTestLink{}
	attached, err := f.hub.attachAgentTerminalLink(f.machineID, link)
	if err == nil {
		t.Fatal("attach returned no database error")
	}
	if attached {
		t.Fatal("attach succeeded after database error")
	}
	if got := f.hub.agentLinks.Snapshot(); len(got.Machines) != 0 {
		t.Fatalf("machine link remained live after database error: %+v", got)
	}
}

func TestStaleDetachPersistsNothingAndLeavesReplacementRowOpen(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "stale-session")
	f.openLedgerSession(t, "replacement-session")
	stale := &revocationTestLink{}
	attached, err := f.hub.attachAgentTerminalLink(f.machineID, stale)
	if err != nil || !attached {
		t.Fatalf("attach stale = %t, %v", attached, err)
	}
	if err := f.hub.agentLinks.OpenSession("stale-session", f.machineID, &revocationTestSink{}); err != nil {
		t.Fatal(err)
	}

	replacement := &revocationTestLink{}
	attached, err = f.hub.attachAgentTerminalLink(f.machineID, replacement)
	if err != nil || !attached {
		t.Fatalf("attach replacement = %t, %v", attached, err)
	}
	if err := f.hub.agentLinks.OpenSession("replacement-session", f.machineID, &revocationTestSink{}); err != nil {
		t.Fatal(err)
	}

	// This models the deferred teardown of the old handler after Attach has
	// already installed its replacement. Detach must return no IDs here.
	f.hub.detachAgentTerminalLink(f.machineID, stale)
	open, err := f.hub.store.ListOpenAgentSessionsForMachine(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].SessionID != "replacement-session" {
		t.Fatalf("open sessions after stale detach = %+v", open)
	}
	if got := f.hub.agentLinks.Snapshot(); got.Sessions["replacement-session"] != f.machineID {
		t.Fatalf("replacement live state changed: %+v", got)
	}
}

func TestCurrentDetachPersistsReturnedSessionIDs(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "detached-session")
	link := &revocationTestLink{}
	attached, err := f.hub.attachAgentTerminalLink(f.machineID, link)
	if err != nil || !attached {
		t.Fatalf("attach = %t, %v", attached, err)
	}
	if err := f.hub.agentLinks.OpenSession("detached-session", f.machineID, &revocationTestSink{}); err != nil {
		t.Fatal(err)
	}

	f.hub.detachAgentTerminalLink(f.machineID, link)
	open, err := f.hub.store.ListOpenAgentSessionsForMachine(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("detached session rows still open: %+v", open)
	}
}

func TestUnattachedSessionOlderThanGraceIsClosed(t *testing.T) {
	f := newRevocationFixture(t)
	opened := f.openLedgerSession(t, "never-attached")
	var output strings.Builder
	tick := opened.OpenedAt.Add(agentSessionAttachGrace + time.Nanosecond)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, tick, func(format string, args ...any) {
		fmt.Fprintf(&output, format, args...)
	}); err != nil {
		t.Fatal(err)
	}
	closedAt, reason, open := agentSessionClose(t, f.hub.store, "never-attached")
	if open || reason != store.AgentSessionCloseReasonNeverAttached || closedAt == "" {
		t.Fatalf("unattached session open=%t closed_at=%q reason=%q", open, closedAt, reason)
	}
	logged := output.String()
	if !strings.Contains(logged, "1") || strings.Contains(logged, "never-attached") || strings.Contains(logged, f.machineID) {
		t.Fatalf("sweep log = %q", logged)
	}
}

func TestRoutedSessionOlderThanGraceStaysOpen(t *testing.T) {
	f := newRevocationFixture(t)
	opened := f.openLedgerSession(t, "routed-old")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.agentLinks.OpenSession("routed-old", f.machineID, &revocationTestSink{}); err != nil {
		t.Fatal(err)
	}
	tick := opened.OpenedAt.Add(agentSessionAttachGrace + time.Hour + time.Nanosecond)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, tick, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if _, _, open := agentSessionClose(t, f.hub.store, "routed-old"); !open {
		t.Fatal("routed session was closed")
	}
	if f.hub.agentLinks.Snapshot().Sessions["routed-old"] != f.machineID {
		t.Fatalf("routed session left the registry: %+v", f.hub.agentLinks.Snapshot())
	}
}

func TestUnattachedSessionYoungerThanGraceClosesOnALaterTick(t *testing.T) {
	f := newRevocationFixture(t)
	opened := f.openLedgerSession(t, "still-young")
	young := opened.OpenedAt.Add(agentSessionAttachGrace - time.Second + time.Nanosecond)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, young, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if _, _, open := agentSessionClose(t, f.hub.store, "still-young"); !open {
		t.Fatal("session younger than the grace was closed")
	}
	later := opened.OpenedAt.Add(agentSessionAttachGrace + time.Nanosecond)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, later, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	_, reason, open := agentSessionClose(t, f.hub.store, "still-young")
	if open || reason != store.AgentSessionCloseReasonNeverAttached {
		t.Fatalf("aged session open=%t reason=%q", open, reason)
	}
}

// The other sweep tests place their ticks relative to the grace, so they pass
// with any grace. These times are literal: a page that attaches within twenty
// seconds keeps its row, and an abandoned open frees its slot within a minute.
func TestUnattachedSessionGraceCoversASlowPageAndEndsWithinAMinute(t *testing.T) {
	f := newRevocationFixture(t)
	opened := f.openLedgerSession(t, "slow-page")
	slowPage := opened.OpenedAt.Add(20 * time.Second)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, slowPage, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	if _, _, open := agentSessionClose(t, f.hub.store, "slow-page"); !open {
		t.Fatal("session was closed twenty seconds after it opened")
	}
	abandoned := opened.OpenedAt.Add(time.Minute)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, abandoned, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	_, reason, open := agentSessionClose(t, f.hub.store, "slow-page")
	if open || reason != store.AgentSessionCloseReasonNeverAttached {
		t.Fatalf("session a minute after it opened: open=%t reason=%q", open, reason)
	}
}

func TestAlreadyClosedSessionKeepsItsFirstClose(t *testing.T) {
	f := newRevocationFixture(t)
	opened := f.openLedgerSession(t, "already-closed")
	if _, err := f.hub.store.CloseAgentSessionsByID(
		[]string{"already-closed"}, store.AgentSessionCloseReasonAgentConnectionEnded); err != nil {
		t.Fatal(err)
	}
	closedAt, reason, open := agentSessionClose(t, f.hub.store, "already-closed")
	if open {
		t.Fatal("session was not closed before the sweep")
	}
	tick := opened.OpenedAt.Add(agentSessionAttachGrace + time.Hour)
	if err := reconcileAgentSessionState(f.hub.store, f.hub.agentLinks, tick, discardRevocationLog); err != nil {
		t.Fatal(err)
	}
	againAt, againReason, againOpen := agentSessionClose(t, f.hub.store, "already-closed")
	if againOpen || againAt != closedAt || againReason != reason {
		t.Fatalf("first close %q %q became open=%t %q %q", closedAt, reason, againOpen, againAt, againReason)
	}
}

type sweepFailingStore struct {
	*store.Store
	err error
}

func (s *sweepFailingStore) CloseUnattachedAgentSessions([]string, time.Time, string) (int, error) {
	return 0, s.err
}

func TestSweepFailureStillRevokesRoutedSession(t *testing.T) {
	f := newRevocationFixture(t)
	f.openLedgerSession(t, "revoked-route")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	sink := &revocationTestSink{}
	if err := f.hub.agentLinks.OpenSession("revoked-route", f.machineID, sink); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.store.CloseAgentSessionsByID(
		[]string{"revoked-route"}, store.AgentSessionCloseReasonAgentConnectionEnded); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("sweep failed")
	st := &sweepFailingStore{Store: f.hub.store, err: wantErr}
	err := reconcileAgentSessionState(st, f.hub.agentLinks, time.Now(), discardRevocationLog)
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcile error = %v, want sweep failure", err)
	}
	if _, ok := f.hub.agentLinks.Snapshot().Sessions["revoked-route"]; ok {
		t.Fatal("revoked route survived a failed sweep")
	}
	if len(link.closeReasons()) != 0 {
		t.Fatalf("active machine link was closed: %q", link.closeReasons())
	}
	assertRevokedFrame(t, sink, "revoked-route")
}

type unauthorizedFailingStore struct {
	*store.Store
	err   error
	calls int
	ids   []string
}

func (s *unauthorizedFailingStore) UnauthorizedAgentSessionIDs(sessionIDs []string) ([]string, error) {
	s.calls++
	s.ids = append([]string(nil), sessionIDs...)
	return nil, s.err
}

func TestRevocationFailureStillSweepsUnattachedSessions(t *testing.T) {
	f := newRevocationFixture(t)
	orphan := f.openLedgerSession(t, "orphan")
	f.openLedgerSession(t, "live")
	link := &revocationTestLink{}
	if _, err := f.hub.agentLinks.Attach(f.machineID, link); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.agentLinks.OpenSession("live", f.machineID, &revocationTestSink{}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("authorization read failed")
	st := &unauthorizedFailingStore{Store: f.hub.store, err: wantErr}
	tick := orphan.OpenedAt.Add(agentSessionAttachGrace + time.Hour + time.Nanosecond)
	err := reconcileAgentSessionState(st, f.hub.agentLinks, tick, discardRevocationLog)
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcile error = %v, want authorization failure", err)
	}
	if st.calls != 1 || len(st.ids) != 1 || st.ids[0] != "live" {
		t.Fatalf("authorization query calls=%d ids=%v", st.calls, st.ids)
	}
	_, reason, open := agentSessionClose(t, f.hub.store, "orphan")
	if open || reason != store.AgentSessionCloseReasonNeverAttached {
		t.Fatalf("orphan open=%t reason=%q", open, reason)
	}
	if _, _, liveOpen := agentSessionClose(t, f.hub.store, "live"); !liveOpen {
		t.Fatal("routed session was swept")
	}
	if f.hub.agentLinks.Snapshot().Sessions["live"] != f.machineID {
		t.Fatalf("live route changed: %+v", f.hub.agentLinks.Snapshot())
	}
}

func agentSessionClose(t *testing.T, st *store.Store, sessionID string) (closedAt, reason string, open bool) {
	t.Helper()
	var closed sql.NullString
	var closeReason sql.NullString
	if err := st.DB().QueryRow(
		`SELECT closed_at, close_reason FROM agent_sessions WHERE session_id=?`, sessionID,
	).Scan(&closed, &closeReason); err != nil {
		t.Fatal(err)
	}
	return closed.String, closeReason.String, !closed.Valid
}

func assertRevokedFrame(t *testing.T, sink *revocationTestSink, sessionID string) {
	t.Helper()
	got := sink.delivered()
	want := []agentrelay.Upstream{{
		Type: agentrelay.UpstreamError, Session: sessionID, Reason: agentlink.ReasonSessionRevoked.String(),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session close frames = %+v, want %+v", got, want)
	}
}
