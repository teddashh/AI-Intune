package store

import (
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"strings"
	"testing"
	"time"
)

func scriptFixture(t *testing.T) (*Store, ScriptRunRequest, AuditEntry, time.Time) {
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := mustEnroll(t, st, "probe-node", now)
	c := healthyCheckin(now)
	c.ScriptV1 = true
	if err := st.RecordCheckin(id, c, now); err != nil {
		t.Fatal(err)
	}
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	return st, ScriptRunRequest{ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: json.RawMessage(`{}`), Targets: []string{id}, Reason: "collect facts"}, AuditEntry{AuthSubject: "operator-test", AuthCapability: "operate"}, now
}
func TestScriptRunRejectsInvalidRequests(t *testing.T) {
	st, req, a, _ := scriptFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*ScriptRunRequest, *AuditEntry)
	}{
		{"unknown", func(r *ScriptRunRequest, a *AuditEntry) { r.ScriptID = "unknown" }},
		{"hash", func(r *ScriptRunRequest, a *AuditEntry) { r.ScriptSHA256 = strings.Repeat("a", 64) }},
		{"targets", func(r *ScriptRunRequest, a *AuditEntry) { r.Targets = make([]string, 51) }},
		{"scope", func(r *ScriptRunRequest, a *AuditEntry) { a.AuthCapability = "view" }},
		{"args", func(r *ScriptRunRequest, a *AuditEntry) { r.Args = json.RawMessage(`{"command":"true"}`) }},
		{"timeout", func(r *ScriptRunRequest, a *AuditEntry) { r.TimeoutSeconds = 61 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, actor := req, a
			tc.change(&r, &actor)
			if _, err := st.ScriptRun(r, true, "reject-"+tc.name, actor); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	var count int
	if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejection created job")
	}
}
func TestScriptRunPreviewApplyReplayCompletionAudit(t *testing.T) {
	st, req, a, now := scriptFixture(t)
	preview, err := st.ScriptRun(req, false, "", a)
	if err != nil {
		t.Fatal(err)
	}
	req.PreviewDigest = preview.PreviewDigest
	result, err := st.ScriptRun(req, true, "script-key", a)
	if err != nil || len(result.JobIDs) != 1 {
		t.Fatalf("apply %+v %v", result, err)
	}
	replay, err := st.ScriptRun(req, true, "script-key", a)
	if err != nil || !replay.Replayed || replay.JobIDs[0] != result.JobIDs[0] {
		t.Fatalf("replay %+v %v", replay, err)
	}
	changed := req
	changed.Reason = "different reason"
	if _, err := st.ScriptRun(changed, true, "script-key", a); err == nil {
		t.Fatal("idempotency conflict accepted")
	}
	id := result.JobIDs[0]
	machine := req.Targets[0]
	token, err := st.ClaimJob(id, machine, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AdvanceJobByAgent(id, machine, token, deploy.Start, now); err != nil {
		t.Fatal(err)
	}
	metadata := scriptcatalog.Result{StdoutSHA256: scriptcatalog.Digest([]byte("private evidence")), StderrSHA256: scriptcatalog.Digest(nil), ExitCode: 0}
	for rule, command := range map[string]string{scriptcatalog.Kind: req.ScriptID, "script_v1_metadata": string(mustScriptJSON(metadata))} {
		if err = st.RecordVerification(id, machine, token, rule, command, 0, "private evidence", "", true, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.AdvanceJobByAgent(id, machine, token, deploy.FinishWork, now); err != nil {
		t.Fatal(err)
	}
	if state, err := st.MarkSucceededIfVerified(id, now); err != nil || state != deploy.Succeeded {
		t.Fatalf("complete %s %v", state, err)
	}
	rows, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	complete := 0
	for _, row := range rows {
		if strings.Contains(row.Detail, "private evidence") {
			t.Fatal("raw output leaked into audit")
		}
		if row.Action == AuditScriptComplete {
			complete++
			if row.AuthSubject != a.AuthSubject || !strings.Contains(row.Detail, metadata.StdoutSHA256) {
				t.Fatal("completion provenance missing")
			}
		}
	}
	if complete != 1 {
		t.Fatalf("completion audits %d", complete)
	}
	if _, err = st.MarkSucceededIfVerified(id, now); err != nil {
		t.Fatal(err)
	}
}
func TestScriptRunFanoutRollsBackOnUnavailableTarget(t *testing.T) {
	st, req, a, _ := scriptFixture(t)
	req.Targets = append(req.Targets, "missing-machine")
	if _, err := st.ScriptRun(req, false, "", a); err == nil {
		t.Fatal("missing target accepted")
	}
	var n int
	_ = st.rdb.QueryRow(`SELECT COUNT(*) FROM desired_state`).Scan(&n)
	if n != 0 {
		t.Fatal("partial fanout")
	}
}

func TestScriptScopeTracksCatalogMode(t *testing.T) {
	for _, tc := range []struct {
		mode, scope string
		allowed     bool
	}{{"read", "operate", true}, {"read", "view", false}, {"read", "admin", false}, {"write", "operate", false}, {"write", "admin", true}} {
		err := scriptScope(scriptcatalog.Entry{Mode: tc.mode}, AuditEntry{AuthCapability: tc.scope})
		if (err == nil) != tc.allowed {
			t.Fatalf("mode=%s scope=%s err=%v", tc.mode, tc.scope, err)
		}
	}
}
func TestScriptDirectQueueBypassIsRejected(t *testing.T) {
	st, req, _, _ := scriptFixture(t)
	spec := string(mustScriptJSON(scriptcatalog.Spec{Kind: scriptcatalog.Kind, ScriptID: req.ScriptID, ScriptSHA256: req.ScriptSHA256, Args: req.Args}))
	if _, _, err := st.CreateDesiredState("machine", req.Targets[0], scriptcatalog.Kind, req.ScriptID, spec, "test"); err == nil {
		t.Fatal("direct queue bypass accepted")
	}
}
func TestScriptCompletionRequiresMatchingMetadata(t *testing.T) {
	st, req, a, now := scriptFixture(t)
	preview, err := st.ScriptRun(req, false, "", a)
	if err != nil {
		t.Fatal(err)
	}
	req.PreviewDigest = preview.PreviewDigest
	result, err := st.ScriptRun(req, true, "metadata-key", a)
	if err != nil {
		t.Fatal(err)
	}
	id := result.JobIDs[0]
	machine := req.Targets[0]
	token, err := st.ClaimJob(id, machine, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AdvanceJobByAgent(id, machine, token, deploy.Start, now); err != nil {
		t.Fatal(err)
	}
	if err = st.RecordVerification(id, machine, token, scriptcatalog.Kind, req.ScriptID, 0, "facts", "", true, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.AdvanceJobByAgent(id, machine, token, deploy.FinishWork, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.MarkSucceededIfVerified(id, now); err != ErrNoVerification {
		t.Fatalf("missing metadata must block success: %v", err)
	}
}
