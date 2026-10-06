package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	maxJobPostAttempts   = 3
	executorKindNoop     = "noop"
	journalSchemaVersion = 2
	maxJournalBytes      = 1 << 20
	maxJournalKinds      = 256
	maxJournalResources  = 4096
	// orphanStagingSweepTimeout 留給 systemd 預設 90 秒的 scope stop 一整輪；一個 scope
	// 最多等這一輪，超過兩分鐘就先啟動工作單迴圈，殘骸留待下次啟動再掃。
	orphanStagingSweepTimeout = 2 * time.Minute
)

var errUnsupported = errors.New("executor 不支援這種工作單")

type executor interface {
	Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error)
}

type retainer interface {
	// AfterSucceeded 只在 Hub 回 succeeded、MaxApplied 寫完後才會被叫；
	// failed、manual_intervention 與 rejected 一律不叫。
	AfterSucceeded(ctx context.Context, job model.JobResponse) []string
}

type kindExecutor struct {
	executors map[string]executor
}

func newKindExecutor(noop, deviceSync, openclaw, nodeRuntime, hermes executor) kindExecutor {
	return kindExecutor{executors: map[string]executor{
		executorKindNoop:                     noop,
		model.DeviceSyncJobKind:              deviceSync,
		agentadapter.ExecutorKindOpenClaw:    openclaw,
		agentadapter.ExecutorKindNodeRuntime: nodeRuntime,
		agentadapter.ExecutorKindHermes:      hermes,
	}}
}

func (e kindExecutor) withKind(kind string, exec executor) kindExecutor {
	next := make(map[string]executor, len(e.executors)+1)
	for key, value := range e.executors {
		next[key] = value
	}
	next[kind] = exec
	return kindExecutor{executors: next}
}

func (e kindExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	if err := validateUniqueJSONFields(job.Spec); err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "工作單 spec 欄位不可重複：" + err.Error()}
	}
	kind, _, err := model.ParseJobSpec(job.Spec)
	if err != nil {
		return nil, &rejectError{Code: deploy.PreconditionFailed, Detail: "工作單 spec 不是合法 JSON：" + err.Error()}
	}
	selected, ok := e.executors[kind]
	if !ok || selected == nil {
		return nil, fmt.Errorf("%w：沒有 kind=%q 的 executor", errUnsupported, kind)
	}
	return selected.Run(ctx, job)
}

func (e kindExecutor) AfterSucceeded(ctx context.Context, job model.JobResponse) []string {
	kind, _, err := model.ParseJobSpec(job.Spec)
	if err != nil {
		return nil
	}
	selected, ok := e.executors[kind]
	if !ok || selected == nil {
		return nil
	}
	retain, ok := selected.(retainer)
	if !ok {
		return nil
	}
	return retain.AfterSucceeded(ctx, job)
}

// noopExecutor 只驗證整圈協定，不變更 OpenClaw、systemd 或機器上的任何設定。
type noopExecutor struct {
	now func() time.Time
}

func (e noopExecutor) Run(_ context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	var spec struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		return nil, fmt.Errorf("%w：noop spec 不是合法 JSON：%v", errUnsupported, err)
	}
	if spec.Kind != "noop" {
		return nil, fmt.Errorf("%w：noop executor 不接受 kind=%q", errUnsupported, spec.Kind)
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	return []model.JobVerificationRequest{{
		RuleID:        "noop",
		Command:       "true",
		ExitCode:      0,
		StdoutExcerpt: "工作單驗證完成。",
		Passed:        true,
		VerifiedAt:    now().UTC(),
	}}, nil
}

// deviceSyncExecutor accepts only the fixed v1 primitive. It does not run a
// command or claim that fresh evidence already exists: jobsRunner schedules
// the existing observation loop only after the Hub accepts the terminal job.
type deviceSyncExecutor struct {
	now func() time.Time
}

func (e deviceSyncExecutor) Run(_ context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	if job.ResourceKind != model.DeviceSyncResourceKind || job.ResourceID != model.DeviceSyncResourceID ||
		job.Irreversible || job.ExecutionTimeout < model.DeviceSyncMinTimeoutSeconds ||
		job.ExecutionTimeout > model.DeviceSyncMaxTimeoutSeconds {
		return nil, fmt.Errorf("%w：device-sync 只接受固定 resource、可逆標記與 timeout", errUnsupported)
	}
	var spec model.DeviceSyncSpec
	decoder := json.NewDecoder(bytes.NewReader(job.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("%w：device-sync spec 不是合法 v1 文件：%v", errUnsupported, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w：device-sync spec 帶有第二份 JSON", errUnsupported)
	}
	if spec.Kind != model.DeviceSyncJobKind || spec.SchemaVersion != model.DeviceSyncSpecSchemaVersion {
		return nil, fmt.Errorf("%w：device-sync 只接受固定 v1 spec", errUnsupported)
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	return []model.JobVerificationRequest{{
		RuleID:        model.DeviceSyncVerificationRuleID,
		Command:       model.DeviceSyncVerificationCommand,
		ExitCode:      0,
		StdoutExcerpt: model.DeviceSyncVerificationStdout,
		Passed:        true,
		VerifiedAt:    now().UTC(),
	}}, nil
}

// journalPath 與 statePath 使用相同的 cache 根目錄，但水位必須獨立可靠落地。
func journalPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(dir, "clawctl", "agent-journal.json")
}

type journalWatermarks struct {
	MaxSeen    deploy.Revision `json:"max_seen"`
	MaxApplied deploy.Revision `json:"max_applied"`
}

type journalDocument struct {
	SchemaVersion int                                     `json:"schema_version,omitempty"`
	Resources     map[string]map[string]journalWatermarks `json:"resources"`
	MaxSeen       *deploy.Revision                        `json:"max_seen,omitempty"`
	MaxApplied    *deploy.Revision                        `json:"max_applied,omitempty"`
}

type resourceScope struct {
	Kind string
	ID   string
}

type watermarkJournal struct {
	resources map[resourceScope]deploy.Watermarks
}

var legacyOpenClawScope = resourceScope{
	Kind: agentadapter.ExecutorKindOpenClaw,
	ID:   agentadapter.ExecutorKindOpenClaw,
}

func newWatermarkJournal() watermarkJournal {
	return watermarkJournal{resources: make(map[resourceScope]deploy.Watermarks)}
}

func (j watermarkJournal) get(scope resourceScope) deploy.Watermarks {
	return j.resources[scope]
}

func (j *watermarkJournal) set(scope resourceScope, watermarks deploy.Watermarks) {
	if j.resources == nil {
		j.resources = make(map[resourceScope]deploy.Watermarks)
	}
	j.resources[scope] = watermarks
}

func validWatermarkIdentity(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func validResourceScope(scope resourceScope) bool {
	return validWatermarkIdentity(scope.Kind, 128) && validWatermarkIdentity(scope.ID, 256)
}

func validJournalWatermarks(watermarks deploy.Watermarks) bool {
	return watermarks.MaxSeen >= 0 && watermarks.MaxApplied >= 0 &&
		watermarks.MaxApplied <= watermarks.MaxSeen
}

func finishOpenJournal(file *os.File) (*os.File, error) {
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("水位日誌不是一般檔案")
	}
	if info.Size() > maxJournalBytes {
		_ = file.Close()
		return nil, fmt.Errorf("水位日誌超過 %d bytes", maxJournalBytes)
	}
	return file, nil
}

func validateUniqueJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key 不是字串")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("重複的 JSON 欄位 %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim('}') {
				return errors.New("JSON object 沒有正確結束")
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim(']') {
				return errors.New("JSON array 沒有正確結束")
			}
		default:
			return errors.New("JSON 結構不合法")
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON 含有多餘內容")
		}
		return err
	}
	return nil
}

func hasExactJournalFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func loadWatermarkJournal(path string) (watermarkJournal, error) {
	journal := newWatermarkJournal()
	file, err := openJournal(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return watermarkJournal{}, err
	}
	defer file.Close()

	b, err := io.ReadAll(io.LimitReader(file, maxJournalBytes+1))
	if err != nil {
		return watermarkJournal{}, err
	}
	if len(b) > maxJournalBytes {
		return watermarkJournal{}, fmt.Errorf("水位日誌超過 %d bytes", maxJournalBytes)
	}
	if err := validateUniqueJSONFields(b); err != nil {
		return watermarkJournal{}, fmt.Errorf("水位日誌不是唯一 JSON 結構：%w", err)
	}
	var doc journalDocument
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return watermarkJournal{}, fmt.Errorf("水位日誌不是有效 JSON：%w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return watermarkJournal{}, errors.New("水位日誌含有多餘 JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil || fields == nil {
		return watermarkJournal{}, errors.New("水位日誌頂層不是 JSON object")
	}

	switch doc.SchemaVersion {
	case 0:
		if !hasExactJournalFields(fields, "max_seen", "max_applied") ||
			doc.Resources != nil || doc.MaxSeen == nil || doc.MaxApplied == nil {
			return watermarkJournal{}, errors.New("舊版水位日誌結構不完整")
		}
		watermarks := deploy.Watermarks{MaxSeen: *doc.MaxSeen, MaxApplied: *doc.MaxApplied}
		if !validJournalWatermarks(watermarks) {
			return watermarkJournal{}, errors.New("舊版水位日誌數值不合法")
		}
		journal.set(legacyOpenClawScope, watermarks)
		return journal, nil
	case journalSchemaVersion:
		if !hasExactJournalFields(fields, "schema_version", "resources") ||
			doc.MaxSeen != nil || doc.MaxApplied != nil || doc.Resources == nil {
			return watermarkJournal{}, errors.New("水位日誌 v2 結構不完整")
		}
		if len(doc.Resources) > maxJournalKinds {
			return watermarkJournal{}, fmt.Errorf("水位日誌超過 %d 種 resource kind", maxJournalKinds)
		}
		resourceCount := 0
		for kind, resources := range doc.Resources {
			if !validWatermarkIdentity(kind, 128) || len(resources) == 0 {
				return watermarkJournal{}, errors.New("水位日誌含有不合法的 resource kind")
			}
			resourceCount += len(resources)
			if resourceCount > maxJournalResources {
				return watermarkJournal{}, fmt.Errorf("水位日誌超過 %d 個 resource", maxJournalResources)
			}
			for id, stored := range resources {
				scope := resourceScope{Kind: kind, ID: id}
				watermarks := deploy.Watermarks{MaxSeen: stored.MaxSeen, MaxApplied: stored.MaxApplied}
				if !validResourceScope(scope) || !validJournalWatermarks(watermarks) {
					return watermarkJournal{}, errors.New("水位日誌含有不合法的 resource 水位")
				}
				journal.set(scope, watermarks)
			}
		}
		return journal, nil
	default:
		return watermarkJournal{}, fmt.Errorf("不支援的水位日誌 schema_version %d", doc.SchemaVersion)
	}
}

func saveWatermarkJournal(path string, journal watermarkJournal) error {
	if len(journal.resources) > maxJournalResources {
		return fmt.Errorf("水位日誌超過 %d 個 resource", maxJournalResources)
	}
	kindSet := make(map[string]struct{})
	scopes := make([]resourceScope, 0, len(journal.resources))
	for scope, watermarks := range journal.resources {
		if !validResourceScope(scope) || !validJournalWatermarks(watermarks) {
			return errors.New("水位日誌含有不合法的 resource 水位")
		}
		kindSet[scope.Kind] = struct{}{}
		scopes = append(scopes, scope)
	}
	if len(kindSet) > maxJournalKinds {
		return fmt.Errorf("水位日誌超過 %d 種 resource kind", maxJournalKinds)
	}
	sort.Slice(scopes, func(i, k int) bool {
		if scopes[i].Kind == scopes[k].Kind {
			return scopes[i].ID < scopes[k].ID
		}
		return scopes[i].Kind < scopes[k].Kind
	})
	resources := make(map[string]map[string]journalWatermarks, len(kindSet))
	for _, scope := range scopes {
		if resources[scope.Kind] == nil {
			resources[scope.Kind] = make(map[string]journalWatermarks)
		}
		watermarks := journal.resources[scope]
		resources[scope.Kind][scope.ID] = journalWatermarks{
			MaxSeen: watermarks.MaxSeen, MaxApplied: watermarks.MaxApplied,
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(journalDocument{
		SchemaVersion: journalSchemaVersion,
		Resources:     resources,
	}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	f, err := os.CreateTemp(dir, ".agent-journal-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	keep = true
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// loadJournal 與 saveJournal 保留舊測試與保留期執行器的單資源介面。
func loadJournal(path string) (deploy.Watermarks, error) {
	journal, err := loadWatermarkJournal(path)
	if err != nil {
		return deploy.Watermarks{}, err
	}
	return journal.get(legacyOpenClawScope), nil
}

func saveJournal(path string, watermarks deploy.Watermarks) error {
	journal := newWatermarkJournal()
	journal.set(legacyOpenClawScope, watermarks)
	return saveWatermarkJournal(path, journal)
}

type jobsOptions struct {
	HubURL       string
	Token        string
	JournalPath  string
	PollInterval time.Duration
	Executor     executor
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	RetryBackoff func(attempt int) time.Duration
	PollJitter   func(time.Duration) time.Duration
	Nudge        chan<- struct{}
}

type jobsRunner struct {
	hubURL       string
	token        string
	journalPath  string
	pollInterval time.Duration
	executor     executor
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
	retryBackoff func(attempt int) time.Duration
	pollJitter   func(time.Duration) time.Duration
	nudge        chan<- struct{}
	watermarks   watermarkJournal
	// pollFailures counts consecutive GET /v1/jobs/next failures that use the
	// exponential delay. A successful poll (200 or 204) resets it.
	pollFailures int
}

func startJobs(ctx context.Context, cfg config, nudge chan<- struct{}) {
	if !cfg.JobsEnabled {
		log.Printf("工作單迴圈未啟用（jobs_enabled=false）")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	// ⚠ 啟動時不可能有活著的工作單：agent 自己的工作會隨 agent 死亡而中止，
	// Hub 也不會讓重啟後的 agent 再領同一張；因此停掉所有 clawctl-stage-* 是安全的。
	sweep := func(sweepCtx context.Context) []string {
		return sweepOrphanStaging(sweepCtx, home, "", realSystemctl)
	}
	launchJobs(ctx, sweep, jobsOptions{
		HubURL:       cfg.HubURL,
		Token:        cfg.AgentToken,
		JournalPath:  journalPath(),
		PollInterval: dur(cfg.JobsPollIntervalSeconds, 30*time.Second),
		Nudge:        nudge,
	}, runJobs)
}

// launchJobs 先掃孤兒（最多 orphanStagingSweepTimeout），掃完才起工作單迴圈；兩件事都在背景。
//
// ⚠ 不能在呼叫者的 goroutine 裡同步等掃描：startJobs 跑在 runAgent 送 READY=1 之前，
// 而 unit 是 Type=notify、沒設 TimeoutStartSec（systemd 預設 90 秒）。一個 scope 的 stop
// 最長就要 90 秒，同步等會讓 agent 在啟動時被 systemd 打死，四台一起進重啟迴圈。
// 順序（掃完才領單）仍然成立，因為兩步在同一條 goroutine 裡。
func launchJobs(ctx context.Context, sweep func(context.Context) []string, opts jobsOptions,
	run func(context.Context, jobsOptions)) {
	go func() {
		sweepCtx, cancel := context.WithTimeout(ctx, orphanStagingSweepTimeout)
		for _, report := range sweep(sweepCtx) {
			log.Printf("%s", report)
		}
		cancel()
		run(ctx, opts)
	}()
}

func runJobs(ctx context.Context, opts jobsOptions) {
	r, err := newJobsRunner(opts)
	if err != nil {
		// ⚠ 日誌讀壞時整條迴圈不准啟動；當成零水位會容許降版。
		log.Printf("工作單迴圈未啟動：%v", err)
		return
	}
	r.run(ctx)
}

func newJobsRunner(opts jobsOptions) (*jobsRunner, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepWithContext
	}
	if opts.RetryBackoff == nil {
		// Full jitter over 500ms * 2^(attempt-1), capped at 30s. post applies
		// the Retry-After floor on top of whatever this returns.
		opts.RetryBackoff = defaultJobPostBackoff
	}
	if opts.PollJitter == nil {
		opts.PollJitter = jitter
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 30 * time.Second
	}
	if opts.JournalPath == "" {
		opts.JournalPath = journalPath()
	}
	if opts.Executor == nil {
		openclaw := defaultOpenClawExecutor(opts.HubURL, opts.Token)
		openclaw.deps.now = opts.Now
		nodeRuntime := defaultNodeRuntimeExecutor(opts.HubURL, opts.Token)
		nodeRuntime.deps.now = opts.Now
		hermes := defaultHermesExecutor(opts.HubURL, opts.Token)
		hermes.deps.now = opts.Now
		kinds := newKindExecutor(noopExecutor{now: opts.Now}, deviceSyncExecutor{now: opts.Now},
			openclaw, nodeRuntime, hermes)
		kinds.executors[maintenance.JobKind] = maintenanceExecutor{}
		opts.Executor = kinds
	}
	w, err := loadWatermarkJournal(opts.JournalPath)
	if err != nil {
		return nil, fmt.Errorf("讀取水位日誌 %s 失敗：%w", opts.JournalPath, err)
	}
	return &jobsRunner{
		hubURL:       strings.TrimRight(opts.HubURL, "/"),
		token:        opts.Token,
		journalPath:  opts.JournalPath,
		pollInterval: opts.PollInterval,
		executor:     opts.Executor,
		now:          opts.Now,
		sleep:        opts.Sleep,
		retryBackoff: opts.RetryBackoff,
		pollJitter:   opts.PollJitter,
		nudge:        opts.Nudge,
		watermarks:   w,
	}, nil
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *jobsRunner) run(ctx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := r.next(ctx)
		if err != nil {
			log.Printf("拉工作單失敗：%v", err)
			var sleepErr error
			if jobPollShouldBackOff(err) {
				r.pollFailures++
				sleepErr = r.pollAfterError(ctx, err)
			} else {
				sleepErr = r.poll(ctx)
			}
			if sleepErr != nil {
				return
			}
			continue
		}
		r.pollFailures = 0
		if !ok {
			if r.poll(ctx) != nil {
				return
			}
			continue
		}

		wait, claimed := r.claim(ctx, job)
		if wait > 0 {
			if r.sleep(ctx, wait) != nil {
				return
			}
			continue
		}
		if claimed == nil {
			continue
		}
		r.process(ctx, job, *claimed)
		// 有單時不睡；立刻再拉，直到 Hub 回 204 才進 poll 間隔。
	}
}

func (r *jobsRunner) poll(ctx context.Context) error {
	return r.sleep(ctx, r.pollJitter(r.pollInterval))
}

// pollAfterError waits out a failed GET /v1/jobs/next. The delay is the
// jittered exponential starting at the poll interval (capped at 5 minutes),
// and never shorter than a parsed Retry-After.
func (r *jobsRunner) pollAfterError(ctx context.Context, err error) error {
	delay := r.pollJitter(pollBackoffCeiling(r.pollInterval, r.pollFailures))
	if delay > pollBackoffCap {
		delay = pollBackoffCap
	}
	if delay < 0 {
		delay = 0
	}
	if ra := honoredRetryAfter(err); ra > delay {
		delay = ra
	}
	return r.sleep(ctx, delay)
}

func (r *jobsRunner) next(ctx context.Context) (model.JobResponse, bool, error) {
	var job model.JobResponse
	status, err := doJSONStatus(ctx, http.MethodGet, r.hubURL+"/v1/jobs/next", r.token, nil, &job)
	if err != nil {
		return job, false, err
	}
	switch status {
	case http.StatusNoContent:
		return job, false, nil
	case http.StatusOK:
		if job.JobID == "" {
			return job, false, errors.New("Hub 的 200 工作單沒有 job_id")
		}
		return job, true, nil
	default:
		return job, false, fmt.Errorf("GET /v1/jobs/next 回了未預期的 HTTP %d", status)
	}
}

type claimResponse struct {
	model.JobLeaseResponse
	model.JobStateResponse
}

func (r *jobsRunner) claim(ctx context.Context, job model.JobResponse) (time.Duration, *model.JobLeaseResponse) {
	var response claimResponse
	err := r.post(ctx, r.jobURL(job.JobID, "/claims"), model.JobClaimRequest{}, &response, http.StatusOK)
	if err == nil {
		if response.Replayed {
			return 0, nil
		}
		if err := validateJobLeaseReceipt(response.JobLeaseResponse, "", r.now()); err != nil {
			log.Printf("工作單 %s 認領回應不完整，放棄這一輪：%v", job.JobID, err)
			return 0, nil
		}
		lease := response.JobLeaseResponse
		return 0, &lease
	}
	var httpErr *hubHTTPError
	if !errors.As(err, &httpErr) {
		log.Printf("認領工作單 %s 失敗：%v", job.JobID, err)
		return 0, nil
	}
	if httpErr.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if httpErr.StatusCode == http.StatusConflict && httpErr.APIError.Code == model.ErrJobStateConflict {
		var stateErr model.JobStateError
		if json.Unmarshal(httpErr.Body, &stateErr) == nil && stateErr.LeaseExpiresAt != nil {
			wait := stateErr.LeaseExpiresAt.Sub(r.now())
			if wait <= 0 {
				return 0, nil
			}
			if wait > r.pollInterval {
				wait = r.pollInterval
			}
			return wait, nil
		}
	}
	log.Printf("認領工作單 %s 失敗：%v", job.JobID, err)
	return 0, nil
}

func validateJobLeaseReceipt(receipt model.JobLeaseResponse, expectedToken string, now time.Time) error {
	if receipt.LeaseToken == "" || receipt.LeaseToken != strings.TrimSpace(receipt.LeaseToken) ||
		strings.ContainsAny(receipt.LeaseToken, "\r\n\t ") || len(receipt.LeaseToken) > 200 {
		return errors.New("缺少或不合法的 lease_token")
	}
	if expectedToken != "" && receipt.LeaseToken != expectedToken {
		return errors.New("lease_token 與目前租約不符")
	}
	if receipt.LeaseExpiresAt.IsZero() || !receipt.LeaseExpiresAt.After(now) {
		return errors.New("lease_expires_at 缺失或已到期")
	}
	return nil
}

func (r *jobsRunner) process(ctx context.Context, job model.JobResponse, lease model.JobLeaseResponse) {
	scope := resourceScope{Kind: job.ResourceKind, ID: job.ResourceID}
	if !validResourceScope(scope) {
		r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed,
			"工作單 resource identity 不合法")
		return
	}
	revision := deploy.Revision(job.Revision)
	currentWatermarks := r.watermarks.get(scope)
	admission, nextWatermarks := deploy.Admit(currentWatermarks, revision)
	if !admission.Accepted {
		detail := fmt.Sprintf("%s/%s 已見過 revision %d，這張是 %d",
			scope.Kind, scope.ID, currentWatermarks.MaxSeen, revision)
		r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.StaleRevision, detail)
		return
	}
	// ⚠ MaxSeen 在「看見」就前進 —— 記憶體先前進，再落地。
	// 落地失敗這張單會被拒，但同一個 process 裡不能因為磁碟壞了就忘記看過 43：
	// 否則磁碟一好，42 進來會被當成新的執行掉。重啟後以磁碟為準，那是拒單的理由。
	r.watermarks.set(scope, nextWatermarks)
	// ⚠ MaxSeen 必須先可靠落地才可執行；吞掉寫入錯誤會讓重啟後的 43→42 降版成功。
	if err := saveWatermarkJournal(r.journalPath, r.watermarks); err != nil {
		detail := fmt.Sprintf("水位日誌寫不進去：%v", err)
		r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed, detail)
		return
	}

	// ⚠ Hub 沒釘 digest 的單不准進 executor；否則產物可在 activation 前被掉包。
	if job.ArtifactDigest == "" {
		r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed,
			"Hub 沒有釘 artifact digest")
		return
	}
	_, artifact, err := model.ParseJobSpec(job.Spec)
	if err != nil {
		r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed,
			"工作單 spec 不是合法 JSON："+err.Error())
		return
	}
	if artifact != nil && artifact.SHA256 != "" {
		if job.ArtifactDigest != "sha256:"+artifact.SHA256 {
			// ⚠ 擋的是工作單 digest 與 spec 指向不同 artifact：若放行，下一刀的
			// executor 可能驗 A 的 digest、卻依 spec 啟用 B 的 tarball。
			r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed,
				"單上釘的 digest 跟 spec 宣告的 artifact 不一致")
			return
		}
	} else {
		sum := sha256.Sum256(job.Spec)
		expected := "sha256:" + hex.EncodeToString(sum[:])
		if job.ArtifactDigest != expected {
			// ⚠ 擋的是無 artifact 工作單在 Hub 編碼後內容已變，卻仍沿用舊 digest；
			// 這會讓 agent 對一份沒有被 Hub 釘住的 spec 動作。
			r.reject(ctx, job.JobID, lease.LeaseToken, 1, deploy.PreconditionFailed,
				"沒有 artifact 的單，digest 該是 spec 本身的 sha256，對不上")
			return
		}
	}

	jobCtx, cancelJob := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(jobCtx, cancelJob, job.JobID, lease)
	}()
	defer func() {
		cancelJob()
		<-renewDone
	}()

	seq := 1
	start := model.JobEventRequest{
		LeaseToken: lease.LeaseToken,
		Seq:        seq,
		Phase:      "start",
		Payload:    json.RawMessage(`{}`),
		OccurredAt: r.now().UTC(),
	}
	if err := r.post(jobCtx, r.jobURL(job.JobID, "/events"), start, &model.JobEventResponse{}, http.StatusAccepted); err != nil {
		log.Printf("工作單 %s start 事件送不出去，放棄這張單：%v", job.JobID, err)
		return
	}

	execCtx := jobCtx
	cancelExec := func() {}
	if job.ExecutionTimeout > 0 {
		execCtx, cancelExec = context.WithTimeout(jobCtx, time.Duration(job.ExecutionTimeout)*time.Second)
	}
	verifications, runErr := r.executor.Run(execCtx, job)
	cancelExec()
	if jobCtx.Err() != nil {
		log.Printf("工作單 %s 的租約續租失敗或迴圈停止，executor 已取消", job.JobID)
		return
	}
	if errors.Is(runErr, errUnsupported) {
		r.reject(jobCtx, job.JobID, lease.LeaseToken, seq+1, deploy.PreconditionFailed, runErr.Error())
		return
	}
	var rejected *rejectError
	if errors.As(runErr, &rejected) {
		r.reject(jobCtx, job.JobID, lease.LeaseToken, seq+1, rejected.Code, rejected.Detail)
		return
	}
	if runErr != nil {
		verifications = append(verifications, model.JobVerificationRequest{
			RuleID:        "executor",
			StderrExcerpt: excerpt(runErr.Error(), 4<<10),
			Passed:        false,
			VerifiedAt:    r.now().UTC(),
		})
	}

	for i := range verifications {
		verifications[i].LeaseToken = lease.LeaseToken
		if verifications[i].VerifiedAt.IsZero() {
			verifications[i].VerifiedAt = r.now().UTC()
		}
		if err := r.post(jobCtx, r.jobURL(job.JobID, "/verifications"), verifications[i], nil, http.StatusCreated); err != nil {
			log.Printf("工作單 %s 第 %d 筆驗證送不出去，放棄這張單：%v", job.JobID, i+1, err)
			return
		}
	}

	seq++
	finish := model.JobEventRequest{
		LeaseToken: lease.LeaseToken,
		Seq:        seq,
		Phase:      "finish",
		Payload:    json.RawMessage(`{}`),
		OccurredAt: r.now().UTC(),
	}
	if err := r.post(jobCtx, r.jobURL(job.JobID, "/events"), finish, &model.JobEventResponse{}, http.StatusAccepted); err != nil {
		log.Printf("工作單 %s finish 事件送不出去，放棄這張單：%v", job.JobID, err)
		return
	}

	var state model.JobStateResponse
	err = r.post(jobCtx, r.jobURL(job.JobID, "/complete"),
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken}, &state, http.StatusOK)
	if err != nil {
		var httpErr *hubHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusConflict {
			switch httpErr.APIError.Code {
			case model.ErrLeaseInvalid:
				log.Printf("工作單 %s complete 時租約已失效；回到拉單", job.JobID)
				return
			case model.ErrNoVerification:
				log.Printf("錯誤：工作單 %s complete 回 NO_VERIFICATION；agent 明明已送驗證", job.JobID)
				return
			}
		}
		log.Printf("工作單 %s complete 失敗：%v", job.JobID, err)
		return
	}
	// Hub 已確認這張單到終態；現在就排一輪觀測，不讓切換前的舊安裝事實
	// 繼續掛到原本的十分鐘週期。清理保留物不影響這個通知。
	r.nudgeObservation()

	switch deploy.JobState(state.State) {
	case deploy.Succeeded:
		r.watermarks.set(scope, deploy.Applied(r.watermarks.get(scope), revision))
		if err := saveWatermarkJournal(r.journalPath, r.watermarks); err != nil {
			// ⚠ Hub 已成功後不能倒打一耙；保留記憶體 MaxApplied，讓下一次成功寫日誌時帶上。
			log.Printf("錯誤：工作單 %s 已 succeeded，但 MaxApplied 水位寫不進日誌：%v", job.JobID, err)
		}
		if retain, ok := r.executor.(retainer); ok {
			for _, line := range retain.AfterSucceeded(jobCtx, job) {
				log.Printf("工作單 %s 保留期：%s", job.JobID, line)
			}
		}
	case deploy.Failed, deploy.ManualIntervention:
		log.Printf("工作單 %s 完成為 %s；MaxApplied 不前進", job.JobID, state.State)
	default:
		if state.Replayed {
			log.Printf("工作單 %s 已由 Hub 回放終態 %s", job.JobID, state.State)
		} else {
			log.Printf("工作單 %s complete 回了未預期狀態 %q", job.JobID, state.State)
		}
	}
}

func (r *jobsRunner) renew(ctx context.Context, cancel context.CancelFunc, jobID string, lease model.JobLeaseResponse) {
	expires := lease.LeaseExpiresAt
	for {
		interval := renewalInterval(expires.Sub(r.now()))
		if err := r.sleep(ctx, interval); err != nil {
			return
		}
		var response model.JobLeaseResponse
		err := r.post(ctx, r.jobURL(jobID, "/lease:renew"),
			model.JobLeaseRequest{LeaseToken: lease.LeaseToken}, &response, http.StatusOK)
		if err != nil {
			// ⚠ 續租非 200 後 executor 必須停；舊 fencing token 可能已經屬於別人。
			log.Printf("工作單 %s 續租失敗，取消 executor：%v", jobID, err)
			cancel()
			return
		}
		if err := validateJobLeaseReceipt(response, lease.LeaseToken, r.now()); err != nil {
			log.Printf("工作單 %s 續租回應不完整，取消 executor：%v", jobID, err)
			cancel()
			return
		}
		expires = response.LeaseExpiresAt
	}
}

func renewalInterval(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 20 * time.Second
	}
	interval := remaining / 3
	if interval < 10*time.Second {
		return 10 * time.Second
	}
	return interval
}

func (r *jobsRunner) reject(ctx context.Context, jobID, leaseToken string, seq int,
	code deploy.RejectionCode, detail string) {
	req := model.JobRejectRequest{
		LeaseToken:    leaseToken,
		RejectionCode: string(code),
		Detail:        detail,
		Seq:           seq,
		OccurredAt:    r.now().UTC(),
	}
	var state model.JobStateResponse
	if err := r.post(ctx, r.jobURL(jobID, "/reject"), req, &state, http.StatusOK); err != nil {
		log.Printf("工作單 %s 拒單回報失敗：%v", jobID, err)
		return
	}
	r.nudgeObservation()
}

func (r *jobsRunner) nudgeObservation() {
	// ⚠ 這不是催快固定週期，而是機器剛結束一張可能改動狀態的工作單，舊事實
	// 不准再掛十分鐘。滿格表示已經有一次觀測排隊；再一個 nudge 丟掉不會少掉喚醒。
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *jobsRunner) post(ctx context.Context, endpoint string, body, out any, wantStatus int) error {
	var lastErr error
	for attempt := 1; attempt <= maxJobPostAttempts; attempt++ {
		status, err := doJSONStatus(ctx, http.MethodPost, endpoint, r.token, body, out)
		lastErr = err
		if err == nil && status != wantStatus {
			lastErr = &hubHTTPError{StatusCode: status,
				APIError: model.APIError{Code: "UNEXPECTED_STATUS", Message: fmt.Sprintf("預期 HTTP %d", wantStatus)}}
		}
		if lastErr == nil || !retryableJobPost(lastErr) {
			return lastErr
		}
		if attempt == maxJobPostAttempts {
			break
		}
		delay := r.retryBackoff(attempt)
		if ra := honoredRetryAfter(lastErr); ra > delay {
			delay = ra
		}
		if err := r.sleep(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}

func retryableJobPost(err error) bool {
	var httpErr *hubHTTPError
	if !errors.As(err, &httpErr) {
		return true
	}
	return httpErr.StatusCode >= 500 || httpErr.StatusCode == http.StatusRequestTimeout ||
		httpErr.StatusCode == http.StatusTooManyRequests
}

func (r *jobsRunner) jobURL(jobID, suffix string) string {
	return r.hubURL + "/v1/jobs/" + url.PathEscape(jobID) + suffix
}

func excerpt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := []byte(s)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
