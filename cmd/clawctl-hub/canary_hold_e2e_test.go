package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatoragent"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// canaryE2E boots the production Hub handler on loopback and presents every
// accepted connection as a Tailscale source. The operator client still dials
// the pinned tailnet authority; DialContext is the only redirect. Nothing here
// is selectable from Hub flags.
type canaryE2E struct {
	t         *testing.T
	hub       *hub
	store     *store.Store
	resolver  *e2eWhoIs
	authority string
	client    *operatorclient.Client
	agent     *http.Client
	base      string
}

type e2eMachine struct {
	name  string
	id    string
	token string
}

type e2eWhoIs struct {
	mu     sync.Mutex
	dest   netip.Addr
	who    *apitype.WhoIsResponse
	calls  int
	remote string
}

func (r *e2eWhoIs) Status(context.Context) (operatorauth.ResolverStatus, error) {
	return operatorauth.ResolverStatus{
		Version: "1.102.2", BackendState: "Running", TailscaleIPs: []netip.Addr{r.dest},
	}, nil
}

func (r *e2eWhoIs) WhoIsForIP(_ context.Context, remote string, destination netip.Addr) (*apitype.WhoIsResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.remote = remote
	if destination != r.dest {
		return nil, fmt.Errorf("whois destination %s, want %s", destination, r.dest)
	}
	if !strings.HasPrefix(remote, "100.100.10.20:") {
		return nil, fmt.Errorf("whois remote %q is not the wrapped tailnet source", remote)
	}
	return r.who, nil
}

func (r *e2eWhoIs) callsSoFar() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.remote
}

type fixedRemoteListener struct {
	net.Listener
	remote net.Addr
}

func (l fixedRemoteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return fixedRemoteConn{Conn: conn, remote: l.remote}, nil
}

type fixedRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c fixedRemoteConn) RemoteAddr() net.Addr { return c.remote }

func startCanaryE2E(t *testing.T) *canaryE2E {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Match serve(): check-in refuses to hand out a policy token until this
	// process has published the expectations it actually loaded.
	st.SetExpectations(&expect.Set{})
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	artifactsDir := artifactsDirFor(dbPath)
	h := &hub{store: st, artifactsDir: artifactsDir}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener %T", ln.Addr())
	}
	const destinationIP = "100.64.0.1"
	destination := netip.MustParseAddr(destinationIP)
	authority := fmt.Sprintf("%s:%d", destinationIP, tcp.Port)
	const prefix = "example.com/cap/clawctl"
	who := &apitype.WhoIsResponse{
		Node: &tailcfg.Node{StableID: "node-stable-e2e", Name: "operator-laptop.example.ts.net."},
		UserProfile: &tailcfg.UserProfile{
			ID: 42, LoginName: "operator@example.com", DisplayName: "Operator Example",
		},
		CapMap: tailcfg.PeerCapMap{
			tailcfg.PeerCapability(prefix + "-view"):    nil,
			tailcfg.PeerCapability(prefix + "-operate"): nil,
			tailcfg.PeerCapability(prefix + "-admin"):   nil,
		},
	}
	resolver := &e2eWhoIs{dest: destination, who: who}
	authorizer, err := operatorauth.NewWithResolver(operatorauth.Config{
		Destination: destination, CapabilityPrefix: prefix,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHubHTTPHandler(h, ui, authorizer, authority)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	listener := fixedRemoteListener{
		Listener: ln,
		remote:   &net.TCPAddr{IP: net.ParseIP("100.100.10.20"), Port: 4321},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	dialAddr := ln.Addr().String()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, dialAddr)
		},
	}
	client, err := operatorclient.NewWithHTTPClient("http://"+authority, &http.Client{Transport: transport, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &canaryE2E{
		t: t, hub: h, store: st, resolver: resolver, authority: authority, client: client,
		agent: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		base:  "http://" + dialAddr,
	}
}

func (e *canaryE2E) enrollFleet(names ...string) []e2eMachine {
	e.t.Helper()
	now := time.Now().UTC()
	out := make([]e2eMachine, 0, len(names))
	for _, name := range names {
		token, err := e.store.CreateEnrollToken(name, time.Hour)
		if err != nil {
			e.t.Fatal(err)
		}
		var enrolled model.EnrollResponse
		e.agentJSON(http.MethodPost, "/v1/enrollments", "", model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "u",
		}, http.StatusOK, &enrolled)
		if enrolled.MachineID == "" || enrolled.AgentToken == "" {
			e.t.Fatalf("enroll %s: %+v", name, enrolled)
		}
		e.agentJSON(http.MethodPost, "/v1/checkins", enrolled.AgentToken, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: now, AgentVersion: "e2e",
			BootID: "boot-" + name, AgentSeq: 1, AgentStartedAt: now.Add(-time.Hour),
		}, http.StatusOK, nil)
		e.agentJSON(http.MethodPost, "/v1/observations:batch", enrolled.AgentToken, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
		}, http.StatusNoContent, nil)
		if err := e.store.SetMachineChannel(enrolled.MachineID, "canary"); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, e2eMachine{name: name, id: enrolled.MachineID, token: enrolled.AgentToken})
	}
	return out
}

func (e *canaryE2E) agentJSON(method, path, token string, body any, want int, dest any) {
	e.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, e.base+path, bytes.NewReader(raw))
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.agent.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, payload)
	}
	if dest != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, dest); err != nil {
			e.t.Fatalf("decode %s: %v body=%s", path, err, payload)
		}
	}
}

func (e *canaryE2E) publishArtifact() artifactSidecar {
	e.t.Helper()
	record := writeJobTestArtifact(e.t, e.hub.artifactsDir, "2026.9.8", "canary-hold-e2e")
	record.EnginesNode = ">=24.15.0 <25"
	if err := writeArtifactSidecar(e.hub.artifactsDir, record); err != nil {
		e.t.Fatal(err)
	}
	return record
}

func (e *canaryE2E) operatorRaw(method, path, body string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://"+e.authority+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := e.agent.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, payload
}

func TestCanaryHoldPausesUntilOperatorExpandAndStopsOnFailure(t *testing.T) {
	t.Run("succeeded canary pauses then expands", func(t *testing.T) {
		e := startCanaryE2E(t)
		machines := e.enrollFleet("e2e-1", "e2e-2", "e2e-3")
		record := e.publishArtifact()
		ctx := context.Background()

		omitted := fmt.Sprintf(`{"channel":"canary","version":%q,"artifact_sha256":%q,"execution_timeout_seconds":600,"irreversible":false}`,
			record.Version, record.SHA256)
		status, body := e.operatorRaw(http.MethodPost, "/v1/operator/deployments/preview", omitted)
		if status != http.StatusOK {
			t.Fatalf("omitted batch_size preview = %d %s", status, body)
		}
		var omittedPreview operator.DeploymentCreatePreviewResult
		if err := json.Unmarshal(body, &omittedPreview); err != nil {
			t.Fatal(err)
		}
		if omittedPreview.BatchSize != 1 || omittedPreview.TotalBatches != 3 || omittedPreview.Impact != 3 {
			t.Fatalf("omitted batch_size is not canary-first of 1: %+v", omittedPreview)
		}
		batchCounts := map[int]int{}
		for _, target := range omittedPreview.Targets {
			if target.ExcludedReason == nil {
				batchCounts[target.BatchNo]++
			}
		}
		if batchCounts[1] != 1 || batchCounts[2] != 1 || batchCounts[3] != 1 {
			t.Fatalf("omitted batch_size batches = %v", batchCounts)
		}

		preview, err := e.client.PreviewDeploymentCreate(ctx, operator.DeploymentCreatePreviewRequest{
			Channel: "canary", Version: record.Version, ArtifactSHA256: record.SHA256,
			BatchSize: 2, ExecutionTimeoutSeconds: 600,
		})
		if err != nil {
			t.Fatal(err)
		}
		if preview.BatchSize != 2 || preview.TotalBatches != 2 || preview.Impact != 3 || !preview.CreateAllowed {
			t.Fatalf("batch 2 preview = %+v", preview)
		}
		later := map[int]int{}
		for _, target := range preview.Targets {
			if target.ExcludedReason != nil {
				t.Fatalf("excluded target %+v", target)
			}
			later[target.BatchNo]++
		}
		if later[1] != 1 || later[2] != 2 {
			t.Fatalf("later batch size 2 did not keep the first machine alone: %v targets=%+v", later, preview.Targets)
		}

		created, err := e.client.CreateDeployment(ctx, "e2e-create-canary", operatorclient.DeploymentCreateRequest{
			Channel: "canary", Version: record.Version, ArtifactSHA256: record.SHA256,
			BatchSize: 2, ExecutionTimeoutSeconds: 600,
			PreviewDigest: preview.PreviewDigest, ConfirmChannel: "canary", ConfirmVersion: record.Version,
			Reason: "e2e canary hold",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.State != store.DeploymentRunning || created.OpenedBatch != 1 || created.BatchSize != 2 || len(created.Jobs) != 1 {
			t.Fatalf("create opened more than the canary: %+v", created)
		}
		detail, err := e.client.Deployment(ctx, created.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if !detail.Item.PauseAfterCanary || detail.Item.OpenedBatch != 1 || detail.Item.TotalBatches != 2 {
			t.Fatalf("new deployment did not record the hold: %+v", detail.Item)
		}
		canary := machineByID(t, machines, created.Jobs[0].MachineID)
		if canary.name != "e2e-1" {
			t.Fatalf("canary machine = %s, want the first display name e2e-1", canary.name)
		}

		e.succeedJob(canary, created.Jobs[0].JobID)
		e.hub.advanceDeployments(time.Now().UTC())
		detail, err = e.client.Deployment(ctx, created.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Item.State != store.DeploymentPaused || detail.Item.OpenedBatch != 1 {
			t.Fatalf("succeeded canary did not pause: %+v", detail.Item)
		}
		if jobs, err := e.store.ListJobs("", 20); err != nil || len(jobs) != 1 {
			t.Fatalf("pause opened jobs: len=%d err=%v", len(jobs), err)
		}
		assertHubEventKinds(t, e.store, map[string]int{store.HubDeploymentCanaryHeld: 1}, store.HubDeploymentContinued, store.HubDeploymentPaused)

		svc := &operatoragent.Service{Hub: e.client}
		statusRaw, err := svc.Call(ctx, "rollout_status", json.RawMessage(`{"deployment_id":"`+created.DeploymentID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		view := statusRaw.(map[string]any)
		if view["deployment_state"] != store.DeploymentPaused || view["opened_batch"] != 1 {
			t.Fatalf("rollout_status = %#v", view)
		}
		previewRaw, err := svc.Call(ctx, "deployment_continue_preview", json.RawMessage(`{"deployment_id":"`+created.DeploymentID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		actionPreview := previewRaw.(operator.DeploymentActionPreviewResult)
		if !actionPreview.Eligibility.Eligible || actionPreview.PreviewDigest == "" {
			t.Fatalf("continue preview = %+v", actionPreview)
		}
		rev := view["control_revision"].(int64)
		opened := view["opened_batch"].(int)
		expandBody, err := json.Marshal(map[string]any{
			"deployment_id": created.DeploymentID, "preview_digest": actionPreview.PreviewDigest,
			"expected_control_revision": rev, "expected_opened_batch": opened,
			"confirm_channel": "canary", "reason": "expand after succeeded canary",
			"idempotency_key": "e2e-expand-canary",
		})
		if err != nil {
			t.Fatal(err)
		}
		expanded, err := svc.Call(ctx, "rollout_expand", expandBody)
		if err != nil {
			t.Fatal(err)
		}
		wrote, _ := expanded.(map[string]any)["wrote"].(bool)
		if !wrote {
			t.Fatalf("rollout_expand did not write: %#v", expanded)
		}
		detail, err = e.client.Deployment(ctx, created.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Item.State != store.DeploymentRunning || detail.Item.OpenedBatch != 2 {
			t.Fatalf("expand did not open batch 2: %+v", detail.Item)
		}
		if jobs, err := e.store.ListJobs("", 20); err != nil || len(jobs) != 3 {
			t.Fatalf("expanded job count = %d err=%v", len(jobs), err)
		}
		e.hub.advanceDeployments(time.Now().UTC())
		if jobs, _ := e.store.ListJobs("", 20); len(jobs) != 3 {
			t.Fatalf("driver opened another batch after the explicit continue: %d", len(jobs))
		}
		calls, remote := e.resolver.callsSoFar()
		if calls == 0 || !strings.HasPrefix(remote, "100.100.10.20:") {
			t.Fatalf("WhoIs calls=%d remote=%q", calls, remote)
		}
	})

	t.Run("failed canary stops the rollout", func(t *testing.T) {
		e := startCanaryE2E(t)
		machines := e.enrollFleet("e2e-1", "e2e-2", "e2e-3")
		record := e.publishArtifact()
		ctx := context.Background()
		preview, err := e.client.PreviewDeploymentCreate(ctx, operator.DeploymentCreatePreviewRequest{
			Channel: "canary", Version: record.Version, ArtifactSHA256: record.SHA256,
			BatchSize: 2, ExecutionTimeoutSeconds: 600,
		})
		if err != nil {
			t.Fatal(err)
		}
		created, err := e.client.CreateDeployment(ctx, "e2e-create-failed-canary", operatorclient.DeploymentCreateRequest{
			Channel: "canary", Version: record.Version, ArtifactSHA256: record.SHA256,
			BatchSize: 2, ExecutionTimeoutSeconds: 600,
			PreviewDigest: preview.PreviewDigest, ConfirmChannel: "canary", ConfirmVersion: record.Version,
			Reason: "e2e failed canary",
		})
		if err != nil {
			t.Fatal(err)
		}
		canary := machineByID(t, machines, created.Jobs[0].MachineID)
		var rejected model.JobStateResponse
		lease := e.claim(canary, created.Jobs[0].JobID)
		e.agentJSON(http.MethodPost, "/v1/jobs/"+created.Jobs[0].JobID+"/reject", canary.token, model.JobRejectRequest{
			LeaseToken: lease.LeaseToken, RejectionCode: string(deploy.PreconditionFailed), Seq: 1,
		}, http.StatusOK, &rejected)
		if rejected.State != string(deploy.Rejected) {
			t.Fatalf("reject state = %+v", rejected)
		}
		detail, err := e.client.Deployment(ctx, created.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Item.State != store.DeploymentPaused || detail.Item.OpenedBatch != 1 {
			t.Fatalf("failed canary did not stop: %+v", detail.Item)
		}
		if jobs, err := e.store.ListJobs("", 20); err != nil || len(jobs) != 1 {
			t.Fatalf("failed canary opened jobs: len=%d err=%v", len(jobs), err)
		}
		assertHubEventKinds(t, e.store, map[string]int{store.HubDeploymentPaused: 1}, store.HubDeploymentCanaryHeld, store.HubDeploymentContinued)

		svc := &operatoragent.Service{Hub: e.client}
		_, err = svc.Call(ctx, "rollout_expand", json.RawMessage(`{"deployment_id":"`+created.DeploymentID+`"}`))
		var callErr *operatoragent.CallError
		if !errors.As(err, &callErr) || callErr.Code != "canary_blocked" {
			t.Fatalf("rollout_expand err = %v", err)
		}
		rev := detail.Item.ControlRevision
		opened := detail.Item.OpenedBatch
		continueBody, err := json.Marshal(map[string]any{
			"deployment_id": created.DeploymentID, "preview_digest": "sha256:" + strings.Repeat("ab", 32),
			"expected_control_revision": rev, "expected_opened_batch": opened,
			"confirm_channel": "canary", "reason": "try to skip the failure",
			"idempotency_key": "e2e-continue-refused",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Call(ctx, "deployment_continue", continueBody)
		callErr = nil
		if !errors.As(err, &callErr) || callErr.Code != "failed_batch_skip_refused" {
			t.Fatalf("deployment_continue err = %v", err)
		}
		if jobs, _ := e.store.ListJobs("", 20); len(jobs) != 1 {
			t.Fatalf("refused continue opened jobs: %d", len(jobs))
		}
		assertHubEventKinds(t, e.store, map[string]int{store.HubDeploymentPaused: 1}, store.HubDeploymentCanaryHeld, store.HubDeploymentContinued)
		audits, err := e.store.Audit("", 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range audits {
			if entry.Action == store.AuditDeploymentContinue {
				t.Fatalf("MCP continue wrote an audit row: %+v", entry)
			}
		}
	})
}

func (e *canaryE2E) claim(machine e2eMachine, jobID string) model.JobLeaseResponse {
	e.t.Helper()
	var lease model.JobLeaseResponse
	e.agentJSON(http.MethodPost, "/v1/jobs/"+jobID+"/claims", machine.token, struct{}{}, http.StatusOK, &lease)
	if lease.LeaseToken == "" {
		e.t.Fatal("claim returned an empty lease")
	}
	return lease
}

func (e *canaryE2E) succeedJob(machine e2eMachine, jobID string) {
	e.t.Helper()
	lease := e.claim(machine, jobID)
	now := time.Now().UTC()
	prefix := "/v1/jobs/" + jobID
	e.agentJSON(http.MethodPost, prefix+"/events", machine.token, model.JobEventRequest{
		LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: now,
	}, http.StatusAccepted, nil)
	e.agentJSON(http.MethodPost, prefix+"/verifications", machine.token, model.JobVerificationRequest{
		LeaseToken: lease.LeaseToken, RuleID: "health", Command: "true", Passed: true, VerifiedAt: now,
	}, http.StatusCreated, nil)
	e.agentJSON(http.MethodPost, prefix+"/events", machine.token, model.JobEventRequest{
		LeaseToken: lease.LeaseToken, Seq: 2, Phase: "finish", OccurredAt: now,
	}, http.StatusAccepted, nil)
	var state model.JobStateResponse
	e.agentJSON(http.MethodPost, prefix+"/complete", machine.token, model.JobCompleteRequest{
		LeaseToken: lease.LeaseToken,
	}, http.StatusOK, &state)
	if state.State != string(deploy.Succeeded) {
		e.t.Fatalf("complete state = %+v", state)
	}
}

func machineByID(t *testing.T, machines []e2eMachine, id string) e2eMachine {
	t.Helper()
	for _, machine := range machines {
		if machine.id == id {
			return machine
		}
	}
	t.Fatalf("machine %s is not in the enrolled fleet", id)
	return e2eMachine{}
}

func assertHubEventKinds(t *testing.T, st *store.Store, want map[string]int, absent ...string) {
	t.Helper()
	events, err := st.HubEventsBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, event := range events {
		got[event.Kind]++
	}
	for kind, count := range want {
		if got[kind] != count {
			t.Fatalf("hub event %s count=%d, want %d; events=%+v", kind, got[kind], count, events)
		}
	}
	for _, kind := range absent {
		if got[kind] != 0 {
			t.Fatalf("hub event %s was recorded: %+v", kind, events)
		}
	}
}
