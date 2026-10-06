// Package model holds the wire format shared by clawctl-hub and clawctl-agent.
//
// 這個 package 是 Hub 與 Agent 之間的契約，改這裡等於改 API。
//
// 三條規則，違反了整個健康模型就會塌（見 docs/SPEC.md §2.1）：
//
//  1. Agent 只回報「事實」，Hub 才產生「判決」。所以這裡沒有任何叫 Status
//     或 Healthy 的欄位讓 Agent 填 —— 那是 Hub 算出來的衍生值。
//  2. 時間有兩個：Agent 說的 (MeasuredAt/SentAt) 與 Hub 收到的 (ReceivedAt)。
//     生死判斷一律用 Hub 的那個。機器時鐘快兩天不能讓它看起來很健康。
//  3. 拿不到的東西回報 nil + Reason，不猜、不填零值。unknown 是合法狀態，
//     假裝知道不是。
package model

import (
	"encoding/json"
	"path/filepath"
	"time"
)

// SchemaVersion 是 Agent 上報格式的版本。Hub 收到不認得的版本要拒絕並說出來，
// 不要嘗試盡力解析 —— 那是 adapter 讀到屍體檔的同一類錯誤。
const SchemaVersion = 1

// ---------------------------------------------------------------- enrollment

// EnrollRequest 是機器第一次向 Hub 報到。用一次性 token 換一個長期身分。
type EnrollRequest struct {
	SchemaVersion int    `json:"schema_version"`
	EnrollToken   string `json:"enroll_token"`
	Hostname      string `json:"hostname"`
	MachineIDHint string `json:"machine_id_hint"` // /etc/machine-id，重灌會變，只是候選
	UnixUser      string `json:"unix_user"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	TailscaleIP   string `json:"tailscale_ip,omitempty"`
	AgentVersion  string `json:"agent_version"`
}

type EnrollResponse struct {
	SchemaVersion int    `json:"schema_version"`
	MachineID     string `json:"machine_id"`
	AgentToken    string `json:"agent_token"` // Phase 1 用 bearer token，見 docs/SPEC.md §5.1 的偏離說明
	// CheckinInterval 由 Hub 指派，Agent 不自己決定 —— 這樣調整節奏不用重裝 agent。
	CheckinIntervalSeconds     int `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int `json:"observation_interval_seconds"`

	// SettingsDigest 是上面那組值的身分。Agent 原樣存下來，之後每次 check-in
	// 帶回去，Hub 才有辦法說出「它真的在跑我指派的那一份」。
	SettingsDigest string `json:"settings_digest,omitempty"`
}

// ---------------------------------------------------------------- check-in

// Checkin 是每 2 分鐘一次的心跳。它必須很小、很快、而且跟 observation 分開 ——
// observation 連續失敗時心跳仍要能送達，否則 Hub 分不出「機器死了」與
// 「只是狀態檔過期」。
type Checkin struct {
	SchemaVersion int       `json:"schema_version"`
	SentAt        time.Time `json:"sent_at"`
	AgentVersion  string    `json:"agent_version"`

	// BootID 是**機器**的開機 ID（/proc/sys/kernel/random/boot_id），
	// 只有機器重開機才會變。
	//
	// ⚠ 它抓不到 agent 的 crash-loop。這裡原本的註解寫著「BootID 一小時內
	// 變 10 次就是 crash-looping」，那是錯的，而且錯了很久沒人發現 ——
	// 2026-09-03 全機隊每 90 秒被 SIGABRT 一次，機器已連續開機 80 天，
	// BootID 一次都沒變，Hub 全綠。要抓 process 重啟迴圈請用 AgentStartedAt。
	BootID   string `json:"boot_id"`
	AgentSeq int64  `json:"agent_seq"`

	// AgentStartedAt 是這個 agent process 起來的時刻，也就是 process 的身分證。
	//
	// ⚠ Hub 數它一小時內出現過幾個不同的值 = agent 重啟了幾次。
	// 這是唯一能分辨「一顆健康的心跳」與「一顆重啟送出來的心跳」的東西。
	// AgentSeq 分辨不了：它刻意跨重啟連續，所以 crash-loop 看起來會像平順遞增。
	AgentStartedAt time.Time `json:"agent_started_at"`

	// UptimeSeconds 必須用指標，因為剛開機不到一秒時 uptime 確實是 0，零值不能
	// 表示「沒量到」。也不另加 UptimeMeasured 兄弟旗標：舊 agent 的數字 JSON
	// 仍會解成非 nil，資料庫既有的 NOT NULL uptime_seconds 也仍是 Valid，沒有升級
	// 空窗。nil 直接交給 store 的 Exec 會寫成 SQL NULL，讓 UptimeObserved=false，
	// 一路在畫面顯示 unknown。
	UptimeSeconds  *int64 `json:"uptime_seconds"`
	DiskFreeBytes  int64  `json:"disk_free_bytes"`
	DiskTotalBytes int64  `json:"disk_total_bytes"`
	JobsEnabled    *bool  `json:"jobs_enabled,omitempty"`

	// DeviceSyncV1 says this exact agent process has the fixed device-sync v1
	// executor. Old agents omit it; the Hub must treat omission as unsupported
	// and must not infer support from agent_version text.
	DeviceSyncV1 bool `json:"device_sync_v1,omitempty"`

	// MaintenanceDiskCleanV1 says this process has the embedded disk-clean
	// executor. Omission means unsupported. The Hub must not infer it from
	// agent_version text.
	MaintenanceDiskCleanV1 bool `json:"maintenance_disk_clean_v1,omitempty"`

	// 本機狀態檔的年齡。心跳成功但這個一直變大 = Agent 在線但沒在觀測。
	ObservationAgeSeconds *int64 `json:"observation_age_seconds,omitempty"`

	// Phase 4 才會用到，但欄位先開著 —— 空欄位很便宜，改 wire format 很貴。
	MaxSeenRevision    int64 `json:"max_seen_revision"`
	MaxAppliedRevision int64 `json:"max_applied_revision"`

	// SettingsDigest 是 Agent 此刻正在跑的設定文件 digest，原樣帶回 Hub 上一次
	// 在 check-in 回應裡給它的那個值。
	//
	// ⚠ 這是規則 1 的直接應用：Agent 回報事實（我在跑這一份），Hub 才判決
	// （那跟我指派的是不是同一份）。Agent 不准自己說「已套用」。
	// ⚠ 太舊而不會填這一欄的 agent 留空。Hub 說「還沒回報」，不說「已套用」。
	SettingsDigest string `json:"settings_digest,omitempty"`
}

type CheckinResponse struct {
	// ReceivedAt 回送給 Agent，讓它能自己發現時鐘漂移並記在 log 裡。
	ReceivedAt                 time.Time `json:"received_at"`
	CheckinIntervalSeconds     int       `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int       `json:"observation_interval_seconds"`

	// Expectations 是 Hub 要這台機器去量的東西。
	//
	// ⚠ 走 checkin 而不是另開一個 endpoint：Hub→Agent 的設定下發這條路
	// （interval）本來就在這裡，多開一條就多一個會壞、會忘記驗的東西。
	// ⚠ 這**不是**控制指令，agent 只會去 stat 檔案，不會執行任何東西。
	// 真正改機器的控制走 Phase 4 工作單；目前以 per-machine bearer 的
	// machine_id 歸屬、activation 前 artifact digest 與單一 tailnet listener
	// 三條不變量保護。重新開過 mTLS 決策的理由見 docs/PHASE1.md §4。
	Expectations []Expectation `json:"expectations,omitempty"`

	// SettingsDigest 是這次下發的間隔值的身分，見 EnrollResponse 的說明。
	SettingsDigest string `json:"settings_digest,omitempty"`

	// WorkloadPolicyToken 是這一份 Expectations 與這台機器目前顯示名稱的
	// opaque 身分。Agent 必須把 token 跟規則當成同一包保存，並在用這包
	// 規則量完 observation 後原樣帶回；Hub 不可以替 incoming batch 補 token。
	WorkloadPolicyToken string `json:"workload_policy_token,omitempty"`
}

// AgentReadinessResponse is the Hub receipt for the latest check-in and
// identity evidence accepted under the caller's machine credential.
// IdentityMeasuredAt uses the agent's clock; IdentityReceivedAt uses the Hub's.
// Both timestamps describe the same stored identity evidence.
type AgentReadinessResponse struct {
	MachineID              string     `json:"machine_id"`
	LastCheckinReceivedAt  *time.Time `json:"last_checkin_received_at"`
	AgentStartedAt         *time.Time `json:"agent_started_at"`
	AgentVersion           string     `json:"agent_version,omitempty"`
	JobsEnabled            *bool      `json:"jobs_enabled,omitempty"`
	DeviceSyncV1           *bool      `json:"device_sync_v1,omitempty"`
	MaintenanceDiskCleanV1 *bool      `json:"maintenance_disk_clean_v1,omitempty"`
	IdentityReceivedAt     *time.Time `json:"identity_received_at"`
	IdentityMeasuredAt     *time.Time `json:"identity_measured_at"`
	IdentityOS             string     `json:"identity_os,omitempty"`
	IdentityArch           string     `json:"identity_arch,omitempty"`
}

// ---------------------------------------------------------------- 工作單

const (
	DeviceSyncJobKind               = "device-sync"
	MaintenanceDiskCleanCapability  = "maintenance_disk_clean_v1"
	DeviceSyncSpecSchemaVersion     = 1
	DeviceSyncSpecJSON              = `{"kind":"device-sync","schema_version":1}`
	DeviceSyncMinTimeoutSeconds     = 1
	DeviceSyncMaxTimeoutSeconds     = 300
	DeviceSyncDefaultTimeoutSeconds = 60
	DeviceSyncResourceKind          = "device"
	DeviceSyncResourceID            = "sync"
	DeviceSyncVerificationRuleID    = "device_sync_request_accepted"
	DeviceSyncVerificationCommand   = "agent observation queue"
	DeviceSyncVerificationStdout    = "同步要求已接受；工作單終態後排入一輪完整觀測。"
)

// DeviceSyncSpec is deliberately closed and carries no command, path, URL or
// operator text. The executor only requests the agent's existing observation
// loop after this job reaches a Hub-confirmed terminal state.
type DeviceSyncSpec struct {
	Kind          string `json:"kind"`
	SchemaVersion int    `json:"schema_version"`
}

// OpenClawSpec 是 Hub 依自己抓回並量過的 artifact 生成的期望狀態。
// Agent 只解析它來核對釘住的 digest；這一刀不下載也不執行 artifact。
type OpenClawSpec struct {
	Kind     string       `json:"kind"`
	Version  string       `json:"version"`
	Artifact *ArtifactRef `json:"artifact,omitempty"`
}

const NodeRuntimeBundleLayoutV1 = "node-runtime-bundle:v1"

// BATServerRoot holds AI-Intune's bat-server releases, token, data, and unit file.
func BATServerRoot(home string) string {
	return filepath.Join(home, ".local", "share", "clawctl", "bat-server")
}

func BATServerDataDir(home string) string {
	return filepath.Join(BATServerRoot(home), "data")
}

const HermesOCIBundleLayoutV1 = "hermes-oci-bundle:v1"

// NodeRuntimeSpec selects one platform subtree from an immutable AI-Intune
// runtime bundle. The agent proves its compiled OS/architecture matches before
// extracting or activating any bytes.
type NodeRuntimeSpec struct {
	Kind         string       `json:"kind"`
	Version      string       `json:"version"`
	TargetOS     string       `json:"target_os"`
	TargetArch   string       `json:"target_arch"`
	BundleLayout string       `json:"bundle_layout"`
	Artifact     *ArtifactRef `json:"artifact,omitempty"`
}

// HermesSpec selects the target platform from a Hub-verified multi-platform
// OCI image bundle. ImageIndexDigest binds the official upstream image index;
// Artifact binds the deterministic Hub bundle delivered to the endpoint.
type HermesSpec struct {
	Kind             string       `json:"kind"`
	Version          string       `json:"version"`
	TargetOS         string       `json:"target_os"`
	TargetArch       string       `json:"target_arch"`
	BundleLayout     string       `json:"bundle_layout"`
	ImageReference   string       `json:"image_reference"`
	ImageIndexDigest string       `json:"image_index_digest"`
	Artifact         *ArtifactRef `json:"artifact,omitempty"`
}

type ArtifactRef struct {
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	URL             string `json:"url"`
	EnginesNode     string `json:"engines_node"`
	UpstreamTarball string `json:"upstream_tarball"`
	SHA512          string `json:"sha512"`
}

// ParseJobSpec 只讀 admission gate 需要的 kind 與 artifact。
// 未知欄位留給各 executor；共用閘門不該替 executor 解讀完整 spec。
func ParseJobSpec(raw []byte) (kind string, artifact *ArtifactRef, err error) {
	var spec struct {
		Kind     string       `json:"kind"`
		Artifact *ArtifactRef `json:"artifact"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", nil, err
	}
	return spec.Kind, spec.Artifact, nil
}

// JobResponse 是 Hub 交給已認證 Agent 的工作單。
// ArtifactDigest 不可省略；Agent 必須在 activation 之前拿它核對產物。
type JobResponse struct {
	JobID            string `json:"job_id"`
	MachineID        string `json:"machine_id"`
	DesiredID        string `json:"desired_id"`
	Revision         int64  `json:"revision"`
	State            string `json:"state"`
	ExecutionTimeout int    `json:"execution_timeout"`
	ArtifactDigest   string `json:"artifact_digest"`
	Irreversible     bool   `json:"irreversible"`
	// ResourceKind、ResourceID 與 Spec 來自 desired_state：工作單只是
	// 「給誰、第幾版」，「要做什麼」與 revision 屬於哪個資源在這裡。
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Spec         json.RawMessage `json:"spec"`
}

// JobClaimRequest 目前沒有可由 Agent 決定的欄位。
// 保留物件型別，讓協定日後新增選填欄位時不必改 handler 形狀。
type JobClaimRequest struct{}

type JobLeaseRequest struct {
	LeaseToken string `json:"lease_token"`
}

type JobLeaseResponse struct {
	LeaseToken     string    `json:"lease_token"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type JobEventRequest struct {
	LeaseToken string          `json:"lease_token"`
	Seq        int             `json:"seq"`
	Phase      string          `json:"phase"`
	Payload    json.RawMessage `json:"payload"`
	OccurredAt time.Time       `json:"occurred_at"`
}

type JobEventResponse struct {
	AcceptedSeq int `json:"accepted_seq"`
}

type JobVerificationRequest struct {
	LeaseToken    string    `json:"lease_token"`
	RuleID        string    `json:"rule_id"`
	Command       string    `json:"command"`
	ExitCode      int       `json:"exit_code"`
	StdoutExcerpt string    `json:"stdout_excerpt"`
	StderrExcerpt string    `json:"stderr_excerpt"`
	Passed        bool      `json:"passed"`
	VerifiedAt    time.Time `json:"verified_at"`
}

type JobCompleteRequest struct {
	LeaseToken string `json:"lease_token"`
}

type JobStateResponse struct {
	State string `json:"state"`
	// Replayed：這不是這次呼叫造成的結果，是工作單早就到了終態，Hub 把原結果
	// 回放給你（SPEC §5.2 DUPLICATE_JOB_ID）。回應弄丟之後的重送會看到這個。
	Replayed bool `json:"replayed,omitempty"`
}

// JobStateError 在拒絕狀態轉移時，同時交代工作單最後停留的位置。
type JobStateError struct {
	APIError
	State string `json:"state"`
	// LeaseExpiresAt：領單撞到「已經被領走」時告訴 agent 要等到什麼時候 ——
	// 回應弄丟、token 跟著丟的 agent，唯一的出路是等這個租約過期再領一次。
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

type JobRejectRequest struct {
	LeaseToken    string    `json:"lease_token"`
	RejectionCode string    `json:"rejection_code"`
	Detail        string    `json:"detail,omitempty"`
	Seq           int       `json:"seq,omitempty"`
	OccurredAt    time.Time `json:"occurred_at,omitempty"`
}

type JobRejectEventPayload struct {
	RejectionCode string `json:"rejection_code"`
	Detail        string `json:"detail,omitempty"`
}

type CapabilitiesResponse struct {
	SchemaVersion int      `json:"schema_version"`
	ResourceKinds []string `json:"resource_kinds"`
}

// ---------------------------------------------------------------- observation

// ObservationBatch 是每 10 分鐘一次的完整觀測。append-only —— Hub 永遠不覆蓋，
// 因為「今天是綠的，星期三就死了」只有歷史答得出來，而上游的資料庫
// 7 天 / 2000 筆就會把歷史吃掉。
type ObservationBatch struct {
	SchemaVersion int       `json:"schema_version"`
	MeasuredAt    time.Time `json:"measured_at"`

	// WorkloadPolicyToken 是產生本批 Artifacts / Events 時實際使用的政策身分。
	// 空值代表舊 Agent 或第一次 checkin 前的量測，Hub 必須視為 unknown，
	// 不能把自己當下的政策身分貼上去冒充 coherent healthy witness。
	WorkloadPolicyToken string        `json:"workload_policy_token,omitempty"`
	Identity            Identity      `json:"identity"`
	Resources           Resources     `json:"resources"`
	Systemd             []Unit        `json:"systemd"`
	Journals            []UnitJournal `json:"journals,omitempty"`

	// Artifacts 是對 Hub 宣告的那些 Expectation 的量測結果。
	// ⚠ 空的有兩個意思：沒有人宣告過期望，或這台的 agent 是舊版。
	// Hub 必須分得出來 —— 見 store.Detail.Expectations。
	Artifacts []ArtifactCheck `json:"artifacts,omitempty"`

	// Events 是那些同時宣告了 EventSpec 的期望的事件流量測。
	Events      []EventStream `json:"events,omitempty"`
	OpenClaw    OpenClaw      `json:"openclaw"`
	Credentials []Credential  `json:"credentials"`
	CLITools    []CLITool     `json:"cli_tools"`
	BAT         BAT           `json:"bat"`
}

// BAT 是這台機器上的 bat-server —— 也就是 Phase 3 那個「連上去」的目標。
//
// ⚠⚠ 這整個型別存在的理由，是因為詳細頁上原本寫的是
// `https://{{TailscaleIP}}:8080` —— 一個**寫死在樣板裡的猜測**。
// 實測結果：機隊四台跑 BAT 的機器，沒有一台在 8080。真正的 port 是 9876，
// 而且其中兩台是 `--bind=localhost` —— 那兩台**任何位址都連不上**，
// 因為 bat-server 根本沒有綁在對外的介面上。
//
// 一個從來沒有被點過的連結，長得跟一個正確的連結一模一樣。
type BAT struct {
	// Running：有沒有在 process 表裡看到 bat-server。
	// ⚠ 這跟「systemd 說 bat-server.service 是 active」是兩件事。
	// 兩個都收，因為它們不一致的時候，那個不一致本身就是答案。
	Running bool `json:"running"`

	// Port / Bind 是從 argv 讀出來的**啟動參數**，不是實際綁定的結果。
	//
	// ⚠ /proc/<pid>/cmdline 連 root 起的 process 都讀得到（跟 exe 不一樣，
	// 那個要 ptrace —— 見 docs/PHASE1.md §5.10）。實測 sampleagent3 上
	// 以 opc 的身分讀得到 root 的 bat-server cmdline。
	Port int    `json:"port,omitempty"`
	Bind string `json:"bind,omitempty"` // argv 裡 --bind 的原文，不翻譯

	// Argv 是原文（截斷過）。上面兩欄看不懂的時候，人自己看這個。
	Argv string `json:"argv,omitempty"`

	// ListenAddrs 是**核心的 socket 表**裡，真的處於 LISTEN 而且 port 相符的本機位址。
	//
	// ⚠ 這一欄跟 Port/Bind 是不同層級的證據：那兩個是「它被要求做什麼」，
	// 這個是「核心那邊真的發生了什麼」。一個 --bind=tailscale 但 tailscale
	// 還沒起來的 bat-server，argv 完全正常而這一欄是空的。
	//
	// ⚠ 沒有特權就沒辦法把 socket 歸屬到 pid，所以這一欄只能說
	// 「這個 port 上有東西在聽」，不能說「那個東西就是 BAT」。
	// 畫面上的措辭必須守住這個分寸。
	ListenAddrs []string `json:"listen_addrs,omitempty"`

	// Reason：講不出一個可以連過去的位址時，原因是什麼。
	// ⚠ 留白不是答案。「沒有位址」跟「不知道有沒有位址」要分得出來。
	Reason string `json:"reason,omitempty"`
}

type Identity struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
	UnixUser      string `json:"unix_user"`
	MachineIDHint string `json:"machine_id_hint"`
	BootID        string `json:"boot_id"`
	TailscaleIP   string `json:"tailscale_ip,omitempty"`

	// LingerEnabled: systemd --user unit 在使用者登出後會不會活著，只有在
	// LingerMeasured 為 true 時才有意義。量不到的平台（macOS 沒有 systemd
	// linger）兩個欄位都是 false，意思是「沒量到」，不是「沒開啟」。
	// false 的機器會「假離線」—— UI 必須把這種 Unreachable 跟真的離線分開講，
	// 否則人會去查一台其實好好的機器。
	LingerEnabled  bool `json:"linger_enabled"`
	LingerMeasured bool `json:"linger_measured"`
}

type Resources struct {
	DiskFreeBytes     int64 `json:"disk_free_bytes"`
	DiskTotalBytes    int64 `json:"disk_total_bytes"`
	MemTotalBytes     int64 `json:"mem_total_bytes"`
	MemAvailableBytes int64 `json:"mem_available_bytes"`
	CPUCount          int   `json:"cpu_count"`
	// Load1m 是指標，因為 0.00 是合法的已量到值，不能拿零值表示沒量到；nil 才是沒量到。
	// 不另加 LoadMeasured 兄弟旗標：舊 agent 與既有觀測列沒有旗標，升級空窗會把
	// Linux 的既有負載誤判成未知；指標則能讓舊 JSON 數字解成非 nil，只有新的 darwin agent 送 null。
	Load1m *float64 `json:"load_1m"`
}

// Unit 是一個 systemd unit 的觀測。
//
// ⚠ ActiveState 幾乎沒有資訊量：Restart=always 讓它恆為 "active"，
// 一個每 10 秒崩潰一次的服務也是 "active"。真正有用的是
// ActiveEnterTimestamp 一直在變，以及 NRestarts。
type Unit struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	// Measured：agent 有沒有真的拿到 systemctl 對這個 unit 的回答。
	//
	// ⚠ 它決定 Present=false 能不能拿來當「沒有這個 unit」的判決。
	// systemctl 叫不動（例如沒有 user bus）時 Present 一樣是 false，
	// 但那時候我們知道的是「我不知道」，不是「它沒裝」。
	// ⚠ 舊 agent 不送這一欄，所以 false 同時代表「舊 agent」與「問不到」——
	// 兩種都是不知道，都不可以講成「沒有這個 unit」。
	// ⚠ 反過來不成立：Present=true 的舊 agent payload 這一欄也是 false，
	// 所以不准建立 Present ⇒ Measured 的不變量。
	Measured bool `json:"measured,omitempty"`

	// Reason：問不到的時候，agent 說的原因原文。
	// ⚠ 它是給人讀的下一步，不是判決依據；不要拿它做字串比對。
	Reason               string     `json:"reason,omitempty"`
	ActiveState          string     `json:"active_state"`
	SubState             string     `json:"sub_state"`
	ActiveEnterTimestamp *time.Time `json:"active_enter_timestamp,omitempty"`
	NRestarts            int        `json:"n_restarts"`
	MainPID              int        `json:"main_pid,omitempty"`
}

// UnitJournal 是一個 unit 在最近一個窗口裡「講了多少話、講的是哪幾種話」。
//
// ⚠⚠ 這個型別**刻意不含任何健康欄位**，而且不准長出來。沒有 Healthy、
// 沒有 ErrorCount、沒有 Severity。理由在 PRODUCT.md 地基二：
// 掃 log 關鍵字判成敗會漏掉最痛的那種錯 —— 安靜地做錯事的服務，log 裡沒有 error。
//
// 它跟 Unit **故意分成兩個型別、走兩個 observed_state.kind**，不是併進 Unit。
// Unit 會餵進 state 判定（state.Facts.AgentUnitSeen / AgentNRestarts）；
// journal 不會，也不可以。分開存是讓這條界線由結構擋著，而不是只靠註解擋著。
//
// 它回答的問題是「這個 unit 在說什麼」，交給人讀。
// 它**不回答**「這個 unit 有沒有在做事」—— 一個安靜的 unit 可能正在好好工作，
// 也可能已經卡死；這兩者在這個型別裡長得一模一樣。
type UnitJournal struct {
	Unit      string `json:"unit"`
	WindowSec int    `json:"window_sec"`

	// Lines 是窗口內的非空行數。Truncated 為真時它是**下界**，不是總數。
	Lines     int  `json:"lines"`
	Truncated bool `json:"truncated,omitempty"`

	// Shapes 是正規化後的相異句型數。
	//
	// ⚠ 不要拿 Lines/Shapes 的比值當「卡住」的指標。實測 2026-09-04：
	// sampleagent2 的 clawctl-agent 一小時 6 行、句型 1 種（完全正常），
	// 跟同一台上真的卡住的 openclaw-watcher（38 行、1 種）在這個比值上
	// 分不出來。重複不是病。
	Shapes int `json:"shapes"`

	// Top 是最常見的幾種句型，各附一行原文範例。
	// 原文已遮蔽密鑰形狀，除此之外一個字都沒改，也沒有被解析過。
	Top []JournalShape `json:"top,omitempty"`

	// Err 是讀取失敗的原因。
	// ⚠ 有 Err 的時候 Lines=0 **不代表這個 unit 很安靜**，代表我們沒看到。
	Err string `json:"err,omitempty"`
}

// JournalShape 是一種句型出現幾次，加一行原文。
type JournalShape struct {
	Count   int    `json:"count"`
	Example string `json:"example"`
}

// ---------------------------------------------------------------- 期望

// Expectation 是**人宣告**的一句話：「這個 unit 做完一輪，應該留下什麼」。
//
// ⚠⚠ 這是整個「有沒有在做事」判定的地基，而它刻意**不是 clawctl 猜的**。
//
// PRODUCT.md 地基二否決了「掃 log 關鍵字判成敗」，但它同時給了兩條合法的路，
// 第二條是：「工作做完之後，由一支**跟被測對象無關**的程式去跑驗證指令」。
// Expectation 就是那條路的輸入 —— clawctl 不猜什麼算成功，由你宣告；
// 宣告完之後，檢查的是產出物，不是 log。
//
// 為什麼一定要看產出物而不是 log（2026-09-04 實測，sampleagent2）：
//
//	openclaw-watcher.service   active_enter = 2026-06-15T03:59  ← 讀作「81 天很穩」
//	machine-mission.md         mtime        = 2026-06-15T04:23  ← 讀作「81 天沒產出」
//
// **同一個日期，在兩個欄位裡意思完全相反。**
// 而它的 log 是 08-27 才開始喊 FAILED —— log 比產出物晚了 73 天。
// log 不只是不可靠的偵測器，它還是個遲到的偵測器。
type Expectation struct {
	// Machine 是機器的顯示名稱；"*" 表示所有機器。
	Machine string `json:"machine"`
	Unit    string `json:"unit"`

	// Artifact 是「做完一輪應該被更新」的那個檔案的絕對路徑。
	Artifact string `json:"artifact"`

	// MaxAgeSeconds：超過這個歲數就是沒做到。
	MaxAgeSeconds int `json:"max_age_seconds"`

	// Why 是人寫給人看的一句話，會原封不動出現在 finding 裡。
	// ⚠ 一條說不出「為什麼」的期望，半年後沒有人敢刪也沒有人敢信。
	Why string `json:"why"`

	// Events 讓這個產出物同時被當成**結構化事件流**來讀（JSONL）。
	// nil = 只看檔案新不新，不讀內容。
	Events *EventSpec `json:"events,omitempty"`
}

// EventSpec 是「這個檔案是一條事件流，而這些事件名代表不 OK」。
//
// ⚠⚠ 為什麼這**不是**掃 log 關鍵字的變形，界線在哪：
//
//  1. 讀的是**列舉欄位**，不是散文。事件名是那支驗證程式自己定義的有限詞彙，
//     不是我們在自然語言裡撈字。
//  2. 哪些事件名算不 OK **由人宣告**，clawctl 不猜。跟 Expectation 同一條原則。
//  3. 地基二反對關鍵字表的理由是「最痛的那種錯，log 裡沒有 error」——
//     而那個漏洞由**同一條規則的 MaxAgeSeconds** 補上：驗證器停止寫入，
//     檔案就會過期，那是一個獨立於內容的訊號。
//     兩者合起來才完整：新鮮度回答「驗證器還活著嗎」，事件名回答「它找到什麼」。
//     **只做第二件事會退回地基二否決的東西。**
type EventSpec struct {
	// TsField / TypeField 是那條 JSONL 裡時間與事件名的欄位名。
	// ⚠ 不給預設值。猜錯欄位名會得到一個「什麼事件都沒有」的乾淨答案。
	TsField   string `json:"ts_field"`
	TypeField string `json:"type_field"`

	// NotOK 是被宣告為「不 OK」的事件名。
	NotOK []string `json:"not_ok"`

	// WindowSeconds：往回看多久。
	WindowSeconds int `json:"window_seconds"`
}

// EventSummary 是一種事件在窗口裡出現幾次、最後一次是什麼時候。
type EventSummary struct {
	Type   string     `json:"type"`
	Count  int        `json:"count"`
	LastAt *time.Time `json:"last_at,omitempty"`
}

// EventStream 是 agent 對一條事件流的量測。
//
// ⚠⚠ 同樣只回報事實，不回報過或不過 —— 判決是 Hub 的事。
type EventStream struct {
	Unit string `json:"unit"`
	Path string `json:"path"`

	// Declared 是宣告過的事件名（**沒出現的也要在**，Count=0）。
	// ⚠ 只回報「有出現的」會讓「這種錯誤沒發生」跟「我沒在看這種錯誤」
	// 長得一樣。宣告過的東西要能證明自己被看過。
	Declared []EventSummary `json:"declared"`

	// Undeclared 是**沒有人宣告過**的事件名。
	// ⚠⚠ 這一欄是這個型別最重要的部分。驗證程式長出一種新的失敗事件時，
	// 如果我們只看宣告過的名字，那個新失敗會完全隱形 ——
	// 而它恰好最可能是還沒有人想到的那一種。
	Undeclared []EventSummary `json:"undeclared,omitempty"`

	// UndeclaredTotal 是**沒被截斷前**有幾種。Undeclared 只留前幾名，
	// ⚠ 有上限就一定要講總數，不然截斷會偽裝成「就這些」。
	UndeclaredTotal int `json:"undeclared_total,omitempty"`

	// CoveredFrom 是這次真的讀到的最舊一筆的時間。
	// ⚠ 它比窗口起點新，就代表**沒有覆蓋完整個窗口**（撞到讀取上限），
	// 那時候 Count 是下界不是總數。不講出來的話，一個被截斷的計數
	// 看起來會跟真的總數一模一樣。
	CoveredFrom *time.Time `json:"covered_from,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"`

	// Malformed 是解析不了的行數。⚠ 安靜跳過會讓一個格式壞掉的事件流
	// 長得像一個很安靜的事件流。
	Malformed int    `json:"malformed,omitempty"`
	Err       string `json:"err,omitempty"`
}

// ArtifactCheck 是 agent 對一條 Expectation 的量測結果。
//
// ⚠⚠ 它只回報**事實**（在不在、多舊），不回報「過或不過」——
// 判決是 Hub 的事（見本檔開頭的三條規則第 1 條）。
type ArtifactCheck struct {
	Unit     string `json:"unit"`
	Artifact string `json:"artifact"`

	Exists  bool       `json:"exists"`
	ModTime *time.Time `json:"mod_time,omitempty"`
	Size    int64      `json:"size,omitempty"`

	// Err 是 stat 失敗的原因（權限、路徑是目錄、symlink 斷掉…）。
	// ⚠ 「檔案不存在」與「我看不到這個檔案」是兩件事：
	// 前者 Exists=false 且 Err 為空，後者 Err 非空。
	// 把兩者混為一談會讓一個權限問題長得像一個真的失敗。
	Err string `json:"err,omitempty"`
}

// OpenClaw 是 L1 存活訊號的來源。
//
// ⚠ 這裡刻意沒有 "healthy" 或 "success" 欄位。實測 cron_run_logs.status='ok'
// 的意思是「agent 的回合正常結束並產出文字」，64% 的 ok 其 summary 在描述失敗。
// 所以我們只回報「最後一次跑完是什麼時候」(L1)，以及把 summary 原文帶回去
// 讓人看 (L2)。永遠不要在這裡加一個布林值說它好不好。
type OpenClaw struct {
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`

	// 三個版本座標。實測同一台上 CLI / gateway / upstream 三個都不一樣，
	// 而 `openclaw --version` 只報第一個。分開存，因為修法不一樣。
	CLIVersion      string `json:"cli_version,omitempty"`
	CLIVersionRaw   string `json:"cli_version_raw,omitempty"`
	GatewayVersion  string `json:"gateway_version,omitempty"`
	UpstreamVersion string `json:"upstream_version,omitempty"`

	Install      *OpenClawInstall `json:"install,omitempty"`
	DB           *OpenClawDB      `json:"db,omitempty"`
	CrashBundles int              `json:"crash_bundles"`
}

// OpenClawInstall 是「這台的 OpenClaw 裝在哪、從哪裡跑、用哪個 node」的事實。
//
// ⚠ 每個欄位都只在量到時填；布林或數字若需要區分「0」與「不知道」，
// 就另帶 Measured 或使用指標。把零值當答案會讓舊 agent 看起來像真的量到 0。
type OpenClawInstall struct {
	// unit：systemctl --user show openclaw-gateway.service。
	UnitFound     bool       `json:"unit_found"`
	UnitReason    string     `json:"unit_reason,omitempty"`
	UnitPath      string     `json:"unit_path,omitempty"`
	DropInPaths   []string   `json:"drop_in_paths,omitempty"`
	ExecStart     string     `json:"exec_start,omitempty"`
	KillMode      string     `json:"kill_mode,omitempty"`
	NRestarts     *int       `json:"n_restarts,omitempty"`
	ActiveEnterAt *time.Time `json:"active_enter_at,omitempty"`
	MainPID       int        `json:"main_pid,omitempty"`

	// 從 ExecStart 拆出的 gateway 執行座標。
	NodePath    string   `json:"node_path,omitempty"`
	RunningDir  string   `json:"running_dir,omitempty"`
	GatewayArgs []string `json:"gateway_args,omitempty"`

	// gateway 套件目錄的唯讀事實。
	RunningDirExists   bool   `json:"running_dir_exists"`
	RunningDirOwner    string `json:"running_dir_owner,omitempty"`
	RunningDirWritable *bool  `json:"running_dir_writable,omitempty"`
	RunningDirVersion  string `json:"running_dir_version,omitempty"`
	RunningDirReason   string `json:"running_dir_reason,omitempty"`

	// /proc/<MainPID>/cmdline 裡實際執行的 script。
	ProcessIndexJS     string `json:"process_index_js,omitempty"`
	ProcessMatchesUnit *bool  `json:"process_matches_unit,omitempty"`
	ProcessReason      string `json:"process_reason,omitempty"`

	// gateway 用的 node，以及和它同一個安裝版面的 npm。
	NodeVersion       string `json:"node_version,omitempty"`
	NodeVersionReason string `json:"node_version_reason,omitempty"`
	NpmPath           string `json:"npm_path,omitempty"`
	NpmVersion        string `json:"npm_version,omitempty"`
	NpmReason         string `json:"npm_reason,omitempty"`

	// clawctl 自己管理 release 時使用的約定版面；目前只觀測，不建立。
	ReleasesDir      string `json:"releases_dir"`
	ReleasesPresent  bool   `json:"releases_present"`
	CurrentLink      string `json:"current_link,omitempty"`
	DiskFreeBytes    int64  `json:"disk_free_bytes"`
	DiskFreeMeasured bool   `json:"disk_free_measured"`
	DiskFreeReason   string `json:"disk_free_reason,omitempty"`
}

// OpenClawDB 是狀態資料庫的觀測。
//
// ⚠ Layout 有兩種同時活在機隊上：
//
//	"consolidated" (2026.6.x) → ~/.openclaw/state/openclaw.sqlite
//	"split"        (2026.5.x) → ~/.openclaw/tasks/runs.sqlite
//
// 欄位相同所以 L1 兩種都拿得到，但寫死單一路徑今天就會在 5 台裡錯 2 台。
type OpenClawDB struct {
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`
	Layout  string `json:"layout,omitempty"`
	Path    string `json:"path,omitempty"`

	// Support 標 unsupported 的時機只有一個：**找得到資料庫檔案，
	// 但它不在我們認得的任何一條路徑上**。
	//
	// ⚠ 「沒有資料庫」跟「有資料庫但版本換了位置」是兩件要做不同事的事。
	// 前者是「OpenClaw 裝了但還沒跑過」，後者是「clawctl 需要更新」。
	// 沒有這一欄，兩者都只會顯示一句「找不到 sqlite」，
	// 然後第二種情況會被當成第一種，靜靜地錯上好幾個月。
	Support SupportLevel `json:"support,omitempty"`

	// FoundAt 是在 ~/.openclaw 底下真的找到、但我們不認得的 sqlite 檔。
	// ⚠ 這是 unsupported 那句話的**證據**。沒有它，那句話跟猜測沒兩樣。
	FoundAt []string `json:"found_at,omitempty"`

	// L1：最後一次任務跑完的時間。這是整個健康模型唯一可自動判定的支點。
	LastTaskEndedAt *time.Time     `json:"last_task_ended_at,omitempty"`
	TaskRunRows     int            `json:"task_run_rows"`
	TaskStatusCount map[string]int `json:"task_status_counts,omitempty"`

	// ⚠ 上游的 last_run_at_ms 是「開始」時間不是完成時間，這裡不收它。
	LastCronRunAt   *time.Time     `json:"last_cron_run_at,omitempty"`
	CronRunLogRows  int            `json:"cron_run_log_rows"`
	CronStatusCount map[string]int `json:"cron_status_counts_24h,omitempty"`

	// CronJobsTotal 是 cron_jobs 宣告的工作數，CronJobsEnabled 是其中啟用的數量。
	// ⚠「0 個工作」與「有工作但沒跑」是兩件事；前者沒有任何工作可跑，
	// 後者才有可能是排程沒有動。沒量到的格子要看 Measured，不能把零值當成 0 個。
	CronJobsTotal            int        `json:"cron_jobs_total"`
	CronJobsTotalMeasured    bool       `json:"cron_jobs_total_measured"`
	CronJobsEnabled          int        `json:"cron_jobs_enabled"`
	CronJobsEnabledMeasured  bool       `json:"cron_jobs_enabled_measured"`
	NextCronRunAt            *time.Time `json:"next_cron_run_at,omitempty"`
	CronJobsOverdue          int        `json:"cron_jobs_overdue"`
	CronJobsScheduleMeasured bool       `json:"cron_jobs_schedule_measured"`

	// L2 的原始素材：最近幾筆 summary 原文，交給人判斷。
	// ⚠ 絕對不要在 Hub 或 Agent 解析這些文字來推斷成敗 —— 那是「掃 log
	// 關鍵字」的變形，docs/PRODUCT.md 的地基二否決過。
	RecentSummaries []RunSummary `json:"recent_summaries,omitempty"`

	// 實測全機隊都是 0，上游沒在寫。收它是為了在上游開始寫的那天能發現。
	TerminalOutcomePopulated int `json:"terminal_outcome_populated"`

	// Occupancy 是占用帳本的原始素材：每一筆都是「某個時刻這台機器用了
	// 哪一家的憑證」的真實證據。
	//
	// ⚠ SPEC §4.4 說「以 task_event 為優先」，但實測**上游根本沒有
	// task_event 這張表**。真正拿得到的證據是 cron 的執行紀錄，
	// 而它剛好帶著 provider —— 那正是「哪張票在被用」。
	Occupancy []OccupancyEvidence `json:"occupancy,omitempty"`

	// OccupancyRowsSeen / OccupancyRowsNoProvider 是這次掃了幾筆、
	// 其中幾筆因為沒有 provider 而**不能**寫成占用。
	//
	// ⚠ 這兩個數字必須一起送。SPEC 的硬規則是「沒證據不准寫 profile_id」，
	// 而一個安靜地丟掉三成資料的帳本，看起來會跟一個完整的帳本一模一樣。
	// 實測 samplehub1 是 3315/4035、sampleagent2 是 2793/2942 有 provider ——
	// 缺的那些大多是失敗的回合，也就是最需要知道「當時用的是哪張票」的那些。
	OccupancyRowsSeen       int `json:"occupancy_rows_seen"`
	OccupancyRowsNoProvider int `json:"occupancy_rows_no_provider"`
}

// OccupancyEvidence 是一筆「這台機器在這個時間點用了哪一家的憑證」的原始證據。
//
// ⚠⚠ Provider 一律存**上游原文，不正規化**。
//
// 實測 samplehub1 寫 "openai"、sampleagent2 寫 "openai-codex"，那是同一條 codex
// 訂閱在不同 OpenClaw 版本下的兩個名字（見 openclaw 2026.6.1 的 auth
// provider 更名）。如果帳本自動把它們併成一個，帳面上會看不出差別；
// 如果反過來自動拆開，看起來會像發生過一次從來沒發生的換票。
// 兩種錯都比「如實寫下當時上游怎麼講的」糟。
type OccupancyEvidence struct {
	// Source 是這筆證據從哪裡讀來的，兩種 layout 給不同的值。
	// 帳本上要看得到，因為它決定了這筆資料的可信度與欄位齊全度。
	Source string `json:"source"` // cron_run_logs | cron_runs_jsonl

	JobID string    `json:"job_id"`
	At    time.Time `json:"at"`

	// Status 是上游原文。⚠ "ok" 只代表這一回合正常結束，不代表做對了。
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`

	// SessionKey 形如 agent:<名字>:cron:<job>:run:<id>。
	// AgentID 是從它切出來的第二段；切不出來就留空，不猜。
	SessionKey string `json:"session_key,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`

	// ErrorText 是上游原文，同樣不分類。
	//
	// ⚠ SPEC 有一個 last_error_category 欄位（rate_limit|auth|timeout|…），
	// 但那需要把自由文字對到固定分類，而那就是「掃 log 關鍵字」的變形，
	// docs/PRODUCT.md 的地基二否決過。實測最常見的一句是
	// "cron: job interrupted by gateway restart" —— 它根本不屬於 SPEC
	// 列的任何一類。所以這裡只存原文，分類等上游給出結構化訊號再說。
	ErrorText string `json:"error_text,omitempty"`

	TotalTokens int `json:"total_tokens,omitempty"`
}

type RunSummary struct {
	JobID   string    `json:"job_id"`
	At      time.Time `json:"at"`
	Status  string    `json:"status"` // ⚠ "ok" 不代表成功
	Summary string    `json:"summary"`
}

// CredStatus 是憑證的五態。
//
// ⚠ 永遠不要壓成布林值。"configured" 不等於 "登入還有效" —— 檔案裡的過期時間
// 沒到，不代表雲端那邊沒把 session 踢掉。綠燈只能來自真實請求成功。
type CredStatus string

const (
	CredAbsent     CredStatus = "absent"     // 沒裝這家
	CredConfigured CredStatus = "configured" // 有憑證且過期時間還沒到
	CredExpired    CredStatus = "expired"    // 過期時間已過
	CredUnknown    CredStatus = "unknown"    // 讀不到 / 沒有過期欄位 / 格式不認得
	CredFailed     CredStatus = "failed"     // 真實請求失敗過

	// ⚠ CredExpiresSoon 目前沒有任何生產者，這是實測改出來的，不是漏做。
	//
	// 「剩餘 ≤ N 天」這條規則對會自動續期的 access token 永遠成立
	// （claude 名目壽命 8 小時、實測剩餘 2.3 小時），會讓全機隊永遠黃燈，
	// 而永遠亮的黃燈會被靜音。Agent 只給 expired / configured / unknown。
	// 完整理由見 internal/state 的常數區與 internal/probe.classifyCred。
	CredExpiresSoon CredStatus = "expires_soon"
)

// Credential 回報的是三元組而不是一個狀態，因為本機檔案裡沒有任何欄位能表達
// 「這個 session 被伺服器端撤銷了」。判斷交給 Hub 做跨機器比對：同一張票在
// A 上一直在更新、在 B 上 mtime 卡住三天，那 B 大概被踢了。
type Credential struct {
	Provider  string     `json:"provider"` // claude / codex / grok / gemini / openclaw
	Status    CredStatus `json:"status"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// LastRefresh 與 FileMTime 是三元組的另外兩隻腳。
	LastRefresh *time.Time `json:"last_refresh,omitempty"`
	FileMTime   *time.Time `json:"file_mtime,omitempty"`

	// ActiveAccountID：BAT 會熱抽換 ~/.codex/auth.json，只讀 auth.json 會不知道
	// 現在是哪個帳號在用。必須同時讀 codex-accounts.json。
	ActiveAccountID string `json:"active_account_id,omitempty"`
	AccountCount    int    `json:"account_count,omitempty"`

	Note string `json:"note,omitempty"` // 為什麼是 unknown，人看得懂的原因

	// ---------------------------------------------------------------
	// 下面三個欄位存在的唯一目的，是讓「檔案裡的到期時間還沒到」
	// 永遠不會被讀成「這張票能用」。
	//
	// 那兩件事差很遠：本機檔案裡**沒有任何欄位**能表達
	// 「伺服器端把這個 session 踢掉了」。一張被踢掉的票，
	// 它的 auth.json 看起來跟一張好票一模一樣。
	// ---------------------------------------------------------------

	// VerifiedAt：拿這張憑證去打一個**真實請求**、而且成功了的時間。
	//
	// ⚠ 目前永遠是 nil，而那是刻意的。要驗證就得花掉使用者的額度、
	// 而且可能觸發對方的風控。沒做就寫沒做 —— 畫面上會顯示「從未驗證」，
	// 不是留白。留白看起來像「這一格不重要」。
	VerifiedAt *time.Time `json:"verified_at,omitempty"`

	// VerificationMethod 說明上面那個 Status 到底是怎麼得到的。
	// ⚠ 這是整組欄位裡最重要的一個：它把「我讀了檔案」跟「我驗過了」分開。
	VerificationMethod VerifyMethod `json:"verification_method,omitempty"`

	// LastError 是讀這張憑證時遇到的錯誤**原文**。
	//
	// ⚠ 原文，不分類。地基二：不准把自由文字歸類成一個形容詞再拿去做判斷。
	// 「permission denied」跟「no such file」要人自己看，因為它們要做的事不一樣。
	LastError string `json:"last_error,omitempty"`
}

// VerifyMethod：這個憑證狀態是怎麼得到的。
//
// ⚠ 刻意只有兩個值，而且第二個目前不會出現。
// 把它做成列舉而不是自由字串，是為了讓「有沒有真的驗過」變成一個
// 看一眼就知道的事實，而不是一句可以含糊過去的描述。
type VerifyMethod string

const (
	// VerifyFileParse：只讀了本機檔案。
	// 它能回答「檔案在不在」「裡面寫的到期時間到了沒」，
	// 它**答不出**「這張票現在還能不能用」。
	VerifyFileParse VerifyMethod = "file_parse"

	// VerifyLiveRequest：真的打了一個請求並且成功了。
	// ⚠ 目前沒有任何程式碼會產生這個值。留著是為了讓那一天到來時，
	// 畫面與資料庫不用改結構 —— 而在那之前，它的缺席本身就是誠實的。
	VerifyLiveRequest VerifyMethod = "live_request"
)

// CLITool 的版本一律多來源收集。
//
// ⚠ --version 全部不可信（實測 grok 自報 1.0.3、npm 說 1.0.13、
// version.json 說 0.2.118；bat-server 根本沒有 --version）。
// 矛盾時如實回報矛盾，不要挑一個當答案。
type CLITool struct {
	Name string `json:"name"`

	// Present 是「這台機器上有這個工具」。
	//
	// ⚠⚠ 它**不等於**「它在 PATH 上」。2026-09-03 之前這兩件事是同一段程式：
	// LookPath 失敗就直接回 present=false，連 process 清單都不看一眼 ——
	// 即使那份清單當下就握著答案。
	//
	// 實測抓到的：samplehub1 上有一個叫 `agy` 的 process 從 10:59 就在跑
	// （吃了 2 分 23 秒 CPU，OAuth token 13:59 才更新過），而 clawctl
	// 對全機隊四台都說它「沒有安裝」—— 只因為它不在 PATH 上。
	//
	// 一個正在跑的 process 是「它裝了」最強的證據，比 PATH 強得多：
	// PATH 講的是「這個 shell 的環境」，process 講的是「這台機器上的事實」。
	// 拿弱的那個去否決強的那個，就是 §5.10 那個 bug 的形狀。
	Present bool `json:"present"`

	// OnPath 是「叫得動它嗎」。跟 Present 分開存，因為兩者的下一步完全不同：
	// present 但不 on_path 的工具，人跟腳本都沒辦法用名字執行它 ——
	// 而那是一件要寫在畫面上的事，不是一個可以被折疊掉的細節。
	OnPath bool `json:"on_path"`

	// PresentEvidence 是「憑什麼說它裝了」：path 或 process。
	// ⚠ present=true 的時候不准留白。一個沒有來源的斷言沒有辦法被質疑。
	PresentEvidence string `json:"present_evidence,omitempty"`

	Path     string `json:"path,omitempty"`
	RealPath string `json:"realpath,omitempty"`

	// PathSource 是「上面那個 Path，是拿**誰的** PATH 找出來的」。
	//
	//   "login"  人登入 shell 用的那條 PATH —— 「operator 打下去會跑什麼」的答案
	//   "daemon" 退回用 clawctl-agent 自己的 PATH，PathReason 說為什麼
	//   ""       這筆觀測早於這個區分。不准當成 login。
	//
	// ⚠⚠ 這個欄位存在，是因為 2026-09-03 量到的一件事：agent 由 systemd --user
	// 起來，PATH 裡沒有 ~/.local/bin；operator 的登入 shell 裡有。於是同一個名字
	// 在兩邊解到**不同的檔案**，而畫面上顯示的是 daemon 那一邊的版號：
	//
	//   claude    人 2.1.258   畫面 2.1.205
	//   openclaw  人 2026.6.6  畫面 2026.6.1
	//   agy       人 有         畫面「沒有安裝」
	//
	// 那個 2.1.205 不是空的、不是紅的、不是壞掉的 —— 它只是量錯了對象，
	// 而且錯得很像真的。§5.15 的「先問它是哪一個鐘」換成了 PATH：
	// 一個相對於觀測者環境的量測，被當成關於這台機器的事實報出去。
	PathSource string `json:"path_source,omitempty"`
	PathReason string `json:"path_reason,omitempty"`

	// DaemonReach 回答一個跟版號無關、但跟「以後 clawctl 自己去執行它」
	// 完全相關的問題：daemon 伸手拿到的，是不是人打下去的那一個檔案。
	//
	//   ""         沒有可比的兩邊（用的就是 daemon 的 PATH，或人的 PATH 上沒有）
	//   "same"     同一個檔案
	//   "shadowed" 是**另一個**檔案；DaemonPath 就是那一個
	//   "missing"  daemon 的 PATH 上根本沒有，clawctl 用名字叫不動它
	//
	// ⚠ 三種狀態不准折成一個 bool。最危險的是中間那個：兩邊都會成功，
	// 只是做的事不一樣 —— 那種 bug 不會有任何一盞燈亮起來。
	DaemonReach string `json:"daemon_reach,omitempty"`
	DaemonPath  string `json:"daemon_path,omitempty"`

	VersionReported    string `json:"version_reported,omitempty"`
	VersionRaw         string `json:"version_raw,omitempty"`
	VersionPackageJSON string `json:"version_package_json,omitempty"`
	VersionReason      string `json:"version_reason,omitempty"`
	SourcesDisagree    bool   `json:"version_sources_disagree"`

	// RunningPID / RunningExe：裝的跟正在跑的可能不是同一個 binary。
	// 三者不一致時 UI 要寫「裝的是 2.2.0，跑的是 2.1.0」。
	RunningPID int    `json:"running_pid,omitempty"`
	RunningExe string `json:"running_exe,omitempty"`

	// RunningScript：node CLI 真正在跑的那個 .js 進入點。
	//
	// ⚠ 對這些工具來說，RunningExe 一律是 /usr/bin/node，它不回答任何問題。
	// 要比「裝的跟跑的是不是同一份」，只能比這個。
	// 實測 samplehub1：裝的是 /usr/lib/node_modules/openclaw/openclaw.mjs，
	// 跑的是 /home/example-user/.local/node_modules/openclaw/dist/index.js ——
	// 兩份不同的安裝，而在這個欄位出現之前，畫面上完全看不出來。
	RunningScript string `json:"running_script,omitempty"`

	// ProcessScan 講的是**這一輪 /proc 掃描本身**，不是這個工具；
	// 同一輪觀測裡每個工具的值都相同。
	//
	// ⚠ 它決定 RunningPID == 0 能不能拿來當「它不在」的判決。
	// ⚠ 空字串代表舊 agent 沒講，Hub 必須當成「不知道」，不准當 complete。
	// ⚠ 三種狀態不准折成一個 bool；restricted 跟 unavailable 的下一步不同。
	// RunningReason 是給人讀的下一步，不是判決依據，不准拿它做字串比對。
	ProcessScan string `json:"process_scan,omitempty"`

	// RunningReason：沒找到 process 的時候，說明**是哪一種沒找到**。
	//
	// ⚠ 「掃了 300 個 process，沒有一個是它」跟「我根本讀不到 /proc」
	// 都會讓 RunningPID 是 0，但意思相反：前者是關於世界的事實，
	// 後者是關於我自己的事實。少了這個欄位，Hub 沒有辦法拒絕
	// 在第二種情況下做判決 —— 而那就是拿「我不知道」當「它不在」。
	RunningReason string `json:"running_reason,omitempty"`

	// Support 是這個 adapter 對**它自己**的評語：「我看得懂我拿到的東西嗎」。
	//
	// ⚠ unsupported 不是說這個 CLI 壞了，是說「這台上裝的版本我沒見過，
	// 我從它的輸出裡取不出版本號」。這個分別很要緊 —— 寫成工具壞了，
	// 人就會去修一個沒壞的東西。
	Support SupportLevel `json:"support,omitempty"`
}

// SupportLevel：adapter 承認自己認不認得眼前這個東西。
//
// ⚠ 刻意**不做**「已知版本清單」。那種清單會在下一個 release 出來的隔天
// 開始把每一台好機器標成 unsupported —— 也就是 docs/PHASE1.md §5.1 那盞
// 永遠亮著的黃燈，三天後沒有人再看它。這裡只在**真的解析失敗**時才標，
// 而「解析失敗」是一個當場可驗證的事實，不是一個會過期的猜測。
type SupportLevel string

const (
	// SupportOK：解析成功，下面的結構化欄位可以信。
	SupportOK SupportLevel = "supported"

	// SupportUnsupported：東西拿到了，但這個 adapter 讀不懂它。
	//
	// ⚠ 標了這個之後，**原始素材必須還在**（binary 路徑、--version 原文、
	// 找到的檔案路徑）。一個說「我不支援」卻把證據丟掉的 adapter，
	// 等於要人從零開始重查一次 —— 而它明明手上有。
	SupportUnsupported SupportLevel = "unsupported"
)

// CLITool.PathSource / CLITool.DaemonReach / CLITool.ProcessScan 的值。
// 字串常數而不是 iota，
// 是因為它們會被序列化進 observed_state 的 payload 躺很久 ——
// 一個數字在資料庫裡三十天之後沒有人記得它是什麼意思。
const (
	PathSourceLogin  = "login"
	PathSourceDaemon = "daemon"

	DaemonReachSame     = "same"
	DaemonReachShadowed = "shadowed"
	DaemonReachMissing  = "missing"

	ProcessScanComplete    = "complete"
	ProcessScanRestricted  = "restricted"
	ProcessScanUnavailable = "unavailable"
)

// ---------------------------------------------------------------- errors

// APIError 是 Hub 回給 Agent 的錯誤。Code 是給程式看的，Message 是給人看的。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	ErrBadSchemaVersion        = "BAD_SCHEMA_VERSION"
	ErrUnauthorized            = "UNAUTHORIZED"
	ErrHubBusy                 = "HUB_BUSY"
	ErrUnknownMachine          = "UNKNOWN_MACHINE"
	ErrEnrollTokenBad          = "ENROLL_TOKEN_INVALID"
	ErrJobNotFound             = "JOB_NOT_FOUND"
	ErrArtifactNotFound        = "ARTIFACT_NOT_FOUND"
	ErrLeaseInvalid            = "LEASE_INVALID"
	ErrNoVerification          = "NO_VERIFICATION"
	ErrVerificationFailed      = "VERIFICATION_FAILED"
	ErrJobStateConflict        = "JOB_STATE_CONFLICT"
	ErrJobEventConflict        = "JOB_EVENT_CONFLICT"
	ErrJobVerificationConflict = "JOB_VERIFICATION_CONFLICT"
	// ErrIdentityConflict：兩台機器帶著同一個身分來報到（通常是複製了資料目錄）。
	// ⚠ 不合併、不覆蓋、兩列都留著，並且都標成衝突 —— 否則你會看到一台機器
	// 的狀態在兩個真實機器之間跳動，而且永遠查不出為什麼。
	ErrIdentityConflict = "IDENTITY_CONFLICT"
)

// VerificationAssignmentsResponse is the verifier plane's only read. It answers
// exactly one question — which jobs was I told to look at — and it is scoped by
// the caller's own credential.
//
// ⚠ It carries no rule, no command and no artifact digest, and it must never
// grow one. A verifier that ran Hub-supplied commands against a fleet machine
// would be a remote execution channel gated by a single bearer, not a second
// independent judgement. The rules live in the verifier's own code; the Hub
// only names the job.
type VerificationAssignmentsResponse struct {
	SchemaVersion int                      `json:"schema_version"`
	Assignments   []VerificationAssignment `json:"assignments"`
}

// VerificationAssignment names one job and the machine it ran on. MachineName
// is a display value; MachineID is the stable key a verifier resolves against
// its own configuration to decide how to reach the target.
type VerificationAssignment struct {
	AssignmentID string    `json:"assignment_id"`
	JobID        string    `json:"job_id"`
	MachineID    string    `json:"machine_id"`
	MachineName  string    `json:"machine_name"`
	AssignedAt   time.Time `json:"assigned_at"`
}

// Fleet-peer verifier rule IDs are shared identities, not Hub-supplied
// commands. The verifier still owns the command behind each rule; the Hub only
// uses these names to decide whether one assigned report is complete enough to
// participate in the stable promotion gate.
const (
	IndependentRuleOpenClawCurrentRelease = "openclaw.current_release"
	IndependentRuleOpenClawGatewayHTTP    = "openclaw.gateway_http"
	IndependentRuleOpenClawUnitState      = "openclaw.unit_state"
	IndependentFleetPeerRequiredRules     = 3
	// IndependentVerificationSchemaVersion is separate from the machine-agent
	// protocol version. Version 2 adds the current-release rule's structured
	// observed_version; changing that write contract must not silently change
	// check-in or assignment envelopes that still use SchemaVersion.
	IndependentVerificationSchemaVersion = 2
)

// IndependentVerificationRequest is the verifier plane's only write. It
// deliberately has no lease_token and no machine_id: the job supplies the
// machine, and this producer never holds a job lease.
type IndependentVerificationRequest struct {
	SchemaVersion   int       `json:"schema_version"`
	JobID           string    `json:"job_id"`
	RuleID          string    `json:"rule_id"`
	Command         string    `json:"command"`
	ExitCode        int       `json:"exit_code"`
	StdoutExcerpt   string    `json:"stdout_excerpt"`
	StderrExcerpt   string    `json:"stderr_excerpt"`
	ObservedDigest  string    `json:"observed_digest"`
	ObservedVersion string    `json:"observed_version"`
	Passed          bool      `json:"passed"`
	VerifiedAt      time.Time `json:"verified_at"`
}
