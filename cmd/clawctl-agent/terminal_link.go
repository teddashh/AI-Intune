package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentpty"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/batremote"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	agentTerminalSubprotocol = "aiintune.terminal.v1"
	agentTerminalPath        = "/v1/agent/terminal-link"

	agentTerminalReadLimit       = 2 << 20
	agentTerminalQueueFrames     = 256
	agentTerminalQueueByteBudget = 1 << 20
	agentTerminalDialTimeout     = 10 * time.Second
	agentTerminalInvokeTimeout   = 30 * time.Second
	agentTerminalWriteTimeout    = 10 * time.Second
	agentTerminalHeartbeatEvery  = 30 * time.Second
	agentTerminalHeartbeatWait   = 10 * time.Second
	agentTerminalCleanupTimeout  = 30 * time.Second
	agentTerminalReapTimeout     = 10 * time.Second
	agentTerminalInvocationWait  = 5 * time.Second
	agentTerminalSettleWindow    = 40 * time.Second
	agentTerminalBackoffBase     = time.Second
	agentTerminalBackoffCap      = 40 * time.Second

	agentTerminalProfile = "default"
)

type terminalLinkDeps struct {
	goos           string
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
	jitter         func(time.Duration) time.Duration
	userHomeDir    func() (string, error)
	loadEndpoint   func(string, string) (batremote.Endpoint, error)
	dialBAT        func(context.Context, batremote.Endpoint, string, string, batremote.ClientInfo) (*batremote.Session, error)
	openProfile    func(context.Context, *batremote.Session, string) (*batremote.Context, error)
	dialHub        func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	attempt        func(context.Context, config, terminalLinkDeps) terminalAttemptResult
	settleWindow   time.Duration
	operationLimit time.Duration
	openRecord     func(home string) (*terminalRecord, error)
	invocationID   func(context.Context) (string, bool)
}

func defaultTerminalLinkDeps() terminalLinkDeps {
	return terminalLinkDeps{
		goos:        runtime.GOOS,
		now:         time.Now,
		sleep:       sleepWithContext,
		jitter:      jitter,
		userHomeDir: os.UserHomeDir,
		loadEndpoint: func(tokenPath, dataDir string) (batremote.Endpoint, error) {
			return batremote.LoadEndpoint(tokenPath, dataDir, model.BATServerPort)
		},
		dialBAT: batremote.Dial,
		openProfile: func(ctx context.Context, session *batremote.Session, profile string) (*batremote.Context, error) {
			return session.OpenProfileContext(ctx, profile)
		},
		dialHub:        websocket.Dial,
		attempt:        runTerminalLinkAttempt,
		settleWindow:   agentTerminalSettleWindow,
		operationLimit: agentTerminalInvokeTimeout,
		openRecord: func(home string) (*terminalRecord, error) {
			return agentTerminalRecords.open(terminalRecordDir(home))
		},
		invocationID: batServerInvocationID,
	}
}

// batServerInvocationID reads the InvocationID of AI-Intune's bat-server unit.
// It changes every time the unit starts, and stopping the unit ends every
// shell it started.
func batServerInvocationID(ctx context.Context) (string, bool) {
	readCtx, cancel := context.WithTimeout(ctx, agentTerminalInvocationWait)
	defer cancel()
	stdout, _, err := realSystemctl(readCtx, "--user", "show", "-p", "InvocationID", "--value", model.BATServerUnit)
	if err != nil {
		return "", false
	}
	id := strings.TrimSuffix(stdout, "\n")
	if !validInvocationID(id) {
		return "", false
	}
	return id, true
}

func runTerminalLink(ctx context.Context, cfg config) {
	runTerminalLinkWithDeps(ctx, cfg, defaultTerminalLinkDeps())
}

func runTerminalLinkWithDeps(ctx context.Context, cfg config, deps terminalLinkDeps) {
	// The catalog has no bat-server build for other operating systems. This
	// check must precede even path discovery so unsupported agents do no I/O.
	if deps.goos != agentrelay.TerminalOS {
		return
	}

	attempt := 0
	batState := terminalBATReachable
	for ctx.Err() == nil {
		result := deps.attempt(ctx, cfg, deps)
		if ctx.Err() != nil {
			return
		}

		resultState := terminalBATReachable
		switch {
		case result.recordHeld:
			resultState = terminalBATRecordHeld
			if batState != resultState {
				log.Printf("another clawctl-agent holds the terminal record; terminal link will retry")
			}
		case !result.endpointAvailable:
			resultState = terminalBATEndpointUnavailable
			if batState != resultState {
				log.Printf("bat-server endpoint not available; terminal link will retry")
			}
		case !result.batAvailable:
			resultState = terminalBATUnreachable
			if batState != resultState {
				log.Printf("bat-server is not reachable; terminal link will retry")
			}
		default:
			if result.logMessage != "" {
				log.Print(result.logMessage)
			}
		}
		batState = resultState

		if result.linked && result.survived >= deps.settleWindow {
			attempt = 0
		} else {
			attempt++
		}
		if deps.sleep(ctx, terminalRetryDelay(attempt, deps.jitter)) != nil {
			return
		}
	}
}

type terminalBATState uint8

const (
	terminalBATEndpointUnavailable terminalBATState = iota
	terminalBATRecordHeld
	terminalBATUnreachable
	terminalBATReachable
)

type terminalAttemptResult struct {
	endpointAvailable bool
	batAvailable      bool
	recordHeld        bool
	linked            bool
	survived          time.Duration
	logMessage        string
}

// terminalLinkRecorder stamps each PTY a link creates with the InvocationID
// read for that link, or with none when it could not be read.
type terminalLinkRecorder struct {
	record     *terminalRecord
	invocation string
}

func (r terminalLinkRecorder) Add(ptyID string) error { return r.record.Add(ptyID, r.invocation) }

func (r terminalLinkRecorder) Remove(ptyID string) { _ = r.record.Remove(ptyID) }

// reapTerminalRecord ends the shells an earlier link or agent process created
// and never saw end. An entry stamped with another InvocationID ended when
// that bat-server instance stopped, so it is dropped without a kill. Entries
// whose kill is not confirmed stay for the next attempt.
func reapTerminalRecord(ctx context.Context, invoker agentpty.Invoker, record *terminalRecord, invocation string, invocationOK bool) {
	reapCtx, cancel := context.WithTimeout(ctx, agentTerminalReapTimeout)
	defer cancel()
	for _, entry := range record.entries() {
		if reapCtx.Err() != nil {
			return
		}
		if entry.Invocation != "" && invocationOK && entry.Invocation != invocation {
			_ = record.Remove(entry.ID)
			continue
		}
		if agentpty.Kill(reapCtx, invoker, entry.ID) {
			_ = record.Remove(entry.ID)
		}
	}
}

func runTerminalLinkAttempt(ctx context.Context, cfg config, deps terminalLinkDeps) terminalAttemptResult {
	home, err := deps.userHomeDir()
	if err != nil {
		return terminalAttemptResult{}
	}
	record, err := deps.openRecord(home)
	if err != nil {
		return terminalAttemptResult{recordHeld: errors.Is(err, errTerminalRecordHeld)}
	}
	endpoint, err := deps.loadEndpoint(model.BATServerTokenFile(home), model.BATServerDataDir(home))
	if err != nil {
		return terminalAttemptResult{}
	}
	result := terminalAttemptResult{endpointAvailable: true}

	batSession, err := deps.dialBAT(ctx, endpoint, "clawctl-agent", "terminal-link", batremote.ClientInfo{
		AppName: "clawctl-agent", AppVersion: version, Label: "terminal-link",
		Platform: runtime.GOOS + "/" + runtime.GOARCH, DeviceID: cfg.MachineID,
	})
	if err != nil {
		return result
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, agentTerminalDialTimeout)
	profile, err := deps.openProfile(openCtx, batSession, agentTerminalProfile)
	cancelOpen()
	if err != nil {
		_ = batSession.Close()
		return result
	}

	result.batAvailable = true

	// Read the InvocationID only after the profile opens, so a unit restart
	// before this point cannot stamp the new instance's shells with the old
	// instance's id.
	invocation, invocationOK := deps.invocationID(ctx)
	if !invocationOK {
		invocation = ""
	}
	reapTerminalRecord(ctx, profile, record, invocation, invocationOK)

	dialCtx, cancelDial := context.WithTimeout(ctx, agentTerminalDialTimeout)
	conn, response, err := deps.dialHub(dialCtx, strings.TrimRight(cfg.HubURL, "/")+agentTerminalPath, &websocket.DialOptions{
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + cfg.AgentToken}},
		Subprotocols:    []string{agentTerminalSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		closeBATWithoutMapper(profile, batSession)
		result.logMessage = terminalHubDialCategory(response)
		return result
	}
	if conn.Subprotocol() != agentTerminalSubprotocol {
		closeBATWithoutMapper(profile, batSession)
		_ = conn.CloseNow()
		result.logMessage = "Hub terminal link rejected the required subprotocol; retrying"
		return result
	}

	result.linked = true
	linkedAt := deps.now()
	recorder := terminalLinkRecorder{record: record, invocation: invocation}
	normalClose := runActiveTerminalLink(ctx, conn, batSession, profile, home, deps.operationLimit, recorder)
	result.survived = deps.now().Sub(linkedAt)
	if !normalClose {
		result.logMessage = "terminal link ended; retrying"
	}
	return result
}

func terminalHubDialCategory(response *http.Response) string {
	if response == nil {
		return "Hub terminal link transport failed; retrying"
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return "Hub terminal link redirect refused; retrying"
	}
	return "Hub terminal link handshake refused; retrying"
}

func closeBATWithoutMapper(profile *batremote.Context, session *batremote.Session) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), agentTerminalCleanupTimeout)
	defer cancel()
	_ = profile.Close(cleanupCtx)
	_ = session.Close()
}

func terminalRetryDelay(attempt int, jitterFn func(time.Duration) time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := agentTerminalBackoffBase
	for i := 1; i < attempt && delay < agentTerminalBackoffCap; i++ {
		if delay > agentTerminalBackoffCap/2 {
			delay = agentTerminalBackoffCap
			break
		}
		delay *= 2
	}
	if delay > agentTerminalBackoffCap {
		delay = agentTerminalBackoffCap
	}
	delay = jitterFn(delay)
	if delay > agentTerminalBackoffCap {
		return agentTerminalBackoffCap
	}
	if delay < 0 {
		return 0
	}
	return delay
}

type queuedTerminalFrame struct {
	frame agentrelay.Downstream
	size  int
}

type terminalSessionWorker struct {
	queue   chan queuedTerminalFrame
	done    chan struct{}
	closing bool
}

type terminalDispatchResult uint8

const (
	terminalDispatchAccepted terminalDispatchResult = iota
	terminalDispatchDropped
	terminalDispatchRefused
)

type terminalDispatcher struct {
	ctx            context.Context
	cancel         context.CancelFunc
	mapper         *agentpty.Mapper
	batSession     *batremote.Session
	operationLimit time.Duration

	mu          sync.Mutex
	workers     map[string]*terminalSessionWorker
	queuedBytes int
	queuedCount int
}

func newTerminalDispatcher(ctx context.Context, cancel context.CancelFunc, mapper *agentpty.Mapper,
	batSession *batremote.Session, operationLimit time.Duration,
) *terminalDispatcher {
	return &terminalDispatcher{
		ctx: ctx, cancel: cancel, mapper: mapper, batSession: batSession,
		operationLimit: operationLimit, workers: make(map[string]*terminalSessionWorker),
	}
}

func (d *terminalDispatcher) dispatch(frame agentrelay.Downstream, size int) terminalDispatchResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	// MaxDataSize is pinned at or below the queue byte budget by a test, so the
	// per-frame check is not currently reachable from the wire. Keep the guard
	// for that constant relationship and for symmetry with the Hub dispatcher.
	if d.ctx.Err() != nil || size > agentTerminalQueueByteBudget ||
		d.queuedCount >= agentTerminalQueueFrames || d.queuedBytes > agentTerminalQueueByteBudget-size {
		return terminalDispatchRefused
	}
	worker := d.workers[frame.Session]
	if worker == nil {
		worker = &terminalSessionWorker{
			queue: make(chan queuedTerminalFrame, agentTerminalQueueFrames),
			done:  make(chan struct{}),
		}
		d.workers[frame.Session] = worker
		go d.runWorker(frame.Session, worker)
	}
	if worker.closing {
		return terminalDispatchDropped
	}
	if frame.Type == agentrelay.DownstreamClose {
		worker.closing = true
	}
	frame.Data = append([]byte(nil), frame.Data...)
	select {
	case worker.queue <- queuedTerminalFrame{frame: frame, size: size}:
		d.queuedCount++
		d.queuedBytes += size
		return terminalDispatchAccepted
	default:
		return terminalDispatchRefused
	}
}

func (d *terminalDispatcher) endSession(sessionID string) {
	d.mu.Lock()
	worker := d.workers[sessionID]
	if worker != nil {
		delete(d.workers, sessionID)
		for {
			select {
			case queued := <-worker.queue:
				d.queuedCount--
				d.queuedBytes -= queued.size
			default:
				close(worker.done)
				d.mu.Unlock()
				return
			}
		}
	}
	d.mu.Unlock()
}

func (d *terminalDispatcher) runWorker(sessionID string, worker *terminalSessionWorker) {
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-worker.done:
			return
		case queued := <-worker.queue:
			d.mu.Lock()
			d.queuedCount--
			d.queuedBytes -= queued.size
			d.mu.Unlock()

			operationCtx, cancel := context.WithTimeout(d.ctx, d.operationLimit)
			err := d.mapper.HandleDownstream(operationCtx, queued.frame)
			cancel()
			if err != nil && terminalMapperErrorEndsLink(err, d.batSession) {
				d.cancel()
				return
			}
			if queued.frame.Type == agentrelay.DownstreamClose {
				d.mu.Lock()
				if d.workers[sessionID] == worker {
					delete(d.workers, sessionID)
				}
				d.mu.Unlock()
				return
			}
		}
	}
}

func terminalMapperErrorEndsLink(err error, session *batremote.Session) bool {
	if session.Err() != nil {
		return true
	}
	return errors.Is(err, agentpty.ErrInvalidDownstream)
}

type terminalWriter struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *websocket.Conn
	queue  chan queuedTerminalPayload

	mu          sync.Mutex
	queuedBytes int
}

type queuedTerminalPayload struct {
	payload   []byte
	dataBytes int
}

func newTerminalWriter(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) *terminalWriter {
	return &terminalWriter{ctx: ctx, cancel: cancel, conn: conn, queue: make(chan queuedTerminalPayload, agentTerminalQueueFrames)}
}

func (w *terminalWriter) enqueue(frame agentrelay.Upstream) {
	payload, err := agentrelay.EncodeUpstream(frame)
	if err != nil {
		w.cancel()
		return
	}
	payload = append([]byte(nil), payload...)
	dataBytes := len(frame.Data)
	w.mu.Lock()
	if w.ctx.Err() != nil || dataBytes > agentTerminalQueueByteBudget ||
		w.queuedBytes > agentTerminalQueueByteBudget-dataBytes {
		w.mu.Unlock()
		w.cancel()
		return
	}
	select {
	case w.queue <- queuedTerminalPayload{payload: payload, dataBytes: dataBytes}:
		w.queuedBytes += dataBytes
		w.mu.Unlock()
	default:
		w.mu.Unlock()
		w.cancel()
	}
}

func (w *terminalWriter) run() {
	// coder/websocket permits concurrent writes. One writer is used here for
	// FIFO ordering, ownership of queued byte slices, bounded memory, and
	// backpressure that cannot stall the BAT event pump.
	heartbeat := time.NewTicker(agentTerminalHeartbeatEvery)
	defer heartbeat.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case queued := <-w.queue:
			w.mu.Lock()
			w.queuedBytes -= queued.dataBytes
			w.mu.Unlock()
			writeCtx, cancel := context.WithTimeout(w.ctx, agentTerminalWriteTimeout)
			err := w.conn.Write(writeCtx, websocket.MessageText, queued.payload)
			cancel()
			if err != nil {
				w.cancel()
				return
			}
		case <-heartbeat.C:
			pingCtx, cancel := context.WithTimeout(w.ctx, agentTerminalHeartbeatWait)
			err := w.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				w.cancel()
				return
			}
		}
	}
}

func runActiveTerminalLink(parent context.Context, conn *websocket.Conn, batSession *batremote.Session,
	profile *batremote.Context, home string, operationLimit time.Duration, recorder agentpty.Recorder,
) bool {
	linkCtx, cancel := context.WithCancel(parent)
	writer := newTerminalWriter(linkCtx, cancel, conn)
	var dispatcher *terminalDispatcher
	mapper := agentpty.New(profile, profile.ID(), home, recorder, func(frame agentrelay.Upstream) {
		writer.enqueue(frame)
		if frame.Type == agentrelay.UpstreamExit && dispatcher != nil {
			dispatcher.endSession(frame.Session)
		}
	})
	dispatcher = newTerminalDispatcher(linkCtx, cancel, mapper, batSession, operationLimit)
	conn.SetReadLimit(agentTerminalReadLimit)

	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		writer.run()
	}()
	go func() {
		defer loops.Done()
		for {
			select {
			case <-linkCtx.Done():
				return
			case event, ok := <-batSession.Events():
				if !ok {
					cancel()
					return
				}
				if mapper.HandleEvent(event) != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Read has a cancelable lifetime rather than an idle timeout: protocol ping
	// frames are consumed internally and do not complete Read. The writer owns a
	// bounded ping deadline, so a dead peer still cancels this call promptly.
	normalClose := false
	for linkCtx.Err() == nil {
		messageType, payload, err := conn.Read(linkCtx)
		if err != nil {
			normalClose = websocket.CloseStatus(err) == websocket.StatusNormalClosure
			break
		}
		if messageType != websocket.MessageText {
			break
		}
		frame, err := agentrelay.DecodeDownstream(payload)
		if err != nil {
			break
		}
		if dispatcher.dispatch(frame, len(frame.Data)) == terminalDispatchRefused {
			break
		}
	}
	cancel()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), agentTerminalCleanupTimeout)
	_ = mapper.Shutdown(cleanupCtx)
	_ = profile.Close(cleanupCtx)
	cleanupCancel()
	_ = batSession.Close()
	_ = conn.CloseNow()
	loops.Wait()
	return normalClose
}
