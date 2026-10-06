// Package agentlink keeps the Hub's live agent links and terminal session
// routes. It performs no network or persistence work.
package agentlink

import (
	"errors"
	"reflect"
	"sort"
	"sync"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
)

// CloseReason is a closed vocabulary for operator-visible session closure
// messages. Its zero value falls back to ReasonAgentDisconnected.
type CloseReason struct {
	text string
}

const (
	reasonAgentDisconnectedText = "Agent connection ended. Open a new session."
	reasonSessionRevokedText    = "Session access was revoked. Open a new session after access is restored."
)

var (
	// ReasonAgentDisconnected tells an operator that the agent-side transport
	// ended and that the old terminal session cannot be resumed.
	ReasonAgentDisconnected = CloseReason{reasonAgentDisconnectedText}
	// ReasonSessionRevoked tells an operator that access changed and what to do
	// after access is restored.
	ReasonSessionRevoked = CloseReason{reasonSessionRevokedText}
)

// String returns the operator-visible text for c. The zero value uses the
// safe default rather than producing an invalid empty relay reason.
func (c CloseReason) String() string {
	return c.withDefault().text
}

func (c CloseReason) withDefault() CloseReason {
	if c.text == "" {
		return CloseReason{reasonAgentDisconnectedText}
	}
	return c
}

var (
	ErrInvalidMachineID = errors.New("agent link: machine ID is required")
	ErrInvalidSessionID = errors.New("agent link: session ID is required")
	ErrInvalidLink      = errors.New("agent link: link must be non-nil and comparable")
	ErrInvalidSink      = errors.New("agent link: sink is required")
	ErrUnknownSession   = errors.New("agent link: session is not registered")
	ErrNoLink           = errors.New("agent link: machine has no attached link")
	ErrForeignSession   = errors.New("agent link: session belongs to another machine")
	ErrDuplicateSession = errors.New("agent link: session is already registered")
	ErrSessionMismatch  = errors.New("agent link: frame session does not match route")
	// ErrLinkBusy means Link.Send cannot accept an input or resize frame now.
	// The link stays open. Open and close are not busy answers.
	ErrLinkBusy = errors.New("agent link: input frame was not accepted")
)

// Link is one live agent connection. Implementations own the socket.
//
// Send returns ErrLinkBusy when it cannot accept an input or resize frame
// now. The link stays open and the caller may retry the same frame later.
// Open and close use the remaining queue and are not busy answers.
type Link interface {
	Send(frame agentrelay.Downstream) error
	Close(reason string)
}

// Sink receives upstream frames for one terminal session.
type Sink interface {
	Deliver(frame agentrelay.Upstream) error
}

type sessionRoute struct {
	machineID string
	sink      Sink
}

// Registry maps machines to live links and terminal sessions to their
// machine-specific upstream sinks. Its zero value is ready for use.
type Registry struct {
	mu       sync.RWMutex
	links    map[string]Link
	sessions map[string]sessionRoute
}

// Snapshot is one consistent view of the live links and session routes held
// by a Registry.
type Snapshot struct {
	Machines []string
	Sessions map[string]string
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		links:    make(map[string]Link),
		sessions: make(map[string]sessionRoute),
	}
}

// HasLink reports whether machineID has a live agent link.
// A nil registry has no links.
func (r *Registry) HasLink(machineID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.links[machineID]
	return ok
}

// Snapshot returns the machines with live links and the session-to-machine
// routes observed under one lock acquisition.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snapshot := Snapshot{
		Machines: make([]string, 0, len(r.links)),
		Sessions: make(map[string]string, len(r.sessions)),
	}
	for machineID := range r.links {
		snapshot.Machines = append(snapshot.Machines, machineID)
	}
	sort.Strings(snapshot.Machines)
	for sessionID, route := range r.sessions {
		snapshot.Sessions[sessionID] = route.machineID
	}
	return snapshot
}

// Attach makes link the sole live link for machineID. Replacing a link also
// ends every session opened through the previous connection.
func (r *Registry) Attach(machineID string, link Link) ([]string, error) {
	if machineID == "" {
		return []string{}, ErrInvalidMachineID
	}
	if invalidLink(link) {
		return []string{}, ErrInvalidLink
	}

	r.mu.Lock()
	r.initLocked()
	previous, replacing := r.links[machineID]
	r.links[machineID] = link
	var closed []closedSession
	if replacing {
		closed = r.removeMachineSessionsLocked(machineID, ReasonAgentDisconnected)
	}
	r.mu.Unlock()

	if replacing {
		previous.Close(ReasonAgentDisconnected.String())
		notifyClosed(closed)
	}
	return closedSessionIDs(closed), nil
}

// Detach removes link only if it is still the current link for machineID.
// Sessions from a stale, already-replaced link are left untouched.
func (r *Registry) Detach(machineID string, link Link) []string {
	if machineID == "" || invalidLink(link) {
		return []string{}
	}

	r.mu.Lock()
	current, ok := r.links[machineID]
	if !ok || current != link {
		r.mu.Unlock()
		return []string{}
	}
	delete(r.links, machineID)
	closed := r.removeMachineSessionsLocked(machineID, ReasonAgentDisconnected)
	r.mu.Unlock()

	notifyClosed(closed)
	return closedSessionIDs(closed)
}

// CloseMachine removes a machine's live link and every session routed to it.
func (r *Registry) CloseMachine(machineID string, reason CloseReason) []string {
	reason = reason.withDefault()
	r.mu.Lock()
	link, attached := r.links[machineID]
	if attached {
		delete(r.links, machineID)
	}
	closed := r.removeMachineSessionsLocked(machineID, reason)
	r.mu.Unlock()

	if attached {
		link.Close(reason.String())
	}
	notifyClosed(closed)
	return closedSessionIDs(closed)
}

// OpenSession registers a new session on an attached machine. Session IDs are
// global within a Registry and cannot be rebound to another sink or machine.
// The authorizing ledger row must be committed before this method is called;
// a missing row is unauthorized and may be closed by the Hub at any time.
func (r *Registry) OpenSession(sessionID, machineID string, sink Sink) error {
	if sessionID == "" {
		return ErrInvalidSessionID
	}
	if machineID == "" {
		return ErrInvalidMachineID
	}
	if isNil(sink) {
		return ErrInvalidSink
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	if _, exists := r.sessions[sessionID]; exists {
		return ErrDuplicateSession
	}
	if _, attached := r.links[machineID]; !attached {
		return ErrNoLink
	}
	r.sessions[sessionID] = sessionRoute{machineID: machineID, sink: sink}
	return nil
}

// Send routes a downstream frame only through the link belonging to the
// registered session's machine. It returns that link's Send error unchanged,
// including ErrLinkBusy.
func (r *Registry) Send(sessionID string, frame agentrelay.Downstream) error {
	r.mu.RLock()
	route, ok := r.sessions[sessionID]
	if !ok {
		r.mu.RUnlock()
		return ErrUnknownSession
	}
	if frame.Session != sessionID {
		r.mu.RUnlock()
		return ErrSessionMismatch
	}
	link, attached := r.links[route.machineID]
	r.mu.RUnlock()
	if !attached {
		return ErrNoLink
	}
	return link.Send(frame)
}

// Deliver routes an upstream frame only when its session belongs to
// machineID. A machine cannot deliver into another machine's session.
func (r *Registry) Deliver(machineID string, frame agentrelay.Upstream) error {
	r.mu.RLock()
	route, ok := r.sessions[frame.Session]
	if !ok {
		r.mu.RUnlock()
		return ErrUnknownSession
	}
	if route.machineID != machineID {
		r.mu.RUnlock()
		return ErrForeignSession
	}
	sink := route.sink
	r.mu.RUnlock()
	return sink.Deliver(frame)
}

// CloseSessions removes any listed sessions, tells the machine link that owned
// each one to stop it, and then notifies the removed sinks. Unknown or
// already-closed session IDs are ignored.
func (r *Registry) CloseSessions(sessionIDs []string, reason CloseReason) {
	reason = reason.withDefault()
	r.mu.Lock()
	stopped := make([]sessionStop, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		route, ok := r.sessions[sessionID]
		if !ok {
			continue
		}
		delete(r.sessions, sessionID)
		// Resolve the link here, under the lock that deletes the route. After
		// unlock the machine may detach and attach again, and a later lookup
		// would send this session's close into that new connection.
		stopped = append(stopped, sessionStop{
			closed: closedSession{sessionID: sessionID, sink: route.sink, reason: reason},
			link:   r.links[route.machineID],
		})
	}
	r.mu.Unlock()

	closed := make([]closedSession, 0, len(stopped))
	for _, stop := range stopped {
		if stop.link != nil {
			// Send of this close fails when the link is already closed, or
			// when the frame queue is full and the link closes itself. The
			// agent then loses the socket and kills the PTY. One failure must
			// not skip the remaining sessions or their sinks.
			_ = stop.link.Send(agentrelay.Downstream{
				Type:    agentrelay.DownstreamClose,
				Session: stop.closed.sessionID,
			})
		}
		closed = append(closed, stop.closed)
	}
	notifyClosed(closed)
}

// CloseAll removes and closes every live link and session. It returns no IDs
// because the next startup sweep handles shutdown persistence. It is safe to
// call repeatedly; previously removed links and sinks are not notified again.
func (r *Registry) CloseAll(reason CloseReason) {
	reason = reason.withDefault()
	r.mu.Lock()
	links := make([]Link, 0, len(r.links))
	for machineID, link := range r.links {
		delete(r.links, machineID)
		links = append(links, link)
	}
	closed := make([]closedSession, 0, len(r.sessions))
	for sessionID, route := range r.sessions {
		delete(r.sessions, sessionID)
		closed = append(closed, closedSession{sessionID: sessionID, sink: route.sink, reason: reason})
	}
	r.mu.Unlock()

	for _, link := range links {
		link.Close(reason.String())
	}
	notifyClosed(closed)
}

type closedSession struct {
	sessionID string
	sink      Sink
	reason    CloseReason
}

// sessionStop is a session CloseSessions removed, paired with the link that
// owned it in that same critical section. link is nil when the machine had
// no live link.
type sessionStop struct {
	closed closedSession
	link   Link
}

func (r *Registry) initLocked() {
	if r.links == nil {
		r.links = make(map[string]Link)
	}
	if r.sessions == nil {
		r.sessions = make(map[string]sessionRoute)
	}
}

func (r *Registry) removeMachineSessionsLocked(machineID string, reason CloseReason) []closedSession {
	reason = reason.withDefault()
	var closed []closedSession
	for sessionID, route := range r.sessions {
		if route.machineID != machineID {
			continue
		}
		delete(r.sessions, sessionID)
		closed = append(closed, closedSession{sessionID: sessionID, sink: route.sink, reason: reason})
	}
	return closed
}

func notifyClosed(closed []closedSession) {
	for _, session := range closed {
		_ = session.sink.Deliver(agentrelay.Upstream{
			Type:    agentrelay.UpstreamError,
			Session: session.sessionID,
			Reason:  session.reason.String(),
		})
	}
}

func closedSessionIDs(closed []closedSession) []string {
	sessionIDs := make([]string, 0, len(closed))
	for _, session := range closed {
		sessionIDs = append(sessionIDs, session.sessionID)
	}
	sort.Strings(sessionIDs)
	return sessionIDs
}

func invalidLink(link Link) bool {
	if isNil(link) {
		return true
	}
	return !reflect.TypeOf(link).Comparable()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
