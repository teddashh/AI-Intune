package agentlink

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
)

type recordingLink struct {
	mu           sync.Mutex
	sent         []agentrelay.Downstream
	closeReasons []string
}

func (l *recordingLink) Send(frame agentrelay.Downstream) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, frame)
	return nil
}

func (l *recordingLink) Close(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeReasons = append(l.closeReasons, reason)
}

func (l *recordingLink) snapshot() ([]agentrelay.Downstream, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agentrelay.Downstream(nil), l.sent...), append([]string(nil), l.closeReasons...)
}

type recordingSink struct {
	mu     sync.Mutex
	frames []agentrelay.Upstream
}

func (s *recordingSink) Deliver(frame agentrelay.Upstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
	return nil
}

func (s *recordingSink) snapshot() []agentrelay.Upstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentrelay.Upstream(nil), s.frames...)
}

func TestHasLinkReadsOnlyTheCurrentAttachment(t *testing.T) {
	var absent *Registry
	if absent.HasLink("machine") {
		t.Fatal("nil registry reports a link")
	}
	var zero Registry
	if zero.HasLink("machine") {
		t.Fatal("zero registry reports a link")
	}

	r := New()
	if r.HasLink("machine") || r.HasLink("") {
		t.Fatal("empty registry reports a link")
	}
	first := &recordingLink{}
	if _, err := r.Attach("machine", first); err != nil {
		t.Fatal(err)
	}
	if !r.HasLink("machine") || r.HasLink("other") {
		t.Fatalf("attached snapshot=%+v", r.Snapshot())
	}
	second := &recordingLink{}
	if _, err := r.Attach("machine", second); err != nil {
		t.Fatal(err)
	}
	if !r.HasLink("machine") {
		t.Fatal("replacement removed the machine link")
	}
	if detached := r.Detach("machine", first); len(detached) != 0 || !r.HasLink("machine") {
		t.Fatalf("stale detach changed the current link: detached=%v has=%v", detached, r.HasLink("machine"))
	}
	if detached := r.Detach("machine", second); len(detached) != 0 || r.HasLink("machine") {
		t.Fatalf("current detach left a link: detached=%v has=%v", detached, r.HasLink("machine"))
	}
}

func TestSecondAttachReplacesAndClosesFirst(t *testing.T) {
	r := New()
	first := &recordingLink{}
	second := &recordingLink{}
	if closed := mustAttach(t, r, "machine", first); len(closed) != 0 {
		t.Fatalf("first Attach() closed sessions = %v, want empty", closed)
	}
	if closed := mustAttach(t, r, "machine", second); len(closed) != 0 {
		t.Fatalf("replacement Attach() closed sessions = %v, want empty", closed)
	}

	_, reasons := first.snapshot()
	if len(reasons) != 1 || reasons[0] != ReasonAgentDisconnected.String() {
		t.Fatalf("first close reasons = %q, want [%q]", reasons, ReasonAgentDisconnected.String())
	}
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)
	frame := downstream("session")
	if err := r.Send("session", frame); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	firstFrames, _ := first.snapshot()
	secondFrames, _ := second.snapshot()
	if len(firstFrames) != 0 || len(secondFrames) != 1 || secondFrames[0].Session != "session" {
		t.Fatalf("sent frames: first=%v second=%v", firstFrames, secondFrames)
	}
}

func TestSnapshotReturnsSortedConsistentCopy(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine-b", &recordingLink{})
	mustAttach(t, r, "machine-a", &recordingLink{})
	mustOpen(t, r, "session-b", "machine-b", &recordingSink{})
	mustOpen(t, r, "session-a", "machine-a", &recordingSink{})

	snapshot := r.Snapshot()
	if want := []string{"machine-a", "machine-b"}; !reflect.DeepEqual(snapshot.Machines, want) {
		t.Fatalf("Snapshot().Machines = %v, want %v", snapshot.Machines, want)
	}
	if want := map[string]string{"session-a": "machine-a", "session-b": "machine-b"}; !reflect.DeepEqual(snapshot.Sessions, want) {
		t.Fatalf("Snapshot().Sessions = %v, want %v", snapshot.Sessions, want)
	}

	snapshot.Machines[0] = "changed"
	delete(snapshot.Sessions, "session-a")
	again := r.Snapshot()
	if want := []string{"machine-a", "machine-b"}; !reflect.DeepEqual(again.Machines, want) {
		t.Fatalf("mutating snapshot changed registry machines: %v", again.Machines)
	}
	if again.Sessions["session-a"] != "machine-a" {
		t.Fatalf("mutating snapshot changed registry sessions: %v", again.Sessions)
	}
}

func TestSnapshotNeverPairsSessionWithAbsentMachineDuringMutation(t *testing.T) {
	r := New()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			link := &recordingLink{}
			_, _ = r.Attach("machine", link)
			_ = r.OpenSession(fmt.Sprintf("session-%d", i), "machine", &recordingSink{})
			r.Detach("machine", link)
		}
	}()

	for {
		snapshot := r.Snapshot()
		machines := make(map[string]bool, len(snapshot.Machines))
		for _, machineID := range snapshot.Machines {
			machines[machineID] = true
		}
		for sessionID, machineID := range snapshot.Sessions {
			if !machines[machineID] {
				t.Fatalf("Snapshot() contained session %q for absent machine %q: %+v", sessionID, machineID, snapshot)
			}
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestAttachReplacingLinkReturnsSortedClosedSessionIDs(t *testing.T) {
	r := New()
	first := &recordingLink{}
	mustAttach(t, r, "machine", first)
	firstSink := &recordingSink{}
	secondSink := &recordingSink{}
	mustOpen(t, r, "z-session", "machine", firstSink)
	mustOpen(t, r, "a-session", "machine", secondSink)
	closed := mustAttach(t, r, "machine", &recordingLink{})
	if want := []string{"a-session", "z-session"}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("replacement Attach() closed sessions = %v, want %v", closed, want)
	}

	assertClosureFrame(t, firstSink, "z-session", ReasonAgentDisconnected)
	assertClosureFrame(t, secondSink, "a-session", ReasonAgentDisconnected)
	for _, sessionID := range []string{"a-session", "z-session"} {
		if err := r.Send(sessionID, downstream(sessionID)); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("Send(%q) error = %v, want ErrUnknownSession", sessionID, err)
		}
	}
}

func TestDetachWithAlreadyReplacedLinkReturnsEmptyAndDoesNotCloseReplacementSessions(t *testing.T) {
	r := New()
	stale := &recordingLink{}
	current := &recordingLink{}
	mustAttach(t, r, "machine", stale)
	mustAttach(t, r, "machine", current)
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)

	if closed := r.Detach("machine", stale); len(closed) != 0 {
		t.Fatalf("stale Detach() closed sessions = %v, want empty", closed)
	}
	if err := r.Send("session", downstream("session")); err != nil {
		t.Fatalf("Send() after stale Detach error = %v", err)
	}
	frames, _ := current.snapshot()
	if len(frames) != 1 {
		t.Fatalf("current link received %d frames, want 1", len(frames))
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("current session was closed: %v", got)
	}
}

func TestSendUnknownSessionFails(t *testing.T) {
	r := New()
	if err := r.Send("missing", downstream("missing")); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Send() error = %v, want ErrUnknownSession", err)
	}
}

type busyLink struct{}

func (busyLink) Send(agentrelay.Downstream) error { return ErrLinkBusy }
func (busyLink) Close(string)                     {}

func TestSendReturnsLinkBusyUnchanged(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", busyLink{})
	mustOpen(t, r, "session", "machine", &recordingSink{})
	if err := r.Send("session", downstream("session")); err != ErrLinkBusy {
		t.Fatalf("Send() error = %v, want ErrLinkBusy unchanged", err)
	}
}

func TestSendWithNoLinkFails(t *testing.T) {
	r := registryWithUnlinkedSession("session", "machine", &recordingSink{})
	if err := r.Send("session", downstream("session")); !errors.Is(err, ErrNoLink) {
		t.Fatalf("Send() error = %v, want ErrNoLink", err)
	}
}

func TestSendNeverFallsBackToAnotherMachine(t *testing.T) {
	other := &recordingLink{}
	r := registryWithUnlinkedSession("session-a", "machine-a", &recordingSink{})
	mustAttach(t, r, "machine-b", other)

	if err := r.Send("session-a", downstream("session-a")); !errors.Is(err, ErrNoLink) {
		t.Fatalf("Send() error = %v, want ErrNoLink", err)
	}
	frames, _ := other.snapshot()
	if len(frames) != 0 {
		t.Fatalf("other machine received frames: %v", frames)
	}
}

func TestDeliverRejectsForeignSession(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine-a", &recordingLink{})
	mustAttach(t, r, "machine-b", &recordingLink{})
	sink := &recordingSink{}
	mustOpen(t, r, "session-a", "machine-a", sink)

	err := r.Deliver("machine-b", upstream("session-a"))
	if !errors.Is(err, ErrForeignSession) {
		t.Fatalf("Deliver() error = %v, want ErrForeignSession", err)
	}
	if frames := sink.snapshot(); len(frames) != 0 {
		t.Fatalf("foreign frame reached sink: %v", frames)
	}
}

func TestDeliverUnknownSessionFails(t *testing.T) {
	r := New()
	if err := r.Deliver("machine", upstream("missing")); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Deliver() error = %v, want ErrUnknownSession", err)
	}
}

func TestDeliverRoutesToOwnSessionSink(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)
	frame := upstream("session")
	if err := r.Deliver("machine", frame); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	frames := sink.snapshot()
	if len(frames) != 1 || frames[0].Type != frame.Type || string(frames[0].Data) != string(frame.Data) {
		t.Fatalf("delivered frames = %#v, want %#v", frames, frame)
	}
}

func TestDetachClosesItsSessions(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	first := &recordingSink{}
	second := &recordingSink{}
	mustOpen(t, r, "first", "machine", first)
	mustOpen(t, r, "second", "machine", second)

	closed := r.Detach("machine", link)
	if want := []string{"first", "second"}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("Detach() closed sessions = %v, want %v", closed, want)
	}
	assertClosureFrame(t, first, "first", ReasonAgentDisconnected)
	assertClosureFrame(t, second, "second", ReasonAgentDisconnected)
	for _, sessionID := range []string{"first", "second"} {
		if err := r.Send(sessionID, downstream(sessionID)); !errors.Is(err, ErrUnknownSession) {
			t.Errorf("Send(%q) error = %v, want ErrUnknownSession", sessionID, err)
		}
	}
}

func TestCloseMachineClosesLinkAndOnlyItsSessionsExactlyOnce(t *testing.T) {
	r := New()
	targetLink := &recordingLink{}
	otherLink := &recordingLink{}
	mustAttach(t, r, "target-machine", targetLink)
	mustAttach(t, r, "other-machine", otherLink)
	targetFirst := &recordingSink{}
	targetSecond := &recordingSink{}
	other := &recordingSink{}
	mustOpen(t, r, "target-z", "target-machine", targetFirst)
	mustOpen(t, r, "target-a", "target-machine", targetSecond)
	mustOpen(t, r, "other", "other-machine", other)

	closed := r.CloseMachine("target-machine", ReasonSessionRevoked)
	if want := []string{"target-a", "target-z"}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("CloseMachine() closed sessions = %v, want %v", closed, want)
	}
	_, targetReasons := targetLink.snapshot()
	if want := []string{ReasonSessionRevoked.String()}; !reflect.DeepEqual(targetReasons, want) {
		t.Fatalf("target link close reasons = %v, want %v", targetReasons, want)
	}
	if _, otherReasons := otherLink.snapshot(); len(otherReasons) != 0 {
		t.Fatalf("other link close reasons = %v, want empty", otherReasons)
	}
	assertClosureFrame(t, targetFirst, "target-z", ReasonSessionRevoked)
	assertClosureFrame(t, targetSecond, "target-a", ReasonSessionRevoked)
	if frames := other.snapshot(); len(frames) != 0 {
		t.Fatalf("other machine sink was notified: %v", frames)
	}
	if err := r.Send("other", downstream("other")); err != nil {
		t.Fatalf("Send(other) error = %v", err)
	}

	if closed := r.CloseMachine("target-machine", ReasonSessionRevoked); len(closed) != 0 {
		t.Fatalf("second CloseMachine() closed sessions = %v, want empty", closed)
	}
	if _, reasons := targetLink.snapshot(); len(reasons) != 1 {
		t.Fatalf("target link was closed %d times, want 1", len(reasons))
	}
	if got := targetFirst.snapshot(); len(got) != 1 {
		t.Fatalf("first target sink notified %d times, want 1", len(got))
	}
	if got := targetSecond.snapshot(); len(got) != 1 {
		t.Fatalf("second target sink notified %d times, want 1", len(got))
	}
}

func TestCloseMachineUnknownReturnsEmptyAndNotifiesNobody(t *testing.T) {
	r := New()
	link := &recordingLink{}
	sink := &recordingSink{}
	mustAttach(t, r, "known-machine", link)
	mustOpen(t, r, "known-session", "known-machine", sink)

	if closed := r.CloseMachine("unknown-machine", ReasonSessionRevoked); closed == nil || len(closed) != 0 {
		t.Fatalf("CloseMachine(unknown) closed sessions = %v, want empty", closed)
	}
	if _, reasons := link.snapshot(); len(reasons) != 0 {
		t.Fatalf("known link close reasons = %v, want empty", reasons)
	}
	if frames := sink.snapshot(); len(frames) != 0 {
		t.Fatalf("known sink was notified: %v", frames)
	}
}

func TestCloseSessionsIsIdempotent(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)
	for range 3 {
		r.CloseSessions([]string{"session"}, ReasonSessionRevoked)
	}
	assertClosureFrame(t, sink, "session", ReasonSessionRevoked)
}

func TestCloseSessionsIgnoresUnknownIDs(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	closed := &recordingSink{}
	live := &recordingSink{}
	mustOpen(t, r, "closed", "machine", closed)
	mustOpen(t, r, "live", "machine", live)

	r.CloseSessions([]string{"missing", "closed", "also-missing"}, ReasonSessionRevoked)
	assertClosureFrame(t, closed, "closed", ReasonSessionRevoked)
	if err := r.Deliver("machine", upstream("live")); err != nil {
		t.Fatalf("Deliver(live) error = %v", err)
	}
	if frames := live.snapshot(); len(frames) != 1 {
		t.Fatalf("live sink received %d frames, want 1", len(frames))
	}
}

func TestCloseSessionsSendsOneDownstreamCloseToTheOwningMachine(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)

	r.CloseSessions([]string{"session", "session"}, ReasonSessionRevoked)

	assertDownstreamClose(t, link, "session")
	frames, _ := link.snapshot()
	if _, err := agentrelay.EncodeDownstream(frames[0]); err != nil {
		t.Fatalf("EncodeDownstream() error = %v", err)
	}
	assertClosureFrame(t, sink, "session", ReasonSessionRevoked)
	if err := r.Send("session", downstream("session")); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Send() error = %v, want ErrUnknownSession", err)
	}
}

func TestCloseSessionsLeavesTheLinkUpAndKeepsOtherSessionsWorking(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	revokedSink := &recordingSink{}
	survivorSink := &recordingSink{}
	mustOpen(t, r, "revoked", "machine", revokedSink)
	mustOpen(t, r, "survivor", "machine", survivorSink)

	r.CloseSessions([]string{"revoked"}, ReasonSessionRevoked)

	survivorFrame := agentrelay.Downstream{
		Type:    agentrelay.DownstreamInput,
		Session: "survivor",
		Data:    []byte("still-open"),
	}
	if err := r.Send("survivor", survivorFrame); err != nil {
		t.Fatalf("Send(survivor) error = %v", err)
	}
	frames, reasons := link.snapshot()
	if len(reasons) != 0 {
		t.Fatalf("link close reasons = %v, want the link left attached", reasons)
	}
	if snapshot := r.Snapshot(); len(snapshot.Machines) != 1 || snapshot.Machines[0] != "machine" {
		t.Fatalf("Snapshot().Machines = %v, want the link still attached", snapshot.Machines)
	}
	want := []agentrelay.Downstream{
		{Type: agentrelay.DownstreamClose, Session: "revoked"},
		survivorFrame,
	}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("machine frames = %#v, want %#v", frames, want)
	}
	assertClosureFrame(t, revokedSink, "revoked", ReasonSessionRevoked)
	if got := survivorSink.snapshot(); len(got) != 0 {
		t.Fatalf("survivor sink was notified: %v", got)
	}
	if err := r.Deliver("machine", upstream("survivor")); err != nil {
		t.Fatalf("Deliver(survivor) error = %v", err)
	}
}

func TestCloseSessionsSendsEachCloseOnItsOwnMachineLink(t *testing.T) {
	r := New()
	linkA := &recordingLink{}
	linkB := &recordingLink{}
	mustAttach(t, r, "machine-a", linkA)
	mustAttach(t, r, "machine-b", linkB)
	mustOpen(t, r, "session-a", "machine-a", &recordingSink{})
	mustOpen(t, r, "session-b", "machine-b", &recordingSink{})

	r.CloseSessions([]string{"session-b", "session-a"}, ReasonSessionRevoked)

	assertDownstreamClose(t, linkA, "session-a")
	assertDownstreamClose(t, linkB, "session-b")
}

func TestCloseSessionsSendsNothingForUnknownOrAlreadyClosedSessions(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)

	r.CloseSessions([]string{"missing", "also-missing"}, ReasonSessionRevoked)
	frames, reasons := link.snapshot()
	if len(frames) != 0 || len(reasons) != 0 {
		t.Fatalf("unknown IDs produced frames=%#v closes=%v, want nothing", frames, reasons)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("unknown IDs notified the sink: %v", got)
	}

	r.CloseSessions([]string{"session"}, ReasonSessionRevoked)
	r.CloseSessions([]string{"session", "missing"}, ReasonSessionRevoked)
	assertDownstreamClose(t, link, "session")
	assertClosureFrame(t, sink, "session", ReasonSessionRevoked)
}

func TestCloseSessionsRemovesTheRouteAndNotifiesWhenTheMachineHasNoLink(t *testing.T) {
	sink := &recordingSink{}
	r := registryWithUnlinkedSession("session", "machine", sink)
	other := &recordingLink{}
	mustAttach(t, r, "other-machine", other)

	r.CloseSessions([]string{"session", "stale"}, ReasonSessionRevoked)

	assertClosureFrame(t, sink, "session", ReasonSessionRevoked)
	if err := r.Send("session", downstream("session")); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Send() error = %v, want ErrUnknownSession", err)
	}
	frames, reasons := other.snapshot()
	if len(frames) != 0 || len(reasons) != 0 {
		t.Fatalf("other machine received frames=%#v closes=%v, want nothing", frames, reasons)
	}
}

func TestCloseSessionsKeepsRemovingRoutesAndNotifyingWhenSendFails(t *testing.T) {
	r := New()
	link := &failingSendLink{}
	mustAttach(t, r, "machine", link)
	first := &recordingSink{}
	second := &recordingSink{}
	mustOpen(t, r, "first", "machine", first)
	mustOpen(t, r, "second", "machine", second)

	r.CloseSessions([]string{"first", "second"}, ReasonSessionRevoked)

	assertClosureFrame(t, first, "first", ReasonSessionRevoked)
	assertClosureFrame(t, second, "second", ReasonSessionRevoked)
	for _, sessionID := range []string{"first", "second"} {
		if err := r.Send(sessionID, downstream(sessionID)); !errors.Is(err, ErrUnknownSession) {
			t.Errorf("Send(%q) error = %v, want ErrUnknownSession", sessionID, err)
		}
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(link.sessions(), want) {
		t.Fatalf("Send attempts = %v, want %v", link.sessions(), want)
	}
}

func TestCloseSessionsNotifiesTheSinkAfterTellingTheAgent(t *testing.T) {
	r := New()
	order := &closeOrder{}
	mustAttach(t, r, "machine", &orderLink{order: order})
	mustOpen(t, r, "session-a", "machine", &orderSink{order: order})
	mustOpen(t, r, "session-b", "machine", &orderSink{order: order})

	r.CloseSessions([]string{"session-a", "session-b"}, ReasonSessionRevoked)

	want := []closeStep{
		{kind: "agent", sessionID: "session-a"},
		{kind: "agent", sessionID: "session-b"},
		{kind: "sink", sessionID: "session-a"},
		{kind: "sink", sessionID: "session-b"},
	}
	if got := order.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("close order = %+v, want every agent close before any sink notice: %+v", got, want)
	}
}

func TestCloseAllClosesEveryLinkAndNotifiesEverySinkExactlyOnce(t *testing.T) {
	r := New()
	firstLink := &recordingLink{}
	secondLink := &recordingLink{}
	mustAttach(t, r, "first-machine", firstLink)
	mustAttach(t, r, "second-machine", secondLink)
	firstSink := &recordingSink{}
	secondSink := &recordingSink{}
	mustOpen(t, r, "first-session", "first-machine", firstSink)
	mustOpen(t, r, "second-session", "second-machine", secondSink)

	r.CloseAll(ReasonAgentDisconnected)

	for name, link := range map[string]*recordingLink{
		"first": firstLink, "second": secondLink,
	} {
		_, reasons := link.snapshot()
		if len(reasons) != 1 || reasons[0] != ReasonAgentDisconnected.String() {
			t.Errorf("%s link close reasons = %q, want [%q]", name, reasons, ReasonAgentDisconnected.String())
		}
	}
	assertClosureFrame(t, firstSink, "first-session", ReasonAgentDisconnected)
	assertClosureFrame(t, secondSink, "second-session", ReasonAgentDisconnected)
	for _, sessionID := range []string{"first-session", "second-session"} {
		if err := r.Send(sessionID, downstream(sessionID)); !errors.Is(err, ErrUnknownSession) {
			t.Errorf("Send(%q) error = %v, want ErrUnknownSession", sessionID, err)
		}
	}

	r.CloseAll(ReasonSessionRevoked)
	for name, link := range map[string]*recordingLink{
		"first": firstLink, "second": secondLink,
	} {
		_, reasons := link.snapshot()
		if len(reasons) != 1 {
			t.Errorf("%s link was closed %d times after second CloseAll, want 1", name, len(reasons))
		}
	}
	if got := firstSink.snapshot(); len(got) != 1 {
		t.Errorf("first sink received %d frames after second CloseAll, want 1", len(got))
	}
	if got := secondSink.snapshot(); len(got) != 1 {
		t.Errorf("second sink received %d frames after second CloseAll, want 1", len(got))
	}
}

func TestCloseSessionsDoesNotHoldLockWhileNotifying(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	slow := newBlockingSink()
	mustOpen(t, r, "session", "machine", slow)

	operationDone := make(chan struct{})
	go func() {
		r.CloseSessions([]string{"session"}, ReasonSessionRevoked)
		close(operationDone)
	}()

	assertAttachCompletesWhileSinkBlocked(t, r, slow, operationDone)
}

func TestCloseSessionsDoesNotHoldLockWhileSending(t *testing.T) {
	r := New()
	slow := &blockingLink{entered: make(chan struct{}), release: make(chan struct{})}
	mustAttach(t, r, "machine", slow)
	mustOpen(t, r, "session", "machine", &recordingSink{})

	operationDone := make(chan struct{})
	go func() {
		r.CloseSessions([]string{"session"}, ReasonSessionRevoked)
		close(operationDone)
	}()
	await(t, slow.entered, "slow link Send was not entered")

	attachDone := make(chan error, 1)
	go func() {
		_, err := r.Attach("other-machine", &recordingLink{})
		attachDone <- err
	}()
	var attachErr error
	attachTimedOut := false
	select {
	case attachErr = <-attachDone:
	case <-time.After(time.Second):
		attachTimedOut = true
	}

	close(slow.release)
	await(t, operationDone, "CloseSessions did not finish")
	if attachTimedOut {
		select {
		case <-attachDone:
		case <-time.After(time.Second):
			t.Fatal("Attach remained blocked after slow link Send was released")
		}
		t.Fatal("Attach blocked while the close frame was being sent")
	}
	if attachErr != nil {
		t.Fatalf("Attach(other-machine) error = %v", attachErr)
	}
}

func TestReplacedLinkNotifyDoesNotHoldLock(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	slow := newBlockingSink()
	mustOpen(t, r, "session", "machine", slow)

	operationDone := make(chan struct{})
	go func() {
		_, _ = r.Attach("machine", &recordingLink{})
		close(operationDone)
	}()

	assertAttachCompletesWhileSinkBlocked(t, r, slow, operationDone)
}

func TestDetachNotifyDoesNotHoldLock(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	slow := newBlockingSink()
	mustOpen(t, r, "session", "machine", slow)

	operationDone := make(chan struct{})
	go func() {
		r.Detach("machine", link)
		close(operationDone)
	}()

	assertAttachCompletesWhileSinkBlocked(t, r, slow, operationDone)
}

func TestZeroCloseReasonFallsBackToSafeDefault(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	sink := &recordingSink{}
	mustOpen(t, r, "session", "machine", sink)

	r.CloseSessions([]string{"session"}, CloseReason{})

	frames := sink.snapshot()
	if len(frames) != 1 {
		t.Fatalf("closure frames = %v, want exactly one", frames)
	}
	if frames[0].Reason == "" {
		t.Fatal("closure reason is empty")
	}
	if frames[0].Reason != ReasonAgentDisconnected.String() {
		t.Fatalf("closure reason = %q, want safe default %q", frames[0].Reason, ReasonAgentDisconnected.String())
	}
	if _, err := agentrelay.EncodeUpstream(frames[0]); err != nil {
		t.Fatalf("EncodeUpstream() error = %v", err)
	}
}

func TestCloseReasonCannotBeBuiltFromArbitraryText(t *testing.T) {
	typeOfReason := reflect.TypeOf(CloseReason{})
	if typeOfReason.NumField() != 1 {
		t.Fatalf("CloseReason field count = %d, want 1", typeOfReason.NumField())
	}
	if typeOfReason.Field(0).PkgPath == "" {
		t.Fatal("CloseReason field is exported; external callers can construct arbitrary reasons")
	}
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingSink() *blockingSink {
	return &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func assertAttachCompletesWhileSinkBlocked(t *testing.T, r *Registry, slow *blockingSink, operationDone <-chan struct{}) {
	t.Helper()
	await(t, slow.entered, "slow sink was not entered")

	attachDone := make(chan error, 1)
	go func() {
		_, err := r.Attach("other-machine", &recordingLink{})
		attachDone <- err
	}()
	var attachErr error
	attachTimedOut := false
	select {
	case attachErr = <-attachDone:
	case <-time.After(time.Second):
		attachTimedOut = true
	}

	close(slow.release)
	await(t, operationDone, "session-closing operation did not finish")
	if attachTimedOut {
		select {
		case <-attachDone:
		case <-time.After(time.Second):
			t.Fatal("Attach remained blocked after slow sink was released")
		}
		t.Fatal("Attach blocked while slow sink was being notified")
	}
	if attachErr != nil {
		t.Fatalf("Attach(other-machine) error = %v", attachErr)
	}
}

func (s *blockingSink) Deliver(agentrelay.Upstream) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func TestSlowSinkDoesNotBlockOtherSessions(t *testing.T) {
	r := New()
	link := &recordingLink{}
	mustAttach(t, r, "machine", link)
	slow := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	mustOpen(t, r, "slow", "machine", slow)
	mustOpen(t, r, "fast", "machine", &recordingSink{})
	deliverDone := make(chan error, 1)
	go func() { deliverDone <- r.Deliver("machine", upstream("slow")) }()
	await(t, slow.entered, "slow sink was not entered")

	sendDone := make(chan error, 1)
	go func() { sendDone <- r.Send("fast", downstream("fast")) }()
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Send(fast) error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send(fast) blocked behind slow sink")
	}
	close(slow.release)
	if err := <-deliverDone; err != nil {
		t.Fatalf("Deliver(slow) error = %v", err)
	}
}

type blockingLink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingLink) Send(agentrelay.Downstream) error {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return nil
}

func (*blockingLink) Close(string) {}

func TestSlowLinkDoesNotBlockRegistry(t *testing.T) {
	r := New()
	slow := &blockingLink{entered: make(chan struct{}), release: make(chan struct{})}
	mustAttach(t, r, "slow-machine", slow)
	mustOpen(t, r, "slow-session", "slow-machine", &recordingSink{})
	slowDone := make(chan error, 1)
	go func() { slowDone <- r.Send("slow-session", downstream("slow-session")) }()
	await(t, slow.entered, "slow link was not entered")

	fastDone := make(chan error, 1)
	go func() {
		fast := &recordingLink{}
		if _, err := r.Attach("fast-machine", fast); err != nil {
			fastDone <- err
			return
		}
		if err := r.OpenSession("fast-session", "fast-machine", &recordingSink{}); err != nil {
			fastDone <- err
			return
		}
		fastDone <- r.Send("fast-session", downstream("fast-session"))
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast registry operations error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fast registry operations blocked behind slow link")
	}
	close(slow.release)
	if err := <-slowDone; err != nil {
		t.Fatalf("Send(slow) error = %v", err)
	}
}

type blockingCloseLink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingCloseLink) Send(agentrelay.Downstream) error { return nil }

func (l *blockingCloseLink) Close(string) {
	l.once.Do(func() { close(l.entered) })
	<-l.release
}

func TestSlowLinkCloseDoesNotBlockRegistry(t *testing.T) {
	r := New()
	slow := &blockingCloseLink{entered: make(chan struct{}), release: make(chan struct{})}
	mustAttach(t, r, "replaced-machine", slow)
	replaceDone := make(chan error, 1)
	go func() {
		_, err := r.Attach("replaced-machine", &recordingLink{})
		replaceDone <- err
	}()
	await(t, slow.entered, "slow link Close was not entered")

	fast := &recordingLink{}
	if _, err := r.Attach("fast-machine", fast); err != nil {
		t.Fatalf("Attach(fast-machine) error = %v", err)
	}
	mustOpen(t, r, "fast-session", "fast-machine", &recordingSink{})
	if err := r.Send("fast-session", downstream("fast-session")); err != nil {
		t.Fatalf("Send(fast-session) error = %v", err)
	}

	close(slow.release)
	if err := <-replaceDone; err != nil {
		t.Fatalf("replacement Attach() error = %v", err)
	}
}

func TestEveryReasonEncodes(t *testing.T) {
	for name, reason := range map[string]CloseReason{
		"agent disconnected": ReasonAgentDisconnected,
		"session revoked":    ReasonSessionRevoked,
		"zero value default": {},
	} {
		t.Run(name, func(t *testing.T) {
			frame := agentrelay.Upstream{Type: agentrelay.UpstreamError, Session: "valid-session", Reason: reason.String()}
			if _, err := agentrelay.EncodeUpstream(frame); err != nil {
				t.Fatalf("EncodeUpstream() error = %v", err)
			}
		})
	}
}

func TestConcurrentAttachDetachSendIsRaceFree(t *testing.T) {
	r := New()
	const workers = 12
	const iterations = 200
	var start sync.WaitGroup
	start.Add(1)
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for worker := range workers {
		go func(worker int) {
			defer workersDone.Done()
			start.Wait()
			for iteration := range iterations {
				link := &recordingLink{}
				if _, err := r.Attach("machine", link); err != nil {
					t.Errorf("Attach() error = %v", err)
					return
				}
				sessionID := fmt.Sprintf("session-%d-%d", worker, iteration)
				if err := r.OpenSession(sessionID, "machine", &recordingSink{}); err == nil {
					_ = r.Send(sessionID, downstream(sessionID))
					_ = r.Deliver("machine", upstream(sessionID))
					r.CloseSessions([]string{sessionID}, ReasonSessionRevoked)
				}
				r.Detach("machine", link)
			}
		}(worker)
	}
	start.Done()
	workersDone.Wait()
}

func TestOpenSessionRejectsDuplicate(t *testing.T) {
	r := New()
	mustAttach(t, r, "machine", &recordingLink{})
	first := &recordingSink{}
	second := &recordingSink{}
	mustOpen(t, r, "session", "machine", first)
	if err := r.OpenSession("session", "machine", second); !errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("second OpenSession() error = %v, want ErrDuplicateSession", err)
	}
	if err := r.Deliver("machine", upstream("session")); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if len(first.snapshot()) != 1 || len(second.snapshot()) != 0 {
		t.Fatalf("duplicate replaced sink: first=%v second=%v", first.snapshot(), second.snapshot())
	}
}

func TestOpenSessionRequiresAttachedMachine(t *testing.T) {
	r := New()
	if err := r.OpenSession("session", "machine", &recordingSink{}); !errors.Is(err, ErrNoLink) {
		t.Fatalf("OpenSession() error = %v, want ErrNoLink", err)
	}
}

func downstream(sessionID string) agentrelay.Downstream {
	return agentrelay.Downstream{Type: agentrelay.DownstreamClose, Session: sessionID}
}

func upstream(sessionID string) agentrelay.Upstream {
	return agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Session: sessionID, Data: []byte("output")}
}

func mustAttach(t *testing.T, r *Registry, machineID string, link Link) []string {
	t.Helper()
	closed, err := r.Attach(machineID, link)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	return closed
}

func mustOpen(t *testing.T, r *Registry, sessionID, machineID string, sink Sink) {
	t.Helper()
	if err := r.OpenSession(sessionID, machineID, sink); err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
}

func assertClosureFrame(t *testing.T, sink *recordingSink, sessionID string, reason CloseReason) {
	t.Helper()
	frames := sink.snapshot()
	if len(frames) != 1 {
		t.Fatalf("closure frames = %v, want exactly one", frames)
	}
	want := agentrelay.Upstream{Type: agentrelay.UpstreamError, Session: sessionID, Reason: reason.String()}
	if frames[0].Type != want.Type || frames[0].Session != want.Session || frames[0].Reason != want.Reason {
		t.Fatalf("closure frame = %#v, want %#v", frames[0], want)
	}
}

func assertDownstreamClose(t *testing.T, link *recordingLink, sessionID string) {
	t.Helper()
	frames, reasons := link.snapshot()
	if len(reasons) != 0 {
		t.Fatalf("link close reasons = %v, want the link left attached", reasons)
	}
	want := []agentrelay.Downstream{{Type: agentrelay.DownstreamClose, Session: sessionID}}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("machine frames = %#v, want %#v", frames, want)
	}
}

type failingSendLink struct {
	mu   sync.Mutex
	sent []string
}

func (l *failingSendLink) Send(frame agentrelay.Downstream) error {
	l.mu.Lock()
	l.sent = append(l.sent, frame.Session)
	l.mu.Unlock()
	return errors.New("agent link send failed")
}

func (*failingSendLink) Close(string) {}

func (l *failingSendLink) sessions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sent...)
}

type closeStep struct {
	kind      string
	sessionID string
}

type closeOrder struct {
	mu    sync.Mutex
	steps []closeStep
}

func (o *closeOrder) record(kind, sessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, closeStep{kind: kind, sessionID: sessionID})
}

func (o *closeOrder) snapshot() []closeStep {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]closeStep(nil), o.steps...)
}

type orderLink struct {
	order *closeOrder
}

func (l *orderLink) Send(frame agentrelay.Downstream) error {
	l.order.record("agent", frame.Session)
	return nil
}

func (*orderLink) Close(string) {}

type orderSink struct {
	order *closeOrder
}

func (s *orderSink) Deliver(frame agentrelay.Upstream) error {
	s.order.record("sink", frame.Session)
	return nil
}

func registryWithUnlinkedSession(sessionID, machineID string, sink Sink) *Registry {
	return &Registry{
		links: make(map[string]Link),
		sessions: map[string]sessionRoute{
			sessionID: {machineID: machineID, sink: sink},
		},
	}
}

func await(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}
