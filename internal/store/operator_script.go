package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"sort"
	"strings"
)

type ScriptRunRequest struct {
	ScriptID       string          `json:"script_id"`
	ScriptSHA256   string          `json:"script_sha256"`
	Args           json.RawMessage `json:"args"`
	Targets        []string        `json:"targets"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
	Reason         string          `json:"reason"`
	PreviewDigest  string          `json:"preview_digest,omitempty"`
}
type ScriptRunResult struct {
	PreviewDigest string   `json:"preview_digest"`
	Targets       []string `json:"targets"`
	JobIDs        []string `json:"job_ids"`
	Replayed      bool     `json:"replayed"`
}

func scriptBad(detail string) error { return operatorError("SCRIPT_INVALID", detail) }
func normalizeScript(req ScriptRunRequest) (ScriptRunRequest, scriptcatalog.Entry, error) {
	e, ok := scriptcatalog.Lookup(req.ScriptID)
	if !ok {
		return req, e, scriptBad("unknown script id")
	}
	if req.ScriptSHA256 != e.SHA256 || scriptcatalog.Digest(e.Bytes) != e.SHA256 {
		return req, e, scriptBad("script sha256 mismatch")
	}
	args, err := e.Validate(req.Args)
	if err != nil {
		return req, e, scriptBad(err.Error())
	}
	req.Args = args
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = e.DefaultTimeout
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > e.MaxTimeout {
		return req, e, scriptBad("timeout outside catalog bounds")
	}
	if len(req.Targets) < 1 || len(req.Targets) > 50 {
		return req, e, scriptBad("targets must contain 1 to 50 explicit machine IDs")
	}
	req.Targets = append([]string(nil), req.Targets...)
	sort.Strings(req.Targets)
	for i, id := range req.Targets {
		if !validJobReadStoredIdentifier(id, 128) || (i > 0 && req.Targets[i-1] == id) {
			return req, e, scriptBad("invalid or duplicate target")
		}
	}
	if strings.TrimSpace(req.Reason) != req.Reason || req.Reason == "" || len(req.Reason) > 500 || scriptcatalog.Redact(req.Reason) != req.Reason {
		return req, e, scriptBad("reason must be bounded and contain no credentials")
	}
	return req, e, nil
}
func scriptScope(e scriptcatalog.Entry, a AuditEntry) error {
	if (e.Mode == "write" && (a.AuthCapability == "admin" || strings.HasSuffix(a.AuthCapability, "-admin"))) || (e.Mode == "read" && (a.AuthCapability == "operate" || strings.HasSuffix(a.AuthCapability, "-operate"))) {
		return nil
	}
	return operatorError("SCRIPT_SCOPE_REQUIRED", "script mode requires operate for read or admin for write")
}
func scriptDetail(req ScriptRunRequest, jobs []string) string {
	raw, _ := json.Marshal(struct {
		ID      string   `json:"script_id"`
		SHA     string   `json:"sha256"`
		Args    string   `json:"args_digest"`
		Targets []string `json:"targets"`
		Jobs    []string `json:"job_ids"`
	}{req.ScriptID, req.ScriptSHA256, scriptcatalog.Digest(req.Args), req.Targets, jobs})
	return string(raw)
}

// ScriptRun validates and creates the complete fanout in a single writer
// transaction with idempotency and per-target audit rows. No migration needed.
func (s *Store) ScriptRun(req ScriptRunRequest, apply bool, key string, audit AuditEntry) (out ScriptRunResult, err error) {
	audit.Action = AuditScriptPreview
	if apply {
		audit.Action = AuditScriptApply
	}
	audit.Subject = "script catalog"
	audit.IdempotencyKey = key
	original := req
	req, e, err := normalizeScript(req)
	if err == nil {
		err = scriptScope(e, audit)
	}
	if err != nil {
		audit.OK = false
		audit.Detail = "script request rejected"
		audit.RequestDigest = scriptcatalog.Digest(mustScriptJSON(original))
		if ae := s.RecordAudit(audit); ae != nil {
			return out, ae
		}
		return out, err
	}
	audit.Reason = req.Reason
	audit.Subject = e.ID
	previewReq := req
	previewReq.PreviewDigest = ""
	digest := "sha256:" + scriptcatalog.Digest(mustScriptJSON(previewReq))
	out = ScriptRunResult{PreviewDigest: digest, Targets: req.Targets, JobIDs: []string{}}
	audit.RequestDigest = "sha256:" + scriptcatalog.Digest(mustScriptJSON(req))
	tx, err := s.beginWrite(context.Background(), "script_run")
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	audit.At = s.now().UTC()
	reject := func(detail string) (ScriptRunResult, error) {
		audit.OK = false
		audit.Detail = "script request rejected: " + detail
		if ae := s.recordAuditTx(tx, audit); ae != nil {
			return out, ae
		}
		if ae := tx.Commit(); ae != nil {
			return out, ae
		}
		return out, scriptBad(detail)
	}
	if apply {
		if key == "" || len(key) > 200 {
			return reject("idempotency key required")
		}
		var operation, cachedDigest, response string
		ce := tx.QueryRow(`SELECT operation,request_digest,COALESCE(response_json,'') FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&operation, &cachedDigest, &response)
		if ce == nil {
			if operation != "script_v1" || cachedDigest != audit.RequestDigest {
				return reject("idempotency conflict")
			}
			if json.Unmarshal([]byte(response), &out) != nil {
				return out, errors.New("invalid script receipt")
			}
			out.Replayed = true
			audit.OK = true
			audit.Detail = OperatorIdempotencyReplayPrefix + scriptDetail(req, out.JobIDs)
			if err = s.recordAuditTx(tx, audit); err != nil {
				return out, err
			}
			return out, tx.Commit()
		}
		if !errors.Is(ce, sql.ErrNoRows) {
			return out, ce
		}
		if req.PreviewDigest != digest {
			return reject("preview digest mismatch")
		}
	}
	for _, id := range req.Targets {
		var osName string
		var retired sql.NullString
		var enabled, capable sql.NullBool
		var active int
		ge := tx.QueryRow(`SELECT COALESCE(m.os,''),m.retired_at,c.jobs_enabled,cap.supported,
   (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?))
   FROM machine_registry m LEFT JOIN machine_checkins c ON c.rowid=(SELECT rowid FROM machine_checkins WHERE machine_id=m.machine_id ORDER BY received_at DESC,rowid DESC LIMIT 1)
   LEFT JOIN machine_job_capabilities cap ON cap.machine_id=c.machine_id AND cap.sent_at=c.sent_at AND cap.capability=? WHERE m.machine_id=?`, deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention, scriptcatalog.Kind, id).Scan(&osName, &retired, &enabled, &capable, &active)
		if errors.Is(ge, sql.ErrNoRows) {
			return reject("target not found")
		}
		if ge != nil {
			return out, ge
		}
		if retired.Valid || !enabled.Valid || !enabled.Bool || !capable.Valid || !capable.Bool || !e.AllowsOS(osName) || active != 0 {
			return reject("target unavailable, unsupported, execution disabled or active job")
		}
	}
	if apply {
		spec := scriptcatalog.Spec{Kind: scriptcatalog.Kind, ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: req.Args}
		raw := mustScriptJSON(spec)
		for _, id := range req.Targets {
			desired, revision, ce := createDesiredStateTx(tx, "machine", id, scriptcatalog.Kind, e.ID, string(raw), "operator:"+audit.AuthSubject, audit.At)
			if ce != nil {
				return out, ce
			}
			job, ce := createManagedJobTx(tx, id, desired, revision, NewJob{ArtifactDigest: "sha256:" + scriptcatalog.Digest(raw), ExecutionTimeout: req.TimeoutSeconds}, audit.At)
			if ce != nil {
				return out, ce
			}
			out.JobIDs = append(out.JobIDs, job)
			row := audit
			row.MachineID = id
			row.OK = true
			single := req
			single.Targets = []string{id}
			row.Detail = scriptDetail(single, []string{job})
			if ce = s.recordAuditTx(tx, row); ce != nil {
				return out, ce
			}
		}
		_, err = tx.Exec(`INSERT INTO operator_idempotency (idempotency_key,operation,request_digest,outcome,response_json,created_at) VALUES (?,'script_v1',?,'ok',?,?)`, key, audit.RequestDigest, string(mustScriptJSON(out)), fmtTime(audit.At))
		if err != nil {
			return out, err
		}
	} else {
		// One row per target keeps bounded audit Detail complete even at 50 targets.
		for _, id := range req.Targets {
			row := audit
			row.MachineID = id
			row.OK = true
			row.Detail = scriptDetail(ScriptRunRequest{ScriptID: req.ScriptID, ScriptSHA256: req.ScriptSHA256, Args: req.Args, Targets: []string{id}}, nil)
			if err = s.recordAuditTx(tx, row); err != nil {
				return out, err
			}
		}
	}
	return out, tx.Commit()
}
func mustScriptJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

// recordScriptCompletionTx contains digests and status only, never raw output.
func (s *Store) recordScriptCompletionTx(tx dbTx, jobID string) error {
	var raw, kind, machine, creator, state string
	err := tx.QueryRow(`SELECT d.spec,d.resource_kind,j.machine_id,d.created_by,j.state FROM jobs j JOIN desired_state d ON d.desired_id=j.desired_id WHERE j.job_id=?`, jobID).Scan(&raw, &kind, &machine, &creator, &state)
	if err != nil {
		return err
	}
	if kind != scriptcatalog.Kind {
		return nil
	}
	spec, err := scriptcatalog.Decode([]byte(raw))
	if err != nil {
		return err
	}
	var metadata string
	err = tx.QueryRow(`SELECT command FROM verification_results WHERE job_id=? AND rule_id='script_v1_metadata' AND evidence_role=? ORDER BY rowid DESC LIMIT 1`, jobID, JobVerificationRoleExecutor).Scan(&metadata)
	result := scriptcatalog.Result{ExitCode: -1}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if metadata != "" && (json.Unmarshal([]byte(metadata), &result) != nil || !validLowerSHA256(result.StdoutSHA256) || !validLowerSHA256(result.StderrSHA256) || result.Stdout != "" || result.Stderr != "") {
		return errors.New("invalid script completion metadata")
	}
	detail := struct {
		ScriptID string `json:"script_id"`
		SHA      string `json:"sha256"`
		Args     string `json:"args_digest"`
		Job      string `json:"job_id"`
		State    string `json:"state"`
		Exit     int    `json:"exit_code"`
		Out      string `json:"stdout_sha256"`
		Err      string `json:"stderr_sha256"`
	}{spec.ScriptID, spec.ScriptSHA256, scriptcatalog.Digest(spec.Args), jobID, state, result.ExitCode, result.StdoutSHA256, result.StderrSHA256}
	return s.recordAuditTx(tx, AuditEntry{At: s.now().UTC(), Action: AuditScriptComplete, MachineID: machine, Subject: spec.ScriptID, AuthSubject: strings.TrimPrefix(creator, "operator:"), OK: state == string(deploy.Succeeded), Detail: string(mustScriptJSON(detail))})
}

// terminalJobWrite commits terminal status and script audit together.
func (s *Store) terminalJobWrite(ctx context.Context, label, jobID, query string, args ...any) (sql.Result, error) {
	tx, err := s.beginWrite(ctx, label)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		if err = s.recordScriptCompletionTx(tx, jobID); err != nil {
			return nil, err
		}
	}
	return res, tx.Commit()
}

func (s *Store) scriptEvidenceReady(jobID string) (required, ready bool, err error) {
	var kind, raw, machine string
	err = s.rdb.QueryRow(`SELECT d.resource_kind,d.spec,j.machine_id FROM jobs j JOIN desired_state d ON d.desired_id=j.desired_id WHERE j.job_id=?`, jobID).Scan(&kind, &raw, &machine)
	if err != nil {
		return false, false, err
	}
	if kind != scriptcatalog.Kind {
		return false, false, nil
	}
	spec, err := scriptcatalog.Decode([]byte(raw))
	if err != nil {
		return true, false, nil
	}
	var count int
	err = s.rdb.QueryRow(`SELECT COUNT(*) FROM verification_results WHERE job_id=? AND machine_id=? AND producer_kind=? AND producer_id=? AND evidence_role=? AND authority=? AND rule_id=? AND command=? AND exit_code=0 AND passed=1`, jobID, machine, JobVerificationProducerExecutorAgent, machine, JobVerificationRoleExecutor, JobVerificationAuthorityMachineLease, scriptcatalog.Kind, spec.ScriptID).Scan(&count)
	if err != nil || count == 0 {
		return true, false, err
	}
	var metadata string
	err = s.rdb.QueryRow(`SELECT command FROM verification_results WHERE job_id=? AND machine_id=? AND producer_kind=? AND producer_id=? AND evidence_role=? AND authority=? AND rule_id='script_v1_metadata' AND exit_code=0 AND passed=1 ORDER BY rowid DESC LIMIT 1`, jobID, machine, JobVerificationProducerExecutorAgent, machine, JobVerificationRoleExecutor, JobVerificationAuthorityMachineLease).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return true, false, nil
	}
	if err != nil {
		return true, false, err
	}
	var result scriptcatalog.Result
	ready = json.Unmarshal([]byte(metadata), &result) == nil && validLowerSHA256(result.StdoutSHA256) && validLowerSHA256(result.StderrSHA256) && result.Stdout == "" && result.Stderr == "" && result.ExitCode == 0 && !result.TimedOut && result.DurationMS >= 0
	return true, ready, nil
}
