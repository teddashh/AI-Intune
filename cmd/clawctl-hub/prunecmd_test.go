package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type pruneStatusProbeBackend struct {
	status        operator.RetentionStatus
	previewAt     time.Time
	previewPolicy store.OperatorRetentionPolicy
}

func (b *pruneStatusProbeBackend) Status(context.Context, time.Time, store.RetentionPolicy) (operator.RetentionStatus, error) {
	return b.status, nil
}

func (b *pruneStatusProbeBackend) Preview(_ context.Context, at time.Time, policy store.OperatorRetentionPolicy) (operator.RetentionPrunePreview, error) {
	b.previewAt, b.previewPolicy = at, policy
	return operator.RetentionPrunePreview{
		SchemaVersion: 1, EvaluatedAt: at, Policy: policy,
		Counts: []store.PruneCount{}, Confirmation: "DELETE 0 ROWS",
	}, nil
}

func (*pruneStatusProbeBackend) Apply(context.Context, string, operatorclient.RetentionPruneApplyRequest) (store.OperatorPruneResult, error) {
	panic("unexpected retention apply")
}

func pruneCLIFixture(t *testing.T) (jobsFixture, string, pruneCommandDeps, time.Time) {
	t.Helper()
	f := newJobsFixture(t, "retention-cli-machine")
	fixed := time.Now().UTC().Truncate(time.Second)
	for index, age := range []time.Duration{20 * 24 * time.Hour, 18 * 24 * time.Hour, 16 * 24 * time.Hour} {
		at := fixed.Add(-age)
		if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "retention-cli-test",
			BootID: "boot", AgentSeq: int64(index + 1),
		}, at); err != nil {
			t.Fatalf("old checkin: %v", err)
		}
	}
	(&hub{store: f.store, retention: store.DefaultRetention()}).operatorRoutes(f.mux)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	base, machineDeps := machineHTTPTestDeps(t, server)
	machineDeps.discoverHubURL = func() (string, error) { return base, nil }
	return f, base, pruneCommandDeps{machineCommandDeps: machineDeps, now: func() time.Time { return fixed }}, fixed
}

func TestPruneCLIHTTPPreviewApplyAndReplay(t *testing.T) {
	f, base, deps, _ := pruneCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runPruneCommandWithDeps(t.Context(), []string{"--hub-url", base}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%s", err, errOut.String())
	}
	for _, want := range []string{"HTTP operator API preview", "deleted=2", "kept-newest=1", `--confirm "DELETE 2 ROWS"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("preview missing %q: %s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if err := runPruneCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--apply", "--reason", "例行資料保留維護", "--confirm", "DELETE 2 ROWS",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("apply: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "deleted=2") || !strings.Contains(out.String(), "replayed=false") {
		t.Fatalf("apply output=%s", out.String())
	}
	coordinates := regexp.MustCompile(`idempotency-key=([^ ]+) evaluated-at=([^ ]+) expected-revision=([0-9]+) preview-digest=([^ ]+) keep-observations=([^ ]+) keep-checkins=([^ ]+) keep-occupancy=([^\n]+)`).FindStringSubmatch(errOut.String())
	if len(coordinates) != 8 {
		t.Fatalf("private retry coordinates missing: %s", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := runPruneCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--apply", "--reason", "例行資料保留維護", "--confirm", "DELETE 2 ROWS",
		"--idempotency-key", coordinates[1], "--evaluated-at", coordinates[2],
		"--expected-revision", coordinates[3], "--preview-digest", coordinates[4],
		"--keep-observations", coordinates[5], "--keep-checkins", coordinates[6], "--keep-occupancy", coordinates[7],
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("replay: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "replayed=true") || !strings.Contains(out.String(), "deleted=2") {
		t.Fatalf("replay output=%s", out.String())
	}
	var rows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_checkins WHERE machine_id=?`, f.machine.id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("checkin rows=%d err=%v", rows, err)
	}
	entries, err := f.store.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditRetentionPrune}, Limit: 10,
	})
	if err != nil || len(entries.Items) != 2 || entries.Items[0].UserAgent != operatorclient.UserAgent ||
		entries.Items[0].SourceKind != "operator-api" {
		t.Fatalf("retention audit=%+v err=%v", entries, err)
	}
}

func TestPruneCLIJSONPreviewAndAsOfRemainReadOnly(t *testing.T) {
	f, base, deps, fixed := pruneCLIFixture(t)
	var out, errOut bytes.Buffer
	future := fixed.Add(40 * 24 * time.Hour).Format(time.RFC3339)
	if err := runPruneCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--as-of", future, "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("future preview: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), `"schema_version": 1`) || !strings.Contains(out.String(), `"confirmation": "DELETE 2 ROWS"`) {
		t.Fatalf("JSON preview=%s", out.String())
	}
	var logs int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM retention_log`).Scan(&logs); err != nil || logs != 0 {
		t.Fatalf("preview mutated retention log count=%d err=%v", logs, err)
	}
	if err := runPruneCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--as-of", future, "--apply", "--reason", "bad", "--confirm", "DELETE 2 ROWS",
	}, &bytes.Buffer{}, &bytes.Buffer{}, deps); err == nil {
		t.Fatal("--as-of apply was accepted")
	}
}

func TestPruneCLIDefaultPreviewUsesHubClockAndConfiguredPolicy(t *testing.T) {
	hubTime := time.Date(2026, 9, 11, 17, 30, 0, 0, time.UTC)
	hubPolicy, err := store.NewOperatorRetentionPolicy(store.RetentionPolicy{
		Observations: 31 * 24 * time.Hour,
		Checkins:     15 * 24 * time.Hour,
		Occupancy:    401 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &pruneStatusProbeBackend{status: operator.RetentionStatus{
		SchemaVersion: 1, EvaluatedAt: hubTime, Policy: hubPolicy,
	}}
	localPolicy, err := store.NewOperatorRetentionPolicy(store.DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	options := pruneCommandOptions{
		EvaluatedAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		Policy:      localPolicy,
	}
	if err := executePruneCommand(t.Context(), backend, "probe", options, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !backend.previewAt.Equal(hubTime) || backend.previewPolicy != hubPolicy {
		t.Fatalf("preview coordinates at=%s policy=%+v, want Hub at=%s policy=%+v",
			backend.previewAt, backend.previewPolicy, hubTime, hubPolicy)
	}
}

func TestPruneCLIRejectsIncompleteApplyBeforeDiscovery(t *testing.T) {
	var discoveries int
	deps := pruneCommandDeps{machineCommandDeps: machineCommandDeps{
		discoverHubURL: func() (string, error) { discoveries++; return "", nil },
	}, now: time.Now}
	for _, args := range [][]string{
		{"--apply", "--reason", "reason"},
		{"--apply", "--reason", "reason", "--confirm", "DELETE 1 ROWS", "--idempotency-key", "key"},
		{"--reason", "preview does not consume reason"},
		{"--hub-url", "", "--db", "/tmp/nope"},
	} {
		if err := runPruneCommandWithDeps(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}, deps); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	if discoveries != 0 {
		t.Fatalf("invalid prune attempted discovery %d times", discoveries)
	}
}

func TestPruneCLIRetryRefusesANonWholeSecondEvaluationBeforeDiscovery(t *testing.T) {
	var discoveries int
	deps := pruneCommandDeps{machineCommandDeps: machineCommandDeps{
		discoverHubURL: func() (string, error) { discoveries++; return "", nil },
	}, now: time.Now}
	err := runPruneCommandWithDeps(t.Context(), []string{
		"--apply", "--reason", "retry retention", "--confirm", "DELETE 1 ROWS",
		"--idempotency-key", "retention-key", "--evaluated-at", "2026-09-14T00:00:00.5Z",
		"--expected-revision", "4", "--preview-digest", "sha256:" + strings.Repeat("a", 64),
		"--keep-observations", "720h", "--keep-checkins", "336h", "--keep-occupancy", "9600h",
	}, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err == nil {
		t.Errorf("retry 接受了帶小數秒的 evaluated_at；截斷會改變 digest 輸入，使重放不再是重放")
	}
	if discoveries != 0 {
		t.Errorf("帶小數秒的 retry 在拒絕前 discovery %d 次，want 0", discoveries)
	}
}
