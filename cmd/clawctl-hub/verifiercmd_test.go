package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func verifierCLIServer(t *testing.T) (string, machineCommandDeps, jobsFixture) {
	t.Helper()
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	// A CLI that quietly opened a database would make the HTTP-first claim
	// untestable, so point the fallback at a path that must never be created.
	t.Setenv("CLAWCTL_DB", filepath.Join(t.TempDir(), "must-not-be-created.sqlite"))
	return base, deps, f
}

func TestVerifierRegisterCLIHTTPDeliversTheCredentialOnceAndNeverAgain(t *testing.T) {
	base, deps, f := verifierCLIServer(t)

	var previewOut, previewErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", "awx-runner", "--failure-domain", "awx", "--preview",
	}, &previewOut, &previewErr, deps); err != nil {
		t.Fatalf("preview: %v; stdout=%q stderr=%q", err, previewOut.String(), previewErr.String())
	}
	for _, want := range []string{
		`separation_rule: "failure_domain_differs_from_job_machine"`,
		`evidence_role: "independent_verifier"`,
		`credential_delivery: "first-response-only"`,
		"grants_deployment_gate: false",
		"preview-digest=sha256:",
	} {
		if !strings.Contains(previewOut.String(), want) {
			t.Fatalf("preview output lacks %q: %q", want, previewOut.String())
		}
	}
	if rows := verifierCLIRowCount(t, f); rows != 0 {
		t.Fatalf("preview created %d verifier rows", rows)
	}

	var out, errOut bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", "awx-runner", "--failure-domain", "awx",
		"--reason", "AWX job runner corroborates fleet deployments",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("register: %v; stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	credential := strings.TrimSuffix(out.String(), "\n")
	if strings.Contains(credential, "\n") || len(credential) != 43 {
		t.Fatalf("stdout must be exactly one credential line: %q", out.String())
	}
	if strings.Contains(errOut.String(), credential) {
		t.Fatal("the credential was repeated on stderr")
	}
	for _, want := range []string{
		"credential is output only this once", "clawctl-hub verifier revoke", "independent_verifier",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("follow-up output lacks %q: %q", want, errOut.String())
		}
	}
	key := regexp.MustCompile(`request key: (cli-verifier-register-[a-f0-9]{32})`).
		FindStringSubmatch(errOut.String())
	digest := regexp.MustCompile(`preview digest: (sha256:[a-f0-9]{64})`).
		FindStringSubmatch(errOut.String())
	if len(key) != 2 || len(digest) != 2 {
		t.Fatalf("receipt lacks retry coordinates: %q", errOut.String())
	}

	var replayOut, replayErr bytes.Buffer
	err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", "awx-runner", "--failure-domain", "awx",
		"--reason", "AWX job runner corroborates fleet deployments",
		"--idempotency-key", key[1], "--preview-digest", digest[1],
	}, &replayOut, &replayErr, deps)
	if err == nil {
		t.Fatal("replaying a registration reported success")
	}
	if !strings.Contains(err.Error(), "credential cannot be redisplayed") ||
		!strings.Contains(err.Error(), "revoke_and_register") {
		t.Fatalf("replay error does not state the recovery: %v", err)
	}
	if replayOut.Len() != 0 {
		t.Fatalf("replay wrote to stdout: %q", replayOut.String())
	}
	for _, text := range []string{err.Error(), replayOut.String(), replayErr.String()} {
		if strings.Contains(text, credential) {
			t.Fatalf("replay path reproduced the credential: %q", text)
		}
	}
	if rows := verifierCLIRowCount(t, f); rows != 1 {
		t.Fatalf("replay produced %d verifier rows", rows)
	}
}

func TestVerifierListShowAndRevokeCLIHTTPStateTransition(t *testing.T) {
	base, deps, _ := verifierCLIServer(t)
	var registerOut, registerErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", "awx-runner", "--failure-domain", "awx", "--reason", "corroboration",
	}, &registerOut, &registerErr, deps); err != nil {
		t.Fatalf("register: %v", err)
	}
	id := regexp.MustCompile(`verifier_id "([A-Za-z0-9_-]+)"`).FindStringSubmatch(registerErr.String())
	if len(id) != 2 {
		t.Fatalf("receipt lacks a verifier_id: %q", registerErr.String())
	}
	verifierID := id[1]

	var listOut, listErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(),
		[]string{"list", "--hub-url", base}, &listOut, &listErr, deps); err != nil {
		t.Fatalf("list: %v; stderr=%q", err, listErr.String())
	}
	if !strings.Contains(listOut.String(), "1 active / 0 revoked") ||
		!strings.Contains(listOut.String(), "awx-runner") ||
		!strings.Contains(listOut.String(), "LAST_SEEN_AT(Hub)") {
		t.Fatalf("list output=%q", listOut.String())
	}

	var showOut, showErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(),
		[]string{"show", "--hub-url", base, verifierID}, &showOut, &showErr, deps); err != nil {
		t.Fatalf("show: %v; stderr=%q", err, showErr.String())
	}
	for _, want := range []string{
		`state: "active"`, `failure_domain: "awx"`, "evidence_rows: 0",
		"grants_deployment_gate: false", "evidence_role: independent_verifier",
	} {
		if !strings.Contains(showOut.String(), want) {
			t.Fatalf("show output lacks %q: %q", want, showOut.String())
		}
	}

	var previewOut, previewErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(),
		[]string{"revoke", "--hub-url", base, "--preview", verifierID},
		&previewOut, &previewErr, deps); err != nil {
		t.Fatalf("revoke preview: %v; stderr=%q", err, previewErr.String())
	}
	for _, want := range []string{
		"registry_row_retained: true", "evidence_retained: true",
		"display_name_reusable: false", "jobs_losing_only_producer: 0",
	} {
		if !strings.Contains(previewOut.String(), want) {
			t.Fatalf("revoke preview lacks %q: %q", want, previewOut.String())
		}
	}

	var revokeOut, revokeErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"revoke", "--hub-url", base, "--reason", "rotating the runner credential",
		"--confirm-name", "awx-runner", verifierID,
	}, &revokeOut, &revokeErr, deps); err != nil {
		t.Fatalf("revoke: %v; stdout=%q stderr=%q", err, revokeOut.String(), revokeErr.String())
	}
	if !strings.HasPrefix(revokeOut.String(), "revoked:") ||
		!strings.Contains(revokeOut.String(), "revision 1 → 2") ||
		!strings.Contains(revokeOut.String(), "registry_row_retained: true") {
		t.Fatalf("revoke receipt=%q", revokeOut.String())
	}

	listOut.Reset()
	if err := runVerifierCommandWithDeps(t.Context(),
		[]string{"list", "--hub-url", base}, &listOut, &listErr, deps); err != nil {
		t.Fatalf("list after revocation: %v", err)
	}
	if !strings.Contains(listOut.String(), "0 active / 1 revoked") ||
		!strings.Contains(listOut.String(), "revoked") {
		t.Fatalf("list after revocation=%q", listOut.String())
	}

	// The revoked name stays taken, so a new registration cannot inherit the
	// identity that wrote the evidence already on file.
	var retakeOut, retakeErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", "awx-runner", "--failure-domain", "awx", "--reason", "second runner",
	}, &retakeOut, &retakeErr, deps); err == nil {
		t.Fatalf("re-registering a revoked display name succeeded: %q", retakeErr.String())
	}
}

func TestVerifierCLIRejectsIncoherentInvocations(t *testing.T) {
	base, deps, _ := verifierCLIServer(t)
	for _, test := range []struct {
		name string
		argv []string
		want string
	}{
		{"no subcommand", nil, "must specify list, show, register, revoke, or assign"},
		{"unknown subcommand", []string{"rotate"}, "unknown subcommand"},
		{"both transports", []string{"list", "--hub-url", base, "--db", "/tmp/x.sqlite"},
			"cannot both be specified"},
		{"register without reason", []string{
			"register", "--hub-url", base, "--kind", "external_job_runner",
			"--name", "n", "--failure-domain", "awx"}, "--reason cannot be empty"},
		{"register preview with apply input", []string{
			"register", "--hub-url", base, "--kind", "external_job_runner",
			"--name", "n", "--failure-domain", "awx", "--preview", "--reason", "r"},
			"--preview does not accept --reason"},
		{"register half a retry", []string{
			"register", "--hub-url", base, "--kind", "external_job_runner",
			"--name", "n", "--failure-domain", "awx", "--reason", "r",
			"--idempotency-key", "k"}, "must be provided together"},
		{"register hub prober over HTTP with a hub host", []string{
			"register", "--hub-url", base, "--kind", "hub_prober",
			"--name", "n", "--failure-domain", "d", "--reason", "r",
			"--hub-host", "samplehub1"}, "--hub-host is only for --db"},
		{"register json on apply", []string{
			"register", "--hub-url", base, "--kind", "external_job_runner",
			"--name", "n", "--failure-domain", "awx", "--reason", "r", "--json"},
			"--json is only for --preview"},
		{"revoke without confirmation", []string{
			"revoke", "--hub-url", base, "--reason", "r", "verifier-1"},
			"--confirm-name cannot be empty"},
		{"revoke preview with confirmation", []string{
			"revoke", "--hub-url", base, "--preview", "--confirm-name", "n", "verifier-1"},
			"--preview does not accept --confirm-name"},
		{"revoke retry without revision", []string{
			"revoke", "--hub-url", base, "--reason", "r", "--confirm-name", "n",
			"--idempotency-key", "k", "--preview-digest", "sha256:x", "verifier-1"},
			"retry requires original --idempotency-key, --expected-revision, and --preview-digest together"},
		{"revoke without an id", []string{"revoke", "--hub-url", base, "--preview"},
			"must provide a verifier-id"},
		{"show with a path traversal id", []string{"show", "--hub-url", base, "../etc"},
			"cannot be empty, contain leading/trailing whitespace, dot segments, or slashes"},
		{"list with a positional", []string{"list", "--hub-url", base, "extra"},
			"positional arguments not accepted"},
		{"assign without a job", []string{"assign", "--hub-url", base, "--preview", "verifier-1"},
			"--job cannot be empty"},
		{"assign without an id", []string{"assign", "--hub-url", base, "--job", "job-1", "--preview"},
			"must provide a verifier-id"},
		{"assign without reason", []string{"assign", "--hub-url", base, "--job", "job-1", "verifier-1"},
			"--reason cannot be empty"},
		{"assign preview with apply input", []string{
			"assign", "--hub-url", base, "--job", "job-1", "--preview", "--reason", "r", "verifier-1"},
			"--preview creates no writes and does not accept --reason"},
		{"assign json on apply", []string{
			"assign", "--hub-url", base, "--job", "job-1", "--reason", "r",
			"--confirm-name", "onode-peer", "--json", "verifier-1"},
			"--json is only for --preview"},
		{"assign preview with a confirmation", []string{
			"assign", "--hub-url", base, "--job", "job-1", "--preview",
			"--confirm-name", "onode-peer", "verifier-1"},
			"--preview creates no writes and does not accept --confirm-name"},
		{"assign apply without a confirmation", []string{
			"assign", "--hub-url", base, "--job", "job-1", "--reason", "r", "verifier-1"},
			"--confirm-name cannot be empty"},
		{"assign with a path traversal job", []string{
			"assign", "--hub-url", base, "--job", "../etc", "--preview", "verifier-1"},
			"--job cannot contain slashes or dot segments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runVerifierCommandWithDeps(t.Context(), test.argv, &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want %q; stdout=%q", err, test.want, out.String())
			}
			if out.Len() != 0 {
				t.Fatalf("a rejected invocation wrote to stdout: %q", out.String())
			}
		})
	}
}

func verifierCLIRowCount(t *testing.T, f jobsFixture) int {
	t.Helper()
	var rows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM verifiers`).Scan(&rows); err != nil {
		t.Fatalf("count verifiers: %v", err)
	}
	return rows
}

// TestVerifierAssignCLIHTTPStatesTheHandoutRuleAndReceipt walks the operator
// path that makes the first drill possible: name a verifier, name a job, and
// leave a receipt that says what is still owed.
func TestVerifierAssignCLIHTTPStatesTheHandoutRuleAndReceipt(t *testing.T) {
	// Assignment records who asked, so this path needs the authenticated
	// boundary rather than the bare operator mux the read commands use.
	f := observedOperatorFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Operate))
	}))
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	t.Setenv("CLAWCTL_DB", filepath.Join(t.TempDir(), "must-not-be-created.sqlite"))
	jobID := f.newJob(t, verifierAPITestDigest)
	verifier, _ := registerAPITestVerifier(t, f, "onode-peer", "assign-peer")

	var previewOut, previewErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"assign", "--hub-url", base, "--job", jobID, "--preview", verifier.VerifierID,
	}, &previewOut, &previewErr, deps); err != nil {
		t.Fatalf("preview: %v; stdout=%q stderr=%q", err, previewOut.String(), previewErr.String())
	}
	for _, want := range []string{
		`separation_rule: "failure_domain_differs_from_job_machine"`,
		`satisfied_by: "complete_independent_report_received_at_or_after_assignment"`,
		"commands_supplied_by_hub: false",
		"grants_deployment_gate: true",
		"job is still in progress; will be delivered after it ends",
		"preview-digest=sha256:",
	} {
		if !strings.Contains(previewOut.String(), want) {
			t.Fatalf("preview output lacks %q: %q", want, previewOut.String())
		}
	}
	if rows := verifierCLIAssignmentCount(t, f); rows != 0 {
		t.Fatalf("preview created %d assignment rows", rows)
	}

	var out, errOut bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"assign", "--hub-url", base, "--job", jobID,
		"--reason", "第一次跨故障域演練", "--confirm-name", verifier.DisplayName, verifier.VerifierID,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("assign: %v; stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	for _, want := range []string{
		"assigned: verifier ", "pending satisfaction of report conditions fixed by preview",
		"assignment does not determine job outcome, only complete reports from verifiers granting deployment gates participate in stable promotion",
		"idempotency-key=", "preview-digest=sha256:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("receipt lacks %q: %q", want, out.String())
		}
	}
	if rows := verifierCLIAssignmentCount(t, f); rows != 1 {
		t.Fatalf("assign wrote %d assignment rows, want 1", rows)
	}

	// The CLI must never print a command for the verifier to run: the Hub does
	// not decide what "installed" means, so it has nothing to hand over.
	for _, banned := range []string{"ssh", "systemctl", "curl", "--command"} {
		if strings.Contains(previewOut.String()+out.String(), banned) {
			t.Fatalf("派工輸出不可以帶指令 %q：%q / %q", banned, previewOut.String(), out.String())
		}
	}
}

func verifierCLIAssignmentCount(t *testing.T, f jobsFixture) int {
	t.Helper()
	var rows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM verification_assignments`).Scan(&rows); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	return rows
}

func TestVerifierRevokeCLIHTTPReplaysTheSameReceiptForTheDocumentedRetry(t *testing.T) {
	base, deps, _ := verifierCLIServer(t)
	const (
		name   = "retry-revocation-runner"
		reason = "驗證 ambiguous response 的官方重試路徑"
	)

	var registerOut, registerErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"register", "--hub-url", base, "--kind", "external_job_runner",
		"--name", name, "--failure-domain", "retry-domain", "--reason", "建立重試測試 verifier",
	}, &registerOut, &registerErr, deps); err != nil {
		t.Fatalf("register: %v; stdout=%q stderr=%q", err, registerOut.String(), registerErr.String())
	}
	idMatch := regexp.MustCompile(`verifier_id "([A-Za-z0-9_-]+)"`).FindStringSubmatch(registerErr.String())
	if len(idMatch) != 2 {
		t.Fatalf("register receipt lacks verifier_id: stdout=%q stderr=%q", registerOut.String(), registerErr.String())
	}

	var freshOut, freshErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"revoke", "--hub-url", base, "--confirm-name", name, "--reason", reason, idMatch[1],
	}, &freshOut, &freshErr, deps); err != nil {
		t.Fatalf("fresh revoke: %v; stdout=%q stderr=%q", err, freshOut.String(), freshErr.String())
	}
	if !strings.HasPrefix(freshOut.String(), "revoked:") {
		t.Errorf("got first receipt %q, expected prefix %q；第一次撤銷就被標成 replayed，operator 會以為這只是把稍早的結果再拿一次，不會相信自己剛剛真的撤掉了那台 verifier。", freshOut.String(), "revoked:")
	}
	coordinates := regexp.MustCompile(`(?m)^idempotency-key=([^ ]+) preview-digest=([^\n]+)$`).FindStringSubmatch(freshOut.String())
	revision := regexp.MustCompile(`revision ([0-9]+) → [0-9]+`).FindStringSubmatch(freshOut.String())
	if len(coordinates) != 3 || len(revision) != 2 {
		t.Fatalf("fresh receipt lacks retry coordinates: stdout=%q stderr=%q", freshOut.String(), freshErr.String())
	}

	var replayOut, replayErr bytes.Buffer
	if err := runVerifierCommandWithDeps(t.Context(), []string{
		"revoke", "--hub-url", base, "--confirm-name", name, "--reason", reason,
		"--idempotency-key", coordinates[1], "--preview-digest", coordinates[2],
		"--expected-revision", revision[1], idMatch[1],
	}, &replayOut, &replayErr, deps); err != nil {
		t.Fatalf("replayed revoke: %v; stdout=%q stderr=%q", err, replayOut.String(), replayErr.String())
	}
	if !strings.HasPrefix(replayOut.String(), "replayed:") {
		t.Errorf("got retry receipt %q, expected prefix %q；照 --idempotency-key 的說明重送之後，收據仍寫 revoked，operator 會以為自己撤了第二次；他是在不確定第一次有沒有成功的情況下重送的，這正是那個旗標要回答的問題。", replayOut.String(), "replayed:")
	}
}
