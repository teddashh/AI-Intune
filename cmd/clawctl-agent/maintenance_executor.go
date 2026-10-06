package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/model"
)

// maintenanceConfMutator is a test seam. Production leaves it nil. A test may
// set it to change the conf after it is written and before the digest check.
var maintenanceConfMutator func(path string)

// maintenanceExecutor runs the embedded disk-clean script. The job spec
// carries no shell and no operator command. Dry-run is job.Irreversible==false:
// the agent always passes --dry-run in that case. An irreversible job does not
// pass the flag, so the rendered conf decides apply versus dry-run.
type maintenanceExecutor struct {
	now  func() time.Time
	euid func() int
	dir  string
	run  func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

func (e maintenanceExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	spec, err := maintenance.ParseSpec(job.Spec)
	if err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean spec refused: " + err.Error()}
	}
	if job.ResourceKind != maintenance.ResourceKind || job.ResourceID != maintenance.ResourceID {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean job resource is not maintenance/disk-clean"}
	}
	if job.ArtifactDigest != spec.ConfigDigest {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "artifact_digest does not match config_digest"}
	}
	if spec.Scope == maintenance.ScopeRoot {
		euid := currentEUID
		if e.euid != nil {
			euid = e.euid
		}
		if euid() != 0 {
			return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean --scope root requires the agent process to run as root; this agent is not root and will not use sudo"}
		}
	}
	dir := e.dir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil || cache == "" {
			home := os.Getenv("HOME")
			if home == "" {
				return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean has no private cache directory"}
			}
			cache = filepath.Join(home, ".cache")
		}
		dir = filepath.Join(cache, "clawctl", "maintenance")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean directory could not be created: " + err.Error()}
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean directory is not private: " + err.Error()}
	}
	scriptPath := filepath.Join(dir, "disk-clean")
	confPath := filepath.Join(dir, "disk-clean.conf")
	script := maintenance.ScriptBytes()
	if err := writeModeFile(scriptPath, script, 0o700); err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean script could not be written: " + err.Error()}
	}
	written, err := os.ReadFile(scriptPath)
	if err != nil || !bytes.Equal(written, script) {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean script bytes do not match the embedded copy"}
	}
	conf := []byte(spec.Conf)
	if err := writePrivateFile(confPath, conf); err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean conf could not be written: " + err.Error()}
	}
	if maintenanceConfMutator != nil {
		maintenanceConfMutator(confPath)
	}
	got, err := os.ReadFile(confPath)
	if err != nil || !bytes.Equal(got, conf) {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean conf bytes do not match the job spec"}
	}
	sum := sha256.Sum256(got)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != spec.ConfigDigest {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "disk-clean conf digest does not match config_digest"}
	}
	args := []string{"--scope", spec.Scope, "--conf", confPath}
	if !job.Irreversible {
		args = append(args, "--dry-run")
	}
	run := e.run
	if run == nil {
		run = runMaintenance
	}
	timeout := job.ExecutionTimeout
	if timeout <= 0 {
		timeout = maintenance.ExecutionTimeoutSeconds
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	stdout, stderr, runErr := run(runCtx, scriptPath, args...)
	exit := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exit = exitErr.ExitCode()
			runErr = nil
		} else {
			exit = -1
		}
	}
	line := lastNonEmptyLine(stdout)
	summary, parseErr := maintenance.ParseSummary([]byte(line))
	passed := runErr == nil && exit == 0 && parseErr == nil && summary.ConfigDigest == spec.ConfigDigest
	if !job.Irreversible && (parseErr != nil || summary.Mode != "dry-run") {
		passed = false
	}
	// An apply job must report the mode its rendered conf implies; anything
	// else means the script did not honour the revision the Hub approved.
	if job.Irreversible && (parseErr != nil || summary.Mode != spec.Profile.ExpectedApplyMode()) {
		passed = false
	}
	excerpt := line
	if parseErr != nil {
		excerpt = truncateEvidence(stdout, 8<<10)
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	return []model.JobVerificationRequest{{
		RuleID:        maintenance.VerificationRuleID,
		Command:       scriptPath + " " + strings.Join(args, " "),
		ExitCode:      exit,
		StdoutExcerpt: excerpt,
		StderrExcerpt: truncateEvidence(stderr, 4<<10),
		Passed:        passed,
		VerifiedAt:    now().UTC(),
	}}, nil
}

func runMaintenance(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func writeModeFile(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".clawctl-maintenance-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func truncateEvidence(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
