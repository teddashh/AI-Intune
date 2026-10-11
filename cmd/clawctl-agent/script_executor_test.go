package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"os"
	"runtime"
	"testing"
)

func TestScriptExecutorRejectsHashMismatch(t *testing.T) {
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	raw, _ := json.Marshal(scriptcatalog.Spec{Kind: scriptcatalog.Kind, ScriptID: e.ID, ScriptSHA256: "mismatch", Args: json.RawMessage(`{}`)})
	_, err := (scriptExecutor{}).Run(context.Background(), model.JobResponse{Spec: raw, ResourceKind: scriptcatalog.Kind, ResourceID: e.ID, ExecutionTimeout: 15})
	var rejected *rejectError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected rejection, got %v", err)
	}
}

func TestScriptExecutorEvidenceAndValidation(t *testing.T) {
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	spec := scriptcatalog.Spec{Kind: scriptcatalog.Kind, ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: json.RawMessage(`{}`)}
	raw, _ := json.Marshal(spec)
	job := model.JobResponse{Spec: raw, ResourceKind: scriptcatalog.Kind, ResourceID: e.ID, ExecutionTimeout: 15}
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		evidence, err := (scriptExecutor{}).Run(context.Background(), job)
		if err != nil || len(evidence) != 2 || !evidence[0].Passed {
			t.Fatalf("evidence %+v %v", evidence, err)
		}
		var result scriptcatalog.Result
		if json.Unmarshal([]byte(evidence[1].Command), &result) != nil || len(result.StdoutSHA256) != 64 || result.Stdout != "" || result.Stderr != "" {
			t.Fatal("metadata must contain hashes only")
		}
	}
	for _, change := range []func(*model.JobResponse){func(j *model.JobResponse) { j.ExecutionTimeout = 61 }, func(j *model.JobResponse) { j.Irreversible = true }, func(j *model.JobResponse) { j.ResourceID = "unknown" }, func(j *model.JobResponse) {
		s := spec
		s.Args = json.RawMessage(`{"command":"true"}`)
		j.Spec, _ = json.Marshal(s)
	}} {
		invalid := job
		change(&invalid)
		_, err := (scriptExecutor{}).Run(context.Background(), invalid)
		var rejected *rejectError
		if !errors.As(err, &rejected) {
			t.Fatalf("invalid job accepted: %v", err)
		}
	}
}
