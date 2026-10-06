package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	// The v1 decoders reject unknown fields, so this wire version has no
	// forward compatibility. Any new field requires aiintune.terminal.v2.
	agentTerminalSubprotocol = "aiintune.terminal.v1"

	agentTerminalReadLimit   = 2 << 20
	agentTerminalQueueFrames = 256
	// Input and resize stop at three quarters of the frame queue so open and
	// close keep the remaining slots. EncodeDownstream rejects Data above
	// agentrelay.MaxDataSize, and the assertion below keeps that cap inside
	// the byte budget, so one valid input frame is never busy forever.
	agentTerminalInputQueueFrames = agentTerminalQueueFrames * 3 / 4
	agentTerminalQueueByteBudget  = 1 << 20
	agentTerminalWriteTimeout     = 10 * time.Second
	agentTerminalHeartbeatEvery   = 30 * time.Second
	agentTerminalHeartbeatWait    = 10 * time.Second
	agentTerminalCloseReasonMax   = 123
)

// A negative result does not fit in uint, so raising MaxDataSize above the
// byte budget fails the build instead of creating an input frame that is
// busy forever.
const _ uint = agentTerminalQueueByteBudget - agentrelay.MaxDataSize

var (
	errAgentTerminalLinkClosed   = errors.New("agent terminal link is closed")
	errAgentTerminalFrameInvalid = errors.New("agent terminal frame is invalid")
	errAgentTerminalQueueFull    = errors.New("agent terminal link frame queue is full")
)

type queuedDownstream struct {
	payload      []byte
	payloadBytes int
}

type wsLink struct {
	conn  *websocket.Conn
	queue chan queuedDownstream
	done  chan struct{}

	mu          sync.Mutex
	closed      bool
	queuedBytes int
	closeOnce   sync.Once
}

func newWSLink(conn *websocket.Conn) *wsLink {
	return &wsLink{
		conn:  conn,
		queue: make(chan queuedDownstream, agentTerminalQueueFrames),
		done:  make(chan struct{}),
	}
}

func (l *wsLink) Send(frame agentrelay.Downstream) error {
	// Encode before taking the lock. A frame that breaks a relay rule is
	// returned to the caller and the link stays open. json.Marshal already
	// allocated this payload, so the queue does not keep the caller's Data.
	payload, err := agentrelay.EncodeDownstream(frame)
	if err != nil {
		return fmt.Errorf("%w: %w", errAgentTerminalFrameInvalid, err)
	}
	payloadBytes := len(frame.Data)

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errAgentTerminalLinkClosed
	}
	// Input and resize that do not fit are a busy answer. Open and close
	// carry no Data and use the rest of the frame queue. A full frame queue
	// is the only Send path that closes the link.
	overFrames := len(l.queue) >= agentTerminalInputQueueFrames
	overBytes := int64(l.queuedBytes)+int64(payloadBytes) > int64(agentTerminalQueueByteBudget)
	if (frame.Type == agentrelay.DownstreamInput && (overFrames || overBytes)) ||
		(frame.Type == agentrelay.DownstreamResize && overFrames) {
		l.mu.Unlock()
		return agentlink.ErrLinkBusy
	}
	queued := queuedDownstream{payload: payload, payloadBytes: payloadBytes}
	select {
	case <-l.done:
		l.mu.Unlock()
		return errAgentTerminalLinkClosed
	case l.queue <- queued:
		l.queuedBytes += payloadBytes
		l.mu.Unlock()
		return nil
	default:
		l.markClosedLocked()
		l.mu.Unlock()
		go l.closeSocket(websocket.StatusNormalClosure, "terminal send queue exceeded")
		return errAgentTerminalQueueFull
	}
}

func (l *wsLink) Close(reason string) {
	l.markClosed()
	l.closeSocket(websocket.StatusNormalClosure, reason)
}

func (l *wsLink) markClosed() {
	l.mu.Lock()
	l.markClosedLocked()
	l.mu.Unlock()
}

func (l *wsLink) markClosedLocked() {
	if l.closed {
		return
	}
	l.closed = true
	close(l.done)
}

func (l *wsLink) closeWithStatus(status websocket.StatusCode, reason string) {
	l.markClosed()
	l.closeSocket(status, reason)
}

func (l *wsLink) closeSocket(status websocket.StatusCode, reason string) {
	l.closeOnce.Do(func() {
		_ = l.conn.Close(status, truncateWebSocketCloseReason(reason))
	})
}

func truncateWebSocketCloseReason(reason string) string {
	if len(reason) <= agentTerminalCloseReasonMax {
		return reason
	}
	end := agentTerminalCloseReasonMax
	for end > 0 && !utf8.ValidString(reason[:end]) {
		end--
	}
	return reason[:end]
}

func (l *wsLink) take(ctx context.Context) (queuedDownstream, bool) {
	select {
	case <-ctx.Done():
		return queuedDownstream{}, false
	case <-l.done:
		return queuedDownstream{}, false
	case queued := <-l.queue:
		l.mu.Lock()
		l.queuedBytes -= queued.payloadBytes
		l.mu.Unlock()
		return queued, true
	}
}

func (l *wsLink) writeLoop(ctx context.Context) {
	for {
		queued, ok := l.take(ctx)
		if !ok {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, agentTerminalWriteTimeout)
		err := l.conn.Write(writeCtx, websocket.MessageText, queued.payload)
		cancel()
		if err != nil {
			l.Close("terminal write failed")
			return
		}
		// This is one socket writer, but Registry.Send does not impose
		// per-session order. The operator-side session handler must own one
		// sender goroutine per session to provide that invariant.
	}
}

func (l *wsLink) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(agentTerminalHeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, agentTerminalHeartbeatWait)
			err := l.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				l.Close("terminal heartbeat failed")
				return
			}
		}
	}
}

func (h *hub) persistAgentTerminalClosures(sessionIDs []string, reason string) {
	if _, err := h.store.CloseAgentSessionsByID(sessionIDs, reason); err != nil {
		log.Printf("終端工作階段已關閉，但關閉狀態未能寫入資料庫；請修復資料庫：%v", err)
	}
}

func (h *hub) detachAgentTerminalLink(machineID string, link agentlink.Link) {
	closedSessionIDs := h.agentLinks.Detach(machineID, link)
	h.persistAgentTerminalClosures(closedSessionIDs, store.AgentSessionCloseReasonAgentConnectionEnded)
}

func (h *hub) attachAgentTerminalLink(machineID string, link agentlink.Link) (bool, error) {
	closedSessionIDs, err := h.agentLinks.Attach(machineID, link)
	if err != nil {
		return false, err
	}
	h.persistAgentTerminalClosures(closedSessionIDs, store.AgentSessionCloseReasonAgentConnectionEnded)

	retiredMachineIDs, err := h.store.RetiredMachineIDs([]string{machineID})
	if err != nil {
		h.detachAgentTerminalLink(machineID, link)
		return false, err
	}
	if len(retiredMachineIDs) != 0 {
		h.detachAgentTerminalLink(machineID, link)
		link.Close(agentlink.ReasonSessionRevoked.String())
		return false, nil
	}
	return true, nil
}

func (h *hub) handleAgentTerminalLink(w http.ResponseWriter, r *http.Request, machineID string) {
	// No deadline handling belongs here. net/http applies ReadTimeout and
	// WriteTimeout as absolute connection deadlines before the request starts,
	// but Hijack clears them (net/http server.go hijackLocked), and
	// websocket.Accept always hijacks. Clearing them again is dead code.
	if len(r.Header.Values("Origin")) != 0 {
		http.Error(w, "websocket origin is forbidden", http.StatusForbidden)
		return
	}
	if h.agentLinks == nil {
		http.Error(w, "agent terminal links unavailable", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{agentTerminalSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != agentTerminalSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "terminal subprotocol required")
		return
	}

	// TestAgentTerminalReadLimitExceedsLargestCanonicalFrame enforces that
	// canonical relay frames fit below this limit. The guarantee covers
	// canonical encoder output only: DecodeUpstream also accepts whitespace
	// and \uXXXX spellings that can legitimately exceed this limit.
	conn.SetReadLimit(agentTerminalReadLimit)

	// A hijacked request context remains valid until ServeHTTP returns. It is
	// the parent so any loop ending cancels the other two loops as well.
	ctx, cancel := context.WithCancel(r.Context())
	link := newWSLink(conn)
	attached, err := h.attachAgentTerminalLink(machineID, link)
	if err != nil {
		cancel()
		link.Close("terminal link setup failed")
		log.Printf("無法確認這台機器是否仍可使用終端：連線已關閉；請修復資料庫：%v", err)
		return
	}
	if !attached {
		cancel()
		return
	}
	defer h.detachAgentTerminalLink(machineID, link)

	// Neither loop cancels ctx. Cancelling it aborts the read loop mid-close
	// and tears the socket down before the close frame reaches the agent, so a
	// replaced link would report an abnormal closure. Both loops already call
	// Close on failure, and closing the socket is what ends the read loop.
	var loops sync.WaitGroup
	loops.Add(3)
	go func() {
		defer loops.Done()
		link.writeLoop(ctx)
	}()
	go func() {
		defer loops.Done()
		link.heartbeatLoop(ctx)
	}()
	// The bearer was checked once, before the upgrade. Re-check it on the
	// revocation cadence so a re-enrollment or retirement that replaced the
	// credential also ends the link it opened, together with its sessions.
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	go func() {
		defer loops.Done()
		h.agentTerminalCredentialLoop(ctx, link, machineID, bearer, agentTerminalCredentialTicks)
	}()
	defer func() {
		cancel()
		link.Close("terminal link ended")
		loops.Wait()
	}()

	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			link.closeWithStatus(websocket.StatusUnsupportedData, "terminal text frames required")
			return
		}
		frame, err := agentrelay.DecodeUpstream(payload)
		if err != nil {
			link.closeWithStatus(websocket.StatusPolicyViolation, "invalid terminal frame")
			return
		}

		// Sink.Deliver must not block. Registry.Deliver invokes it
		// synchronously from this machine's only reader; a slow sink prevents
		// pong processing, times out the heartbeat, and kills every session on
		// the machine. Operator-side sinks must therefore be bounded and
		// non-blocking.
		err = h.agentLinks.Deliver(machineID, frame)
		switch {
		case err == nil:
			continue
		case errors.Is(err, agentlink.ErrUnknownSession):
			// The operator may have closed the session concurrently.
			continue
		case errors.Is(err, agentlink.ErrForeignSession):
			link.closeWithStatus(websocket.StatusPolicyViolation, "foreign terminal session")
			return
		default:
			link.closeWithStatus(websocket.StatusInternalError, "terminal delivery failed")
			return
		}
	}
}

// agentTerminalCredentialTicks overrides the credential re-check cadence in
// tests. Nil means a ticker at agentSessionRevocationInterval.
var agentTerminalCredentialTicks func() (<-chan time.Time, func())

// agentTerminalCredentialChecked, when set by a test, observes each completed
// re-check (current is false on a database error). Nil in production.
var agentTerminalCredentialChecked func(current bool)

// agentTerminalCredentialLoop closes the link once its bearer is no longer the
// machine's current agent token. A database error keeps the link: an outage
// is not a revocation, and the operator side still re-authorizes every page.
// The bearer is never logged.
func (h *hub) agentTerminalCredentialLoop(ctx context.Context, link *wsLink, machineID, bearer string,
	tickSource func() (<-chan time.Time, func()),
) {
	var ticks <-chan time.Time
	stop := func() {}
	if tickSource != nil {
		ticks, stop = tickSource()
	} else {
		ticker := time.NewTicker(agentSessionRevocationInterval)
		ticks, stop = ticker.C, ticker.Stop
	}
	defer stop()
	reported := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-link.done:
			return
		case <-ticks:
		}
		current, err := h.store.AgentTokenCurrent(machineID, bearer)
		if agentTerminalCredentialChecked != nil {
			agentTerminalCredentialChecked(err == nil && current)
		}
		if err != nil {
			if !reported {
				log.Printf("無法確認終端連線的 agent token 是否仍有效：連線保留；請修復資料庫：%v", err)
				reported = true
			}
			continue
		}
		reported = false
		if !current {
			log.Printf("終端連線已關閉：這台機器的 agent token 已更換或機器已退役 machine=%s", machineID)
			link.closeWithStatus(websocket.StatusPolicyViolation, "agent token is no longer current")
			return
		}
	}
}
