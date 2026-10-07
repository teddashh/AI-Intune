package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	operatorTerminalSubprotocol     = "aiintune.operator-terminal.v1"
	operatorTerminalReadLimit       = 64 << 10
	operatorTerminalDefaultCols     = 80
	operatorTerminalDefaultRows     = 24
	operatorTerminalOpenWait        = 2 * time.Second
	operatorTerminalDuplicateWindow = time.Second
	operatorTerminalBusyInitial     = 20 * time.Millisecond
	operatorTerminalBusyMax         = 200 * time.Millisecond

	operatorTerminalSessionMissing = "找不到這個終端工作階段。請回到機器頁重新開啟。"
	operatorTerminalOriginRefused  = "這個終端只接受從 Hub 頁面開啟的連線。請從機器頁開啟終端。"
	operatorTerminalOpenFailed     = "目前無法開啟終端。請回到機器頁再試一次。"
	operatorTerminalUnavailable    = "終端目前無法使用。請稍後回到機器頁再開啟。"

	operatorTerminalInputInvalid     = "這個終端收到無法處理的輸入，已結束。請回到機器頁重新開啟。"
	operatorTerminalTooSlow          = "這個終端的輸出超過這一頁的接收速度，終端已結束。請回到機器頁重新開啟。"
	operatorTerminalEnded            = "這個終端已結束，未再接受輸入。請回到機器頁重新開啟。"
	operatorTerminalDisconnectedCopy = "這個終端與機器的連線已中斷，終端已結束。請回到機器頁重新開啟。"
	operatorTerminalRevoked          = "這個終端的存取權已撤銷，終端已結束。請回到機器頁。"
	operatorTerminalAuthDown         = "Hub 目前無法確認操作者身分，終端已結束。請稍後回到機器頁重新開啟。"
	operatorTerminalMachineFailed    = "這台機器的終端發生錯誤，終端已結束。請回到機器頁重新開啟。"
	operatorTerminalNoLinkCopy       = "這台的終端連線目前沒有接上 Hub，這個終端已關閉。請確認機器上的 agent 與 bat-server 都在執行，再回到機器頁重新開啟。"
	operatorTerminalDuplicateCopy    = "這個終端已在另一個分頁開啟，這一頁沒有連上它。請回到原本的分頁。"
)

// operatorTerminalEnd is the first reason a page socket stops. The zero value
// means the session is still running.
type operatorTerminalEnd uint8

const (
	operatorTerminalRunning operatorTerminalEnd = iota
	operatorTerminalViewerClosed
	operatorTerminalViewerInvalid
	operatorTerminalViewerTooSlow
	operatorTerminalExited
	operatorTerminalExitWithoutCode
	operatorTerminalAlreadyClosed
	operatorTerminalDisconnected
	operatorTerminalAccessRevoked
	operatorTerminalAuthUnavailable
	operatorTerminalMachineError
	operatorTerminalNoLink
	operatorTerminalDuplicate
	operatorTerminalDuplicateEnded
)

// operatorTerminalSettings are the timings and test seams for one socket.
// A nil channel or hook uses the production behavior.
type operatorTerminalSettings struct {
	openWait          time.Duration
	openTimeout       <-chan time.Time
	duplicateWindow   time.Duration
	duplicateWait     func(context.Context, func() bool) bool
	outputBudget      int
	heartbeatEvery    time.Duration
	heartbeatWait     time.Duration
	heartbeatTicks    <-chan time.Time
	reauthEvery       time.Duration
	reauthTicks       <-chan time.Time
	busyInitial       time.Duration
	busyMax           time.Duration
	busyWait          func(context.Context, time.Duration) error
	beforeOpenSession func()
	afterPageCommand  func()
	onReaderBlocked   func()
	onPingStart       func()
	onPingDone        func(error)
	pageWriteGate     <-chan struct{}
	onWriteGated      func()
}

var (
	operatorTerminalSettingsMu       sync.Mutex
	operatorTerminalSettingsOverride *operatorTerminalSettings
)

func currentOperatorTerminalSettings() operatorTerminalSettings {
	operatorTerminalSettingsMu.Lock()
	defer operatorTerminalSettingsMu.Unlock()
	if operatorTerminalSettingsOverride == nil {
		return defaultOperatorTerminalSettings()
	}
	return operatorTerminalSettingsOverride.normalized()
}

func defaultOperatorTerminalSettings() operatorTerminalSettings {
	return operatorTerminalSettings{
		openWait:        operatorTerminalOpenWait,
		duplicateWindow: operatorTerminalDuplicateWindow,
		outputBudget:    4 * agentrelay.MaxDataSize,
		heartbeatEvery:  agentTerminalHeartbeatEvery,
		heartbeatWait:   agentTerminalHeartbeatWait,
		reauthEvery:     agentSessionRevocationInterval,
		busyInitial:     operatorTerminalBusyInitial,
		busyMax:         operatorTerminalBusyMax,
	}
}

func (s operatorTerminalSettings) normalized() operatorTerminalSettings {
	base := defaultOperatorTerminalSettings()
	if s.openWait > 0 {
		base.openWait = s.openWait
	}
	base.openTimeout = s.openTimeout
	if s.duplicateWindow > 0 {
		base.duplicateWindow = s.duplicateWindow
	}
	base.duplicateWait = s.duplicateWait
	if s.outputBudget > 0 {
		base.outputBudget = s.outputBudget
	}
	if s.heartbeatEvery > 0 {
		base.heartbeatEvery = s.heartbeatEvery
	}
	if s.heartbeatWait > 0 {
		base.heartbeatWait = s.heartbeatWait
	}
	base.heartbeatTicks = s.heartbeatTicks
	if s.reauthEvery > 0 {
		base.reauthEvery = s.reauthEvery
	}
	base.reauthTicks = s.reauthTicks
	if s.busyInitial > 0 {
		base.busyInitial = s.busyInitial
	}
	if s.busyMax > 0 {
		base.busyMax = s.busyMax
	}
	base.busyWait = s.busyWait
	base.beforeOpenSession = s.beforeOpenSession
	base.afterPageCommand = s.afterPageCommand
	base.onReaderBlocked = s.onReaderBlocked
	base.onPingStart = s.onPingStart
	base.onPingDone = s.onPingDone
	base.pageWriteGate = s.pageWriteGate
	base.onWriteGated = s.onWriteGated
	return base
}

func registerOperatorTerminalSocket(mux *http.ServeMux, h *hub, authorizer operatorRequestAuthorizer, authority string) string {
	mux.HandleFunc(operatorTerminalSocketPattern, func(w http.ResponseWriter, r *http.Request) {
		h.handleOperatorTerminalSocket(w, r, authorizer, authority)
	})
	return operatorTerminalSocketPattern
}

// operatorTerminalOriginAllowed requires exactly one Origin whose scheme
// matches the connection and whose host, with the scheme default port filled
// in, canonicalizes to the pinned listener authority.
func operatorTerminalOriginAllowed(r *http.Request, authority string) bool {
	values := r.Header.Values("Origin")
	if len(values) != 1 {
		return false
	}
	parsed, err := url.Parse(values[0])
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if parsed.Scheme != scheme {
		return false
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	canonical, ok := canonicalLiteralAuthority(net.JoinHostPort(parsed.Hostname(), port))
	return ok && canonical == authority
}

func (h *hub) handleOperatorTerminalSocket(w http.ResponseWriter, r *http.Request, authorizer operatorRequestAuthorizer, authority string) {
	principal, ok := operatorauth.PrincipalFromContext(r.Context())
	if !ok || principal.TailnetUserID == "" {
		http.Error(w, operatorTerminalSessionMissing, http.StatusNotFound)
		return
	}
	if !operatorTerminalOriginAllowed(r, authority) {
		http.Error(w, operatorTerminalOriginRefused, http.StatusForbidden)
		return
	}
	machineID := r.PathValue("id")
	sessionID := r.PathValue("session")
	row, err := h.store.AgentSessionForOperator(machineID, sessionID, principal.TailnetUserID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, operatorTerminalSessionMissing, http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("cannot read terminal session: connection refused; please repair the database: %v", err)
		http.Error(w, operatorTerminalOpenFailed, http.StatusInternalServerError)
		return
	}
	if h.agentLinks == nil {
		http.Error(w, operatorTerminalUnavailable, http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{operatorTerminalSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != operatorTerminalSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "terminal subprotocol required")
		return
	}
	conn.SetReadLimit(operatorTerminalReadLimit)

	settings := currentOperatorTerminalSettings()
	if settings.beforeOpenSession != nil {
		settings.beforeOpenSession()
	}
	session := newOperatorTerminalSession(h, conn, row, settings)
	err = h.agentLinks.OpenSession(row.SessionID, row.MachineID, session)
	switch {
	case errors.Is(err, agentlink.ErrNoLink):
		h.finishOperatorTerminalNow(session, conn, row.SessionID, operatorTerminalNoLink, 0, false)
		return
	case errors.Is(err, agentlink.ErrDuplicateSession):
		h.refuseDuplicateOperatorTerminal(r.Context(), conn, row, settings)
		return
	case err != nil:
		h.finishOperatorTerminalNow(session, conn, row.SessionID, operatorTerminalMachineError, 0, false)
		return
	}

	// The unattached sweep or a revocation can close the row after the
	// handshake read and before this route exists. The reloaded page's
	// post-attach read has to observe that close, so the route is removed
	// before any open frame.
	if _, err := h.store.AgentSessionForOperator(row.MachineID, row.SessionID, row.OperatorTailnetUserID); err != nil {
		end := operatorTerminalAlreadyClosed
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("cannot verify whether terminal session remains open: connection closed; please repair the database: %v", err)
			end = operatorTerminalMachineError
		}
		h.finishOperatorTerminalNow(session, conn, row.SessionID, end, 0, true)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		session.loops.Wait()
	}()
	session.start(ctx, r, authorizer)
	end, code := session.waitCause()
	session.shutdown(end, code)
}

func (h *hub) refuseDuplicateOperatorTerminal(ctx context.Context, conn *websocket.Conn, row store.AgentSessionResult, settings operatorTerminalSettings) {
	closed := h.operatorTerminalDuplicateClosed(ctx, row, settings)
	end := operatorTerminalDuplicate
	if closed {
		end = operatorTerminalDuplicateEnded
	}
	// The other page still owns the row and the route.
	if payload, ok := operatorTerminalPageFrame(end, 0); ok {
		writeCtx, cancel := context.WithTimeout(context.Background(), agentTerminalWriteTimeout)
		_ = conn.Write(writeCtx, websocket.MessageText, payload)
		cancel()
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (h *hub) operatorTerminalDuplicateClosed(ctx context.Context, row store.AgentSessionResult, settings operatorTerminalSettings) bool {
	closed := func() bool {
		_, err := h.store.AgentSessionForOperator(row.MachineID, row.SessionID, row.OperatorTailnetUserID)
		return errors.Is(err, store.ErrNotFound)
	}
	if settings.duplicateWait != nil {
		return settings.duplicateWait(ctx, closed)
	}
	deadline := time.Now().Add(settings.duplicateWindow)
	for {
		if closed() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		step := 5 * time.Millisecond
		if remain := time.Until(deadline); remain < step {
			step = remain
		}
		timer := time.NewTimer(step)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closed()
		case <-timer.C:
		}
	}
}

func (h *hub) finishOperatorTerminalNow(session *operatorTerminalSession, conn *websocket.Conn, sessionID string, end operatorTerminalEnd, code int, closeRoute bool) {
	if session != nil {
		session.mu.Lock()
		if session.end == operatorTerminalRunning {
			session.end = end
			session.exitCode = code
		}
		session.mu.Unlock()
	}
	h.persistOperatorTerminalClose(sessionID, end)
	if closeRoute && h.agentLinks != nil {
		h.agentLinks.CloseSessions([]string{sessionID}, operatorTerminalRegistryReason(end))
	}
	if payload, ok := operatorTerminalPageFrame(end, code); ok {
		writeCtx, cancel := context.WithTimeout(context.Background(), agentTerminalWriteTimeout)
		_ = conn.Write(writeCtx, websocket.MessageText, payload)
		cancel()
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (h *hub) persistOperatorTerminalClose(sessionID string, end operatorTerminalEnd) {
	reason := operatorTerminalLedgerReason(end)
	if reason == "" || h == nil || h.store == nil {
		return
	}
	if _, err := h.store.CloseAgentSessionsByID([]string{sessionID}, reason); err != nil {
		log.Printf("terminal session closed, but closure state could not be written to database; please repair the database: %v", err)
	}
}

func operatorTerminalLedgerReason(end operatorTerminalEnd) string {
	switch end {
	case operatorTerminalViewerClosed:
		return store.AgentSessionCloseReasonViewerClosed
	case operatorTerminalViewerInvalid:
		return store.AgentSessionCloseReasonViewerInvalid
	case operatorTerminalViewerTooSlow:
		return store.AgentSessionCloseReasonViewerTooSlow
	case operatorTerminalExited, operatorTerminalExitWithoutCode:
		return store.AgentSessionCloseReasonExited
	case operatorTerminalDisconnected:
		return store.AgentSessionCloseReasonAgentConnectionEnded
	case operatorTerminalAccessRevoked:
		return store.AgentSessionCloseReasonAccessRevoked
	case operatorTerminalAuthUnavailable:
		return store.AgentSessionCloseReasonAuthUnavailable
	case operatorTerminalMachineError:
		return store.AgentSessionCloseReasonMachineError
	case operatorTerminalNoLink:
		return store.AgentSessionCloseReasonTerminalNotLinked
	default:
		return ""
	}
}

func operatorTerminalRegistryReason(end operatorTerminalEnd) agentlink.CloseReason {
	if end == operatorTerminalAccessRevoked {
		return agentlink.ReasonSessionRevoked
	}
	return agentlink.ReasonAgentDisconnected
}

func operatorTerminalPageFrame(end operatorTerminalEnd, code int) ([]byte, bool) {
	var (
		payload []byte
		err     error
	)
	switch end {
	case operatorTerminalViewerClosed:
		return nil, false
	case operatorTerminalViewerInvalid:
		payload, err = agentrelay.EncodePageError(operatorTerminalInputInvalid)
	case operatorTerminalViewerTooSlow:
		payload, err = agentrelay.EncodePageError(operatorTerminalTooSlow)
	case operatorTerminalExited:
		payload, err = agentrelay.EncodePageExit(code)
	case operatorTerminalExitWithoutCode, operatorTerminalAlreadyClosed, operatorTerminalDuplicateEnded:
		payload, err = agentrelay.EncodePageError(operatorTerminalEnded)
	case operatorTerminalDisconnected:
		payload, err = agentrelay.EncodePageError(operatorTerminalDisconnectedCopy)
	case operatorTerminalAccessRevoked:
		payload, err = agentrelay.EncodePageError(operatorTerminalRevoked)
	case operatorTerminalAuthUnavailable:
		payload, err = agentrelay.EncodePageError(operatorTerminalAuthDown)
	case operatorTerminalMachineError:
		payload, err = agentrelay.EncodePageError(operatorTerminalMachineFailed)
	case operatorTerminalNoLink:
		payload, err = agentrelay.EncodePageError(operatorTerminalNoLinkCopy)
	case operatorTerminalDuplicate:
		payload, err = agentrelay.EncodePageError(operatorTerminalDuplicateCopy)
	default:
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	return payload, true
}

type operatorTerminalCommand struct {
	resize bool
	data   []byte
	cols   int
	rows   int
}

type operatorTerminalQueued struct {
	ready bool
	data  []byte
}

type operatorTerminalFinish struct {
	end  operatorTerminalEnd
	code int
}

type operatorTerminalHold struct {
	mu    sync.Mutex
	held  bool
	epoch uint64
}

func (h *operatorTerminalHold) begin() {
	h.mu.Lock()
	h.held = true
	h.epoch++
	h.mu.Unlock()
}

func (h *operatorTerminalHold) end() {
	h.mu.Lock()
	h.held = false
	h.epoch++
	h.mu.Unlock()
}

func (h *operatorTerminalHold) snapshot() (bool, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held, h.epoch
}

func (h *operatorTerminalHold) interfered(epoch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held || h.epoch != epoch
}

type operatorTerminalSession struct {
	hub        *hub
	conn       *websocket.Conn
	sessionID  string
	machineID  string
	operatorID string
	settings   operatorTerminalSettings

	mu          sync.Mutex
	end         operatorTerminalEnd
	exitCode    int
	queue       []operatorTerminalQueued
	queuedBytes int
	readySent   bool

	causeCh    chan struct{}
	readyCh    chan struct{}
	frames     chan operatorTerminalCommand
	finishCh   chan operatorTerminalFinish
	pageWake   chan struct{}
	writerDone chan struct{}
	loops      sync.WaitGroup
	hold       operatorTerminalHold
}

func newOperatorTerminalSession(h *hub, conn *websocket.Conn, row store.AgentSessionResult, settings operatorTerminalSettings) *operatorTerminalSession {
	return &operatorTerminalSession{
		hub: h, conn: conn, sessionID: row.SessionID, machineID: row.MachineID,
		operatorID: row.OperatorTailnetUserID, settings: settings,
		causeCh:    make(chan struct{}, 1),
		readyCh:    make(chan struct{}),
		frames:     make(chan operatorTerminalCommand, 1),
		finishCh:   make(chan operatorTerminalFinish, 1),
		pageWake:   make(chan struct{}, 1),
		writerDone: make(chan struct{}),
	}
}

// Deliver runs on the machine link's only reader. It records, enqueues, and
// wakes. It does not block, does not fail, and does not touch the store, the
// registry, or the socket. A second delivery after the end is already chosen
// is ignored, including the one CloseSessions makes.
func (s *operatorTerminalSession) Deliver(frame agentrelay.Upstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end != operatorTerminalRunning {
		return nil
	}
	switch frame.Type {
	case agentrelay.UpstreamReady:
		if s.readySent {
			return nil
		}
		s.readySent = true
		s.queue = append(s.queue, operatorTerminalQueued{ready: true})
		s.wakeLocked(s.pageWake)
		close(s.readyCh)
	case agentrelay.UpstreamOutput:
		next := s.queuedBytes + len(frame.Data)
		if next > s.settings.outputBudget {
			s.setEndLocked(operatorTerminalViewerTooSlow, 0)
			return nil
		}
		copied := append([]byte{}, frame.Data...)
		s.queue = append(s.queue, operatorTerminalQueued{data: copied})
		s.queuedBytes = next
		s.wakeLocked(s.pageWake)
	case agentrelay.UpstreamExit:
		if frame.Code == nil {
			s.setEndLocked(operatorTerminalExitWithoutCode, 0)
			return nil
		}
		s.setEndLocked(operatorTerminalExited, *frame.Code)
	case agentrelay.UpstreamError:
		s.setEndLocked(operatorTerminalEndForReason(frame.Reason), 0)
	default:
		s.setEndLocked(operatorTerminalMachineError, 0)
	}
	return nil
}

func operatorTerminalEndForReason(reason string) operatorTerminalEnd {
	switch reason {
	case agentlink.ReasonAgentDisconnected.String():
		return operatorTerminalDisconnected
	case agentlink.ReasonSessionRevoked.String():
		return operatorTerminalAccessRevoked
	default:
		return operatorTerminalMachineError
	}
}

func (s *operatorTerminalSession) setEndLocked(end operatorTerminalEnd, code int) {
	if s.end != operatorTerminalRunning {
		return
	}
	s.end = end
	s.exitCode = code
	s.wakeLocked(s.causeCh)
}

func (s *operatorTerminalSession) wakeLocked(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *operatorTerminalSession) requestEnd(end operatorTerminalEnd, code int) {
	s.mu.Lock()
	s.setEndLocked(end, code)
	s.mu.Unlock()
}

func (s *operatorTerminalSession) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end != operatorTerminalRunning
}

func (s *operatorTerminalSession) readyFired() bool {
	select {
	case <-s.readyCh:
		return true
	default:
		return false
	}
}

func (s *operatorTerminalSession) waitCause() (operatorTerminalEnd, int) {
	for {
		s.mu.Lock()
		end, code := s.end, s.exitCode
		s.mu.Unlock()
		if end != operatorTerminalRunning {
			return end, code
		}
		<-s.causeCh
	}
}

func (s *operatorTerminalSession) start(ctx context.Context, r *http.Request, authorizer operatorRequestAuthorizer) {
	s.loops.Add(5)
	go s.writeLoop(ctx)
	go s.produce(ctx)
	go s.readLoop(ctx)
	go s.heartbeat(ctx)
	go s.reauthorize(ctx, r, authorizer)
}

func (s *operatorTerminalSession) shutdown(end operatorTerminalEnd, code int) {
	s.hub.persistOperatorTerminalClose(s.sessionID, end)
	if s.hub.agentLinks != nil {
		s.hub.agentLinks.CloseSessions([]string{s.sessionID}, operatorTerminalRegistryReason(end))
	}
	select {
	case s.finishCh <- operatorTerminalFinish{end: end, code: code}:
	case <-s.writerDone:
	}
	<-s.writerDone
}

func (s *operatorTerminalSession) readLoop(ctx context.Context) {
	defer s.loops.Done()
	for {
		kind, payload, err := s.conn.Read(ctx)
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				s.requestEnd(operatorTerminalViewerInvalid, 0)
			} else if ctx.Err() == nil {
				s.requestEnd(operatorTerminalViewerClosed, 0)
			}
			return
		}
		if kind != websocket.MessageText {
			s.requestEnd(operatorTerminalViewerInvalid, 0)
			return
		}
		frame, err := agentrelay.DecodePage(payload)
		if err != nil {
			s.requestEnd(operatorTerminalViewerInvalid, 0)
			return
		}
		cmd := operatorTerminalCommand{
			resize: frame.Type == agentrelay.PageResize,
			data:   append([]byte(nil), frame.Data...),
			cols:   frame.Cols, rows: frame.Rows,
		}
		if !s.enqueueCommand(ctx, cmd) {
			return
		}
	}
}

func (s *operatorTerminalSession) enqueueCommand(ctx context.Context, cmd operatorTerminalCommand) bool {
	if s.settings.onReaderBlocked == nil {
		select {
		case s.frames <- cmd:
			return true
		case <-ctx.Done():
			return false
		}
	}
	select {
	case s.frames <- cmd:
		return true
	default:
		s.settings.onReaderBlocked()
		select {
		case s.frames <- cmd:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func (s *operatorTerminalSession) produce(ctx context.Context) {
	defer s.loops.Done()
	cols, rows, ok := s.openGeometry(ctx)
	if !ok {
		return
	}
	if err := s.sendDownstream(ctx, agentrelay.Downstream{
		Type: agentrelay.DownstreamOpen, Session: s.sessionID, Cols: cols, Rows: rows,
	}); err != nil {
		s.noteSend(err)
		return
	}
	openCols, openRows := cols, rows
	if !s.waitReady(ctx, &cols, &rows) {
		return
	}
	if cols != openCols || rows != openRows {
		if err := s.sendDownstream(ctx, agentrelay.Downstream{
			Type: agentrelay.DownstreamResize, Session: s.sessionID, Cols: cols, Rows: rows,
		}); err != nil {
			s.noteSend(err)
			return
		}
	}
	s.forward(ctx)
}

func (s *operatorTerminalSession) openGeometry(ctx context.Context) (int, int, bool) {
	timeout := s.settings.openTimeout
	var timer *time.Timer
	if timeout == nil {
		timer = time.NewTimer(s.settings.openWait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return 0, 0, false
		case <-timeout:
			return operatorTerminalDefaultCols, operatorTerminalDefaultRows, true
		case frame, ok := <-s.frames:
			if !ok {
				return 0, 0, false
			}
			if frame.resize {
				s.notePageCommand()
				return frame.cols, frame.rows, true
			}
			s.notePageCommand()
		}
	}
}

func (s *operatorTerminalSession) waitReady(ctx context.Context, cols, rows *int) bool {
	for !s.readyFired() {
		select {
		case <-ctx.Done():
			return false
		case <-s.readyCh:
		case frame, ok := <-s.frames:
			if !ok {
				return false
			}
			if frame.resize {
				*cols, *rows = frame.cols, frame.rows
			}
			s.notePageCommand()
		}
	}
	return s.drainCommands(ctx, cols, rows)
}

func (s *operatorTerminalSession) drainCommands(ctx context.Context, cols, rows *int) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case frame, ok := <-s.frames:
			if !ok {
				return false
			}
			if frame.resize {
				*cols, *rows = frame.cols, frame.rows
			}
			s.notePageCommand()
		default:
			return true
		}
	}
}

func (s *operatorTerminalSession) forward(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-s.frames:
			if !ok {
				return
			}
			down := agentrelay.Downstream{Type: agentrelay.DownstreamInput, Session: s.sessionID, Data: frame.data}
			if frame.resize {
				down = agentrelay.Downstream{Type: agentrelay.DownstreamResize, Session: s.sessionID, Cols: frame.cols, Rows: frame.rows}
			}
			if err := s.sendDownstream(ctx, down); err != nil {
				s.noteSend(err)
				return
			}
			s.notePageCommand()
		}
	}
}

func (s *operatorTerminalSession) notePageCommand() {
	if s.settings.afterPageCommand != nil {
		s.settings.afterPageCommand()
	}
}

func (s *operatorTerminalSession) noteSend(err error) {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, agentlink.ErrUnknownSession) || errors.Is(err, agentlink.ErrNoLink) {
		return
	}
	s.requestEnd(operatorTerminalMachineError, 0)
}

// sendDownstream is the only caller of Registry.Send for open, input, and
// resize. A busy answer retries that same frame so order is preserved.
func (s *operatorTerminalSession) sendDownstream(ctx context.Context, frame agentrelay.Downstream) error {
	delay := s.settings.busyInitial
	holding := false
	defer func() {
		if holding {
			s.hold.end()
		}
	}()
	for {
		err := s.hub.agentLinks.Send(s.sessionID, frame)
		if !errors.Is(err, agentlink.ErrLinkBusy) {
			return err
		}
		if !holding {
			s.hold.begin()
			holding = true
		}
		if err := s.waitBusy(ctx, delay); err != nil {
			return err
		}
		if delay < s.settings.busyMax {
			delay *= 2
			if delay > s.settings.busyMax {
				delay = s.settings.busyMax
			}
		}
	}
}

func (s *operatorTerminalSession) waitBusy(ctx context.Context, delay time.Duration) error {
	if s.settings.busyWait != nil {
		return s.settings.busyWait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *operatorTerminalSession) writeLoop(ctx context.Context) {
	defer s.loops.Done()
	defer close(s.writerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.finishCh:
			s.writeFinishBody(cmd)
			return
		case <-s.pageWake:
			if !s.waitGate(ctx) {
				return
			}
			s.writeQueued()
		}
	}
}

func (s *operatorTerminalSession) waitGate(ctx context.Context) bool {
	if s.settings.pageWriteGate == nil {
		return true
	}
	if s.settings.onWriteGated != nil {
		s.settings.onWriteGated()
	}
	select {
	case <-s.settings.pageWriteGate:
		return true
	case cmd := <-s.finishCh:
		s.writeFinishBody(cmd)
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *operatorTerminalSession) writeFinishBody(cmd operatorTerminalFinish) {
	if cmd.end == operatorTerminalExited || cmd.end == operatorTerminalExitWithoutCode {
		s.writeQueued()
	} else {
		s.discardQueued()
	}
	if payload, ok := operatorTerminalPageFrame(cmd.end, cmd.code); ok {
		_ = s.writePayload(payload)
	}
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

func (s *operatorTerminalSession) writeQueued() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		msg := s.queue[0]
		s.queue = s.queue[1:]
		if !msg.ready {
			s.queuedBytes -= len(msg.data)
			if s.queuedBytes < 0 {
				s.queuedBytes = 0
			}
		}
		s.mu.Unlock()
		var (
			payload []byte
			err     error
		)
		if msg.ready {
			payload, err = agentrelay.EncodePageReady()
		} else {
			payload, err = agentrelay.EncodePageOutput(msg.data)
		}
		if err != nil {
			s.requestEnd(operatorTerminalMachineError, 0)
			return
		}
		if err := s.writePayload(payload); err != nil {
			s.requestEnd(operatorTerminalViewerClosed, 0)
			return
		}
	}
}

func (s *operatorTerminalSession) discardQueued() {
	s.mu.Lock()
	s.queue = nil
	s.queuedBytes = 0
	s.mu.Unlock()
}

func (s *operatorTerminalSession) writePayload(payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentTerminalWriteTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, payload)
}

func (s *operatorTerminalSession) heartbeat(ctx context.Context) {
	defer s.loops.Done()
	ticks := s.settings.heartbeatTicks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(s.settings.heartbeatEvery)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if s.ended() {
			return
		}
		held, epoch := s.hold.snapshot()
		if held {
			continue
		}
		if s.settings.onPingStart != nil {
			s.settings.onPingStart()
		}
		pingCtx, cancel := context.WithTimeout(ctx, s.settings.heartbeatWait)
		err := s.conn.Ping(pingCtx)
		cancel()
		if s.settings.onPingDone != nil {
			s.settings.onPingDone(err)
		}
		if err == nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if s.hold.interfered(epoch) {
			continue
		}
		s.requestEnd(operatorTerminalViewerClosed, 0)
		return
	}
}

func (s *operatorTerminalSession) reauthorize(ctx context.Context, r *http.Request, authorizer operatorRequestAuthorizer) {
	defer s.loops.Done()
	ticks := s.settings.reauthTicks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(s.settings.reauthEvery)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if s.ended() {
			return
		}
		s.checkOperator(r, authorizer)
		if s.ended() {
			return
		}
	}
}

func (s *operatorTerminalSession) checkOperator(r *http.Request, authorizer operatorRequestAuthorizer) {
	if authorizer == nil {
		s.requestEnd(operatorTerminalAuthUnavailable, 0)
		return
	}
	authed, decision := authorizer.Authorize(r, operatorauth.Operate)
	pass := decision.Allowed && authed != nil
	if pass {
		principal, ok := operatorauth.PrincipalFromContext(authed.Context())
		pass = ok && principal.TailnetUserID == s.operatorID
	}
	if pass {
		return
	}
	if decision.Code == operatorauth.AuthSourceUnavailable || decision.Code == operatorauth.AuthConfigurationInvalid {
		s.requestEnd(operatorTerminalAuthUnavailable, 0)
		return
	}
	s.requestEnd(operatorTerminalAccessRevoked, 0)
}
