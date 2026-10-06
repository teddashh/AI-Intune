// Package agentpty translates agent relay terminal frames to bat-server PTY
// invocations and translates scoped PTY events back to relay frames.
package agentpty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
)

const ptyIDPrefix = "aiintune-"

const compensatingKillTimeout = 10 * time.Second

// Error reasons are a closed vocabulary suitable for display to an operator.
const (
	ReasonOpenFailed            = "Terminal could not be opened. Try again."
	ReasonTerminalUnavailable   = "Terminal is no longer available. Open a new terminal."
	ReasonSessionAlreadyOpen    = "A terminal is already open for this session. Close it before opening another."
	ReasonInputInvalidUTF8      = "Input could not be sent. Enter valid UTF-8 text and try again."
	ReasonConnectionInterrupted = "Terminal connection was interrupted. Reconnect and try again."
)

var (
	ErrInvalidDownstream  = errors.New("agentpty: invalid downstream frame")
	ErrSessionExists      = errors.New("agentpty: session already has a terminal")
	ErrSessionNotFound    = errors.New("agentpty: session has no active terminal")
	ErrInvalidUTF8Input   = errors.New("agentpty: input is not valid UTF-8")
	ErrOpenFailed         = errors.New("agentpty: terminal open failed")
	ErrCreateIDMismatch   = errors.New("agentpty: created terminal ID does not match requested ID")
	ErrWriteFailed        = errors.New("agentpty: terminal input failed")
	ErrResizeFailed       = errors.New("agentpty: terminal resize failed")
	ErrCloseFailed        = errors.New("agentpty: terminal close failed")
	ErrInvalidEvent       = errors.New("agentpty: invalid terminal event")
	ErrInvalidResponse    = errors.New("agentpty: invalid terminal response")
	ErrInvokerUnavailable = errors.New("agentpty: invoker is unavailable")
)

// Invoker is the profile-context invocation path into bat-server.
type Invoker interface {
	Invoke(ctx context.Context, channel string, params any) (json.RawMessage, error)
}

// Emitter accepts a terminal frame destined for the Hub.
type Emitter func(agentrelay.Upstream)

type sessionPhase uint8

const (
	phaseOpening sessionPhase = iota
	phaseActive
	phaseClosing
	phaseFailed
)

type sessionState struct {
	operations sync.Mutex
	phase      sessionPhase
}

// Recorder keeps the ids of PTYs that may still be running, so shells that
// outlive the agent process can be ended later. bat-server has no call that
// lists PTYs.
type Recorder interface {
	// Add records ptyID before pty:create runs. It must be durable before it
	// returns; on error the Mapper creates no PTY.
	Add(ptyID string) error
	// Remove forgets ptyID once bat-server confirms it is gone. Best effort.
	Remove(ptyID string)
}

// Mapper owns the PTYs associated with one bat-server profile context.
type Mapper struct {
	invoker    Invoker
	contextID  string
	defaultCWD string
	recorder   Recorder
	emit       Emitter

	sessionsMu sync.Mutex
	sessions   map[string]*sessionState
	emitMu     sync.Mutex
}

// New creates a mapper. It performs no I/O and opens no sockets.
func New(invoker Invoker, contextID, defaultCWD string, recorder Recorder, emit Emitter) *Mapper {
	return &Mapper{
		invoker:    invoker,
		contextID:  contextID,
		defaultCWD: defaultCWD,
		recorder:   recorder,
		emit:       emit,
		sessions:   make(map[string]*sessionState),
	}
}

// HandleDownstream translates and invokes one Hub-to-agent terminal frame.
func (m *Mapper) HandleDownstream(ctx context.Context, frame agentrelay.Downstream) error {
	if _, err := agentrelay.EncodeDownstream(frame); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDownstream, err)
	}

	switch frame.Type {
	case agentrelay.DownstreamOpen:
		return m.open(ctx, frame)
	case agentrelay.DownstreamInput:
		return m.withActiveSession(frame.Session, func() error {
			return m.input(ctx, frame)
		})
	case agentrelay.DownstreamResize:
		return m.withActiveSession(frame.Session, func() error {
			return m.resize(ctx, frame)
		})
	case agentrelay.DownstreamClose:
		return m.close(ctx, frame.Session)
	default:
		return ErrInvalidDownstream
	}
}

func (m *Mapper) open(ctx context.Context, frame agentrelay.Downstream) error {
	state := &sessionState{phase: phaseOpening}
	state.operations.Lock()
	m.sessionsMu.Lock()
	if _, exists := m.sessions[frame.Session]; exists {
		m.sessionsMu.Unlock()
		state.operations.Unlock()
		m.emitError(frame.Session, ReasonSessionAlreadyOpen)
		return ErrSessionExists
	}
	m.sessions[frame.Session] = state
	m.sessionsMu.Unlock()

	ptyID := ptyID(frame.Session)
	cwd := frame.CWD
	if cwd == "" {
		cwd = m.defaultCWD
	}

	if m.recorder != nil {
		if m.recorder.Add(ptyID) != nil {
			state.phase = phaseFailed
			m.removeSession(frame.Session, state)
			state.operations.Unlock()
			m.emitError(frame.Session, ReasonOpenFailed)
			return ErrOpenFailed
		}
	}

	// bat-server refuses a create without cwd; an empty one starts the shell
	// in the server's own working directory, so the field is always sent.
	params := struct {
		ID   string `json:"id"`
		CWD  string `json:"cwd"`
		Type string `json:"type"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}{ID: ptyID, CWD: cwd, Type: "shell", Cols: frame.Cols, Rows: frame.Rows}

	result, err := m.invoke(ctx, "pty:create", params)
	if err != nil {
		if m.compensatingKill(ptyID) && m.recorder != nil {
			m.recorder.Remove(ptyID)
		}
		state.phase = phaseFailed
		m.removeSession(frame.Session, state)
		state.operations.Unlock()
		m.emitError(frame.Session, ReasonOpenFailed)
		return fmt.Errorf("%w: %v", ErrOpenFailed, err)
	}
	var createdID string
	if err := json.Unmarshal(result, &createdID); err != nil {
		if m.compensatingKill(ptyID) && m.recorder != nil {
			m.recorder.Remove(ptyID)
		}
		state.phase = phaseFailed
		m.removeSession(frame.Session, state)
		state.operations.Unlock()
		m.emitError(frame.Session, ReasonOpenFailed)
		return fmt.Errorf("%w: %w", ErrOpenFailed, ErrInvalidResponse)
	}
	if createdID != ptyID {
		if m.recorder != nil {
			_ = m.recorder.Add(createdID)
		}
		if m.compensatingKill(createdID) && m.recorder != nil {
			m.recorder.Remove(createdID)
		}
		state.phase = phaseFailed
		m.removeSession(frame.Session, state)
		state.operations.Unlock()
		m.emitError(frame.Session, ReasonOpenFailed)
		return ErrCreateIDMismatch
	}
	if !m.isCurrentSession(frame.Session, state) {
		// Shutdown may have forgotten this opening session while pty:create was
		// in flight. Kill again after the successful response so a create that
		// raced ahead of the first cleanup kill cannot leave an orphan behind.
		if m.compensatingKill(createdID) && m.recorder != nil {
			m.recorder.Remove(createdID)
		}
		state.phase = phaseFailed
		state.operations.Unlock()
		return ErrSessionNotFound
	}

	state.phase = phaseActive
	state.operations.Unlock()
	m.emitFrame(agentrelay.Upstream{Type: agentrelay.UpstreamReady, Session: frame.Session})
	return nil
}

// Shutdown kills every PTY this Mapper still tracks and forgets them. It emits
// no upstream frames: the Hub link is already gone when cleanup runs.
func (m *Mapper) Shutdown(ctx context.Context) error {
	m.sessionsMu.Lock()
	sessions := make([]string, 0, len(m.sessions))
	for session := range m.sessions {
		sessions = append(sessions, session)
	}
	clear(m.sessions)
	m.sessionsMu.Unlock()

	errs := make(chan error, len(sessions))
	var kills sync.WaitGroup
	for _, session := range sessions {
		// Deliberately do not take state.operations here. The entry is already
		// gone, so concurrent operations fail isCurrentSession, while taking the
		// mutex could let one hung BAT invocation block cleanup for every PTY.
		kills.Add(1)
		go func() {
			defer kills.Done()
			id := ptyID(session)
			result, err := m.invoke(ctx, "pty:kill", struct {
				ID string `json:"id"`
			}{ID: id})
			if err != nil {
				errs <- fmt.Errorf("%w: %v", ErrCloseFailed, err)
				return
			}
			if isTrue(result) {
				if m.recorder != nil {
					m.recorder.Remove(id)
				}
			} else {
				errs <- fmt.Errorf("%w: %w", ErrCloseFailed, ErrInvalidResponse)
			}
		}()
	}
	kills.Wait()
	close(errs)
	var joined []error
	for err := range errs {
		joined = append(joined, err)
	}
	return errors.Join(joined...)
}

// compensatingKill reports whether bat-server answered JSON true.
func (m *Mapper) compensatingKill(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), compensatingKillTimeout)
	defer cancel()
	result, err := m.invoke(ctx, "pty:kill", struct {
		ID string `json:"id"`
	}{ID: id})
	return err == nil && isTrue(result)
}

// Kill ends the PTY id outside any Mapper and reports whether bat-server
// answered JSON true. bat-server answers true for an id it does not know as
// well, so true means the PTY is gone either way.
func Kill(ctx context.Context, invoker Invoker, id string) bool {
	result, err := invoker.Invoke(ctx, "pty:kill", struct {
		ID string `json:"id"`
	}{ID: id})
	return err == nil && isTrue(result)
}

func (m *Mapper) input(ctx context.Context, frame agentrelay.Downstream) error {
	if !utf8.Valid(frame.Data) {
		m.emitError(frame.Session, ReasonInputInvalidUTF8)
		return ErrInvalidUTF8Input
	}
	result, err := m.invoke(ctx, "pty:write", struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}{ID: ptyID(frame.Session), Data: string(frame.Data)})
	if err != nil {
		m.emitError(frame.Session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %v", ErrWriteFailed, err)
	}
	if !isTrue(result) {
		m.emitError(frame.Session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %w", ErrWriteFailed, ErrInvalidResponse)
	}
	return nil
}

func (m *Mapper) resize(ctx context.Context, frame agentrelay.Downstream) error {
	result, err := m.invoke(ctx, "pty:set-viewport-size", struct {
		ID     string `json:"id"`
		Cols   int    `json:"cols"`
		Rows   int    `json:"rows"`
		Source string `json:"source"`
	}{ID: ptyID(frame.Session), Cols: frame.Cols, Rows: frame.Rows, Source: "desktop"})
	if err != nil {
		m.emitError(frame.Session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %v", ErrResizeFailed, err)
	}
	var viewport struct {
		Cols *int `json:"cols"`
		Rows *int `json:"rows"`
	}
	if err := json.Unmarshal(result, &viewport); err != nil || viewport.Cols == nil || viewport.Rows == nil || *viewport.Cols != frame.Cols || *viewport.Rows != frame.Rows {
		m.emitError(frame.Session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %w", ErrResizeFailed, ErrInvalidResponse)
	}
	return nil
}

func (m *Mapper) close(ctx context.Context, session string) error {
	m.sessionsMu.Lock()
	state := m.sessions[session]
	m.sessionsMu.Unlock()
	if state == nil {
		return nil
	}

	state.operations.Lock()
	defer state.operations.Unlock()
	if !m.isCurrentSession(session, state) {
		return nil
	}
	if state.phase == phaseClosing || state.phase == phaseFailed {
		return nil
	}
	if state.phase != phaseActive {
		m.emitError(session, ReasonTerminalUnavailable)
		return ErrSessionNotFound
	}
	state.phase = phaseClosing

	result, err := m.invoke(ctx, "pty:kill", struct {
		ID string `json:"id"`
	}{ID: ptyID(session)})
	if err != nil {
		m.emitError(session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %v", ErrCloseFailed, err)
	}
	if isTrue(result) {
		if m.recorder != nil {
			m.recorder.Remove(ptyID(session))
		}
	} else {
		m.emitError(session, ReasonConnectionInterrupted)
		return fmt.Errorf("%w: %w", ErrCloseFailed, ErrInvalidResponse)
	}
	return nil
}

func (m *Mapper) withActiveSession(session string, operation func() error) error {
	m.sessionsMu.Lock()
	state := m.sessions[session]
	m.sessionsMu.Unlock()
	if state == nil {
		m.emitError(session, ReasonTerminalUnavailable)
		return ErrSessionNotFound
	}

	state.operations.Lock()
	defer state.operations.Unlock()
	if !m.isCurrentSession(session, state) || state.phase != phaseActive {
		m.emitError(session, ReasonTerminalUnavailable)
		return ErrSessionNotFound
	}
	return operation()
}

// HandleEvent translates one bat-server event. Unrelated contexts and PTYs are
// silently ignored.
func (m *Mapper) HandleEvent(event batremote.Event) error {
	if event.ContextID != m.contextID {
		return nil
	}
	if event.Channel != "pty:output" && event.Channel != "pty:exit" {
		return nil
	}
	if !utf8.Valid(event.Params) {
		return ErrInvalidEvent
	}

	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(event.Params, &identity); err != nil || identity.ID == "" {
		return ErrInvalidEvent
	}
	session, ok := strings.CutPrefix(identity.ID, ptyIDPrefix)
	if !ok || session == "" {
		return nil
	}
	m.sessionsMu.Lock()
	state, exists := m.sessions[session]
	m.sessionsMu.Unlock()
	if !exists || identity.ID != ptyID(session) {
		return nil
	}

	switch event.Channel {
	case "pty:output":
		var params struct {
			ID   string  `json:"id"`
			Data *string `json:"data"`
		}
		if err := json.Unmarshal(event.Params, &params); err != nil || params.Data == nil {
			m.emitError(session, ReasonConnectionInterrupted)
			return ErrInvalidEvent
		}
		frame := agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Session: session, Data: []byte(*params.Data)}
		if !m.emitFrame(frame) {
			m.emitError(session, ReasonConnectionInterrupted)
			return ErrInvalidEvent
		}
	case "pty:exit":
		var params struct {
			ID       string `json:"id"`
			ExitCode *int   `json:"exitCode"`
		}
		if err := json.Unmarshal(event.Params, &params); err != nil || params.ExitCode == nil {
			m.emitError(session, ReasonConnectionInterrupted)
			return ErrInvalidEvent
		}
		frame := agentrelay.Upstream{Type: agentrelay.UpstreamExit, Session: session, Code: params.ExitCode}
		if _, err := agentrelay.EncodeUpstream(frame); err != nil {
			m.emitError(session, ReasonConnectionInterrupted)
			return ErrInvalidEvent
		}
		if m.recorder != nil {
			m.recorder.Remove(ptyID(session))
		}
		m.removeSession(session, state)
		m.emitFrame(frame)
	}
	return nil
}

func (m *Mapper) invoke(ctx context.Context, channel string, params any) (json.RawMessage, error) {
	if m.invoker == nil {
		return nil, ErrInvokerUnavailable
	}
	return m.invoker.Invoke(ctx, channel, params)
}

func (m *Mapper) removeSession(session string, state *sessionState) {
	m.sessionsMu.Lock()
	if m.sessions[session] == state {
		delete(m.sessions, session)
	}
	m.sessionsMu.Unlock()
}

func (m *Mapper) isCurrentSession(session string, state *sessionState) bool {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	return m.sessions[session] == state
}

func (m *Mapper) emitError(session, reason string) {
	m.emitFrame(agentrelay.Upstream{Type: agentrelay.UpstreamError, Session: session, Reason: reason})
}

func (m *Mapper) emitFrame(frame agentrelay.Upstream) bool {
	if _, err := agentrelay.EncodeUpstream(frame); err != nil {
		return false
	}
	if m.emit == nil {
		return true
	}
	m.emitMu.Lock()
	m.emit(frame)
	m.emitMu.Unlock()
	return true
}

func ptyID(session string) string { return ptyIDPrefix + session }

func isTrue(result json.RawMessage) bool {
	var value bool
	return json.Unmarshal(result, &value) == nil && value
}
