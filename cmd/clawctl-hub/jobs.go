package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

const jobLeaseDuration = 2 * time.Minute

// hubNow 是 Hub 對工作單蓋時間戳的唯一時鐘來源；測試可換成固定時鐘。
var hubNow = time.Now

func jobNow() time.Time {
	return hubNow().UTC().Truncate(time.Second)
}

func (h *hub) handleNextJob(w http.ResponseWriter, r *http.Request, machineID string) {
	var req struct{}
	if !decodeJobRequest(w, r, &req) {
		return
	}
	// 合規性動作生效中的機器領不到新工作單。這是判決的後果，不是一個存下來的
	// 旗標：它跟判決一樣現算，所以機器一恢復就會自己再領得到。
	blocked, err := h.store.ComplianceBlocksJobs(machineID)
	if err != nil {
		writeJobInternalError(w, "讀取合規性動作", err)
		return
	}
	if blocked {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	job, ok, err := h.store.NextJobForMachine(machineID)
	if err != nil {
		writeJobInternalError(w, "讀取下一張工作單", err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// ⚠ 工作單指到的期望狀態一定存在（FK 擋著）；讀不到是 Hub 自己的 bug，是 5xx。
	ds, err := h.store.DesiredState(job.DesiredID)
	if err != nil {
		writeJobInternalError(w, "讀取工作單的期望狀態", err)
		return
	}
	writeJSON(w, http.StatusOK, model.JobResponse{
		ResourceKind:     ds.ResourceKind,
		ResourceID:       ds.ResourceID,
		Spec:             json.RawMessage(ds.Spec),
		JobID:            job.JobID,
		MachineID:        job.MachineID,
		DesiredID:        job.DesiredID,
		Revision:         int64(job.Revision),
		State:            string(job.State),
		ExecutionTimeout: job.ExecutionTimeout,
		ArtifactDigest:   job.ArtifactDigest,
		Irreversible:     job.Irreversible,
	})
}

func (h *hub) handleClaimJob(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobClaimRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	now := jobNow()
	jobID := r.PathValue("id")
	token, err := h.store.ClaimJob(jobID, machineID, now, jobLeaseDuration)
	if err != nil {
		if errors.Is(err, store.ErrJobNotFound) {
			// ⚠ store 把「不存在」「不是你的」「有人正拿著租約」「已經是終態」
			// 全部回同一個錯，免得領單結果變成列舉 job_id 的工具。這裡用
			// JobForMachine 分開後兩種 —— 它本身就用 machine_id 濾過，所以這台
			// 機器只問得到自己的單；別台問同一個 job_id 仍然是 404。
			if h.replayIfTerminal(w, jobID, machineID) {
				return
			}
			if job, jerr := h.store.JobForMachine(jobID, machineID); jerr == nil {
				// ⚠ 這通常就是 agent 自己：領到了、回應在網路上弄丟、token 跟著丟。
				// 回 404 會讓它以為單消失了；但也不准重新領同一個 job_id，否則
				// seq 從 1 重來會跟第一次執行的事件發生 replay conflict。租約過期後
				// reaper 會收成 lease_expired，下一次 claim 只回放終態。
				writeJSON(w, http.StatusConflict, model.JobStateError{
					APIError: model.APIError{Code: model.ErrJobStateConflict,
						Message: "這張單的租約還沒過期；過期後 Hub 會把它收成 lease_expired，再拉 /next"},
					State:          string(job.State),
					LeaseExpiresAt: job.LeaseExpiresAt,
				})
				return
			}
			writeJobNotFound(w)
			return
		}
		writeJobInternalError(w, "領取工作單", err)
		return
	}
	writeJSON(w, http.StatusOK, model.JobLeaseResponse{
		LeaseToken: token, LeaseExpiresAt: now.Add(jobLeaseDuration),
	})
}

func (h *hub) handleRenewJobLease(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobLeaseRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	now := jobNow()
	if err := h.store.RenewLease(r.PathValue("id"), machineID, req.LeaseToken, now, jobLeaseDuration); err != nil {
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		writeJobInternalError(w, "續租工作單", err)
		return
	}
	writeJSON(w, http.StatusOK, model.JobLeaseResponse{
		LeaseToken: req.LeaseToken, LeaseExpiresAt: now.Add(jobLeaseDuration),
	})
}

func (h *hub) handleJobEvent(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobEventRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	jobID, now := r.PathValue("id"), jobNow()
	// ⚠ received_at 只能來自 Hub。occurred_at 是 Agent 的時鐘，兩者不可混用。
	replayed, err := h.store.AppendJobEventWithReplay(jobID, machineID, req.LeaseToken,
		req.Seq, req.Phase, string(payload), req.OccurredAt, now)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobEvidence) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "事件 seq／phase／payload 大小或 occurred_at 不合法")
			return
		}
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		if errors.Is(err, store.ErrJobEventConflict) {
			writeErr(w, http.StatusConflict, model.ErrJobEventConflict,
				"這個 seq 已經對應另一個事件內容；請不要重用事件序號")
			return
		}
		writeJobInternalError(w, "寫入工作單事件", err)
		return
	}
	// ⚠ 有些階段事件同時是狀態轉移。事件先落地（那是 agent 說了什麼），
	// 轉移再走狀態機（那是 Hub 的結論）—— 轉移被擋，事件仍然是證據。
	// 少了這一段，沒有任何 HTTP 路徑能把單從 claimed 推到 running，
	// 而 /complete 要求 running：真的 agent 永遠 complete 不了。
	if ev, ok := phaseEvents[req.Phase]; ok {
		advance := !replayed
		if replayed {
			job, jerr := h.store.JobForMachine(jobID, machineID)
			if jerr != nil {
				writeJobInternalError(w, "讀取回放階段事件的工作單", jerr)
				return
			}
			// Heal the crash window between the committed event row and its
			// state transition. Once the state has progressed beyond Claimed,
			// the exact event replay has nothing left to apply.
			advance = job.State == deploy.Claimed
		}
		if !advance {
			writeJSON(w, http.StatusAccepted, model.JobEventResponse{AcceptedSeq: req.Seq})
			return
		}
		state, aerr := h.store.AdvanceJobByAgent(jobID, machineID, req.LeaseToken, ev, now)
		// The job can progress between the replay read and transition. Re-read
		// before turning that harmless race into a lease/state error.
		// Preserve the concurrent exact-start case: another overlapping
		// request may have advanced the same committed event first, even when
		// this request was the one whose INSERT reported a new row.
		alreadyProgressed := aerr != nil && ev == deploy.Start &&
			(state == deploy.Running || state == deploy.Verifying || deploy.IsTerminal(state))
		if aerr != nil && ev == deploy.Start && !alreadyProgressed {
			if latest, jerr := h.store.JobForMachine(jobID, machineID); jerr == nil {
				alreadyProgressed = latest.State == deploy.Running || latest.State == deploy.Verifying || deploy.IsTerminal(latest.State)
			}
		}
		if aerr != nil && !alreadyProgressed {
			if errors.Is(aerr, store.ErrLeaseInvalid) {
				writeLeaseInvalid(w)
				return
			}
			writeJobStateError(w, model.ErrJobStateConflict, "這個階段事件跟工作單目前的狀態不合", state)
			return
		}
	}
	writeJSON(w, http.StatusAccepted, model.JobEventResponse{AcceptedSeq: req.Seq})
}

func (h *hub) handleJobVerification(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobVerificationRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	err := h.store.RecordVerification(r.PathValue("id"), machineID, req.LeaseToken,
		req.RuleID, req.Command, req.ExitCode, req.StdoutExcerpt, req.StderrExcerpt,
		req.Passed, req.VerifiedAt)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobEvidence) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "驗證 rule_id／command／output 大小或 verified_at 不合法")
			return
		}
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		if errors.Is(err, store.ErrJobVerificationConflict) {
			writeErr(w, http.StatusConflict, model.ErrJobVerificationConflict,
				"這個 rule_id 已經對應另一組驗證證據；請不要重用驗證規則")
			return
		}
		writeJobInternalError(w, "寫入工作單驗證證據", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *hub) handleCompleteJob(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobCompleteRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	now := jobNow()
	jobID := r.PathValue("id")
	state, err := h.store.AdvanceJobByAgent(jobID, machineID, req.LeaseToken, deploy.FinishWork, now)
	if err != nil && state != deploy.Verifying {
		// ⚠ 終態先於租約：終態是事實，不管誰還拿著租約。回應弄丟後的重送走這裡。
		if h.replayIfTerminal(w, jobID, machineID) {
			return
		}
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		writeJobStateError(w, model.ErrJobStateConflict, "工作單目前的狀態不能回報完成", state)
		return
	}
	// ⚠⚠ err != nil 而 state == verifying，是「complete → 證據不足 → 補證據 →
	// 再 complete」這條**正常**路徑的第二次呼叫：工作單已經在 verifying，
	// FinishWork 對它不合法，但那不是錯 —— 往下讓 Hub 再判一次。
	// （能拿到 state 代表租約在 store 的 SELECT 裡驗過了；租約不對拿到的是空字串。）
	// 少了這一段，一張已經驗證通過的單永遠變不成 succeeded。
	// ⚠ Agent 回報收工只能進 verifying；成功必須由 Hub 查過獨立保存的證據後寫入。
	if state != deploy.Verifying {
		writeJobStateError(w, model.ErrJobStateConflict, "工作單沒有停在等待驗證狀態", state)
		return
	}
	ledgerState, err := h.store.MarkSucceededIfVerified(jobID, now)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoVerification):
			writeJobStateError(w, model.ErrNoVerification,
				"尚未收到任何驗證證據，不能判定成功", deploy.Verifying)
		case errors.Is(err, store.ErrVerificationFailed):
			// ⚠ 有一筆 passed=0 這張單就永遠成功不了（store 的規則），停在 verifying
			// 沒有意義。這是 Hub 看過證據之後下的判決：failed 或 manual_intervention，
			// 由這張單開單時的 irreversible 決定 —— 不可逆的失敗不准被說成已回退。
			// ⚠ 「沒有證據」跟「證據說失敗」是兩件事：前者停在 verifying（上一個 case）。
			job, jerr := h.store.JobForMachine(jobID, machineID)
			if jerr != nil {
				writeJobInternalError(w, "讀取驗證失敗的工作單", jerr)
				return
			}
			if ferr := h.store.FailJob(jobID, job.Irreversible, now); ferr != nil {
				writeJobInternalError(w, "依失敗證據關閉工作單", ferr)
				return
			}
			// ⚠ FailJob 的 UPDATE 排除五個終態，n≠1 時仍可能回 nil（見
			// internal/store/deploy.go:1285-1307），所以「我寫了 failed」與「reaper
			// 先寫了 lease_expired」對呼叫端無法分辨。failed 對人說「已經回到舊版」，
			// lease_expired 則是 Hub 自己的判決、機器現況未知（見
			// internal/deploy/deploy.go:106-110），兩者不准互換。
			ledgerJob, jerr := h.store.JobForMachine(jobID, machineID)
			if jerr != nil {
				writeJobInternalError(w, "讀回依失敗證據關閉的工作單", jerr)
				return
			}
			h.advanceDeployments(now)
			writeJSON(w, http.StatusOK, model.JobStateResponse{
				State:    string(ledgerJob.State),
				Replayed: ledgerJob.State != deploy.OnFailure(job.Irreversible),
			})
		case errors.Is(err, store.ErrNotVerifying):
			writeJobStateError(w, model.ErrJobStateConflict,
				"工作單沒有停在等待驗證狀態", deploy.Verifying)
		case errors.Is(err, store.ErrJobNotFound):
			writeJobNotFound(w)
		default:
			writeJobInternalError(w, "依驗證證據判定工作單", err)
		}
		return
	}
	// ⚠ 不能寫死 succeeded：別人可能已先把這張單收成終態，
	// 否則 agent 會把沒裝成的 revision 記成已套用。
	h.advanceDeployments(now)
	writeJSON(w, http.StatusOK, model.JobStateResponse{
		State:    string(ledgerState),
		Replayed: ledgerState != deploy.Succeeded,
	})
}

func (h *hub) handleRejectJob(w http.ResponseWriter, r *http.Request, machineID string) {
	var req model.JobRejectRequest
	if !decodeJobRequest(w, r, &req) {
		return
	}
	if !validRejectionCode(req.RejectionCode) {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "rejection_code 不是 SPEC §5.2 允許的拒絕碼")
		return
	}
	payload, err := json.Marshal(model.JobRejectEventPayload{
		RejectionCode: req.RejectionCode,
		Detail:        req.Detail,
	})
	if err != nil {
		writeJobInternalError(w, "編碼拒絕事件", err)
		return
	}
	now := jobNow()
	occurredAt := req.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = now
	}
	jobID := r.PathValue("id")
	// 新事件必須在終態前寫入；終態只允許同一 machine 對已存在的
	// canonical event 做 exact replay，不能插入新 seq 或改寫舊內容。
	eventReplayed, err := h.store.AppendJobEventWithReplay(jobID, machineID, req.LeaseToken, req.Seq,
		"rejected", string(payload), occurredAt, now)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobEvidence) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "事件 seq／phase／payload 大小或 occurred_at 不合法")
			return
		}
		if errors.Is(err, store.ErrJobEventConflict) {
			writeErr(w, http.StatusConflict, model.ErrJobEventConflict,
				"這個 seq 已經對應另一個事件內容；請不要重用事件序號")
			return
		}
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		writeJobInternalError(w, "寫入工作單拒絕事件", err)
		return
	}
	// Exact terminal event replay is the only request that may reuse the old
	// reject result. A terminal request with a new seq never reaches here, so it
	// cannot be mislabeled as the original retry.
	if eventReplayed && h.replayIfTerminal(w, jobID, machineID) {
		return
	}
	state, err := h.store.AdvanceJobByAgent(jobID, machineID, req.LeaseToken, deploy.Reject, now)
	if err != nil {
		if h.replayIfTerminal(w, jobID, machineID) {
			return
		}
		if errors.Is(err, store.ErrLeaseInvalid) {
			writeLeaseInvalid(w)
			return
		}
		writeErr(w, http.StatusConflict, model.ErrJobStateConflict, "工作單目前的狀態不能拒絕")
		return
	}
	h.advanceDeployments(now)
	writeJSON(w, http.StatusOK, model.JobStateResponse{State: string(state)})
}

func (h *hub) handleCapabilities(w http.ResponseWriter, r *http.Request, machineID string) {
	var req struct{}
	if !decodeJobRequest(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, model.CapabilitiesResponse{
		SchemaVersion: model.SchemaVersion,
		ResourceKinds: agentadapter.ExecutorKinds(),
	})
}

// decodeJobRequest 先拒絕 body 裡的身分宣告，再解碼真正的 request。
// ⚠ 連 machine_id:null 都拒絕；安靜忽略會讓送出者誤以為身分宣告生效。
func decodeJobRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "JSON 讀取失敗："+err.Error())
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "工作單請求必須是單一 JSON 物件")
		return false
	}
	if _, ok := document.(map[string]any); !ok {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "工作單請求必須是單一 JSON 物件")
		return false
	}
	if containsMachineID(document) {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine_id 只能來自認證身分，不可放在 request body")
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "JSON 欄位格式不正確："+err.Error())
		return false
	}
	return true
}

func containsMachineID(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "machine_id" || containsMachineID(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsMachineID(child) {
				return true
			}
		}
	}
	return false
}

// phaseEvents：哪些階段事件同時是狀態轉移。
// ⚠ 只有 start。finish 走 /complete、reject 走 /reject —— 各有自己的判決邏輯，
// 不准從這張表繞過去。
var phaseEvents = map[string]deploy.Event{
	"start": deploy.Start,
}

func validRejectionCode(code string) bool {
	switch deploy.RejectionCode(code) {
	case deploy.StaleRevision,
		deploy.DuplicateJobID,
		deploy.ArtifactHashMismatch,
		deploy.LeaseInvalid,
		deploy.IrreversibleMigration,
		deploy.PreconditionFailed,
		deploy.UnspecifiedModelSwitch:
		return true
	default:
		return false
	}
}

// replayIfTerminal：工作單已經是終態時把原結果回放給 agent，回 true。
//
// ⚠ SPEC §5.2 DUPLICATE_JOB_ID：「已經是終態 → 回放原結果」。回應在網路上
// 弄丟之後 agent 一定會重送，而一個把重送當成錯誤的協定，會在第一次抖動時
// 把好好的部署標成失敗（store.AppendJobEvent 對 seq 回放講的是同一件事）。
//
// ⚠ 終態先於租約檢查：終態是事實，不管誰還拿著租約。租約過期了的 agent
// 問「我那張單最後怎麼了」，答案不會因為它遲到而改變。
// JobForMachine 用 machine_id 濾過，所以只回放**自己的**單；別台機器問到的
// 仍然是 404，這條路不會變成列舉工具。
func (h *hub) replayIfTerminal(w http.ResponseWriter, jobID, machineID string) bool {
	job, err := h.store.JobForMachine(jobID, machineID)
	if err != nil || !deploy.IsTerminal(job.State) {
		return false
	}
	writeJSON(w, http.StatusOK, model.JobStateResponse{State: string(job.State), Replayed: true})
	return true
}

func writeJobNotFound(w http.ResponseWriter) {
	// ⚠ 不存在與屬於別台機器必須逐字相同，避免用回應列舉 job_id。
	writeErr(w, http.StatusNotFound, model.ErrJobNotFound, "找不到可供這台機器領取的工作單")
}

func writeLeaseInvalid(w http.ResponseWriter) {
	writeErr(w, http.StatusConflict, model.ErrLeaseInvalid, "租約 token 無效、已過期或不屬於這台機器")
}

func writeJobStateError(w http.ResponseWriter, code, message string, state deploy.JobState) {
	writeJSON(w, http.StatusConflict, model.JobStateError{
		APIError: model.APIError{Code: code, Message: message},
		State:    string(state),
	})
}

func writeJobInternalError(w http.ResponseWriter, action string, err error) {
	log.Printf("%s失敗：%v", action, err)
	writeErr(w, http.StatusInternalServerError, "INTERNAL", action+"失敗")
}
