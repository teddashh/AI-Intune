package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// enrollmentDenominatorPredicate 是「這一列算不算分母」的唯一判準。
//
// ⚠ 它是一個共用常數而不是各查各的，因為註冊報告上的分母跟註冊上限擋人時數的那個
// 數字必須是同一個。兩邊各寫一次的話，畫面會說「5/5 已滿」而開票照樣過，或者反過來
// ——開票被擋，畫面卻說還有空位。欄位固定寫成 m.retired_at。
//
// 退役是離開分母的唯一一條路（見 CreateEnrollTokenFor 上面那段）。撤票、票過期都
// 不會讓一列名冊消失，所以它們也不會把一個位子讓出來。
const enrollmentDenominatorPredicate = `m.retired_at IS NULL`

// MaxEnrollmentLimitMachines 是這個上限設得了多大。
//
// 上限的意義是「說出這個機隊應該多大」，一個大到沒有意義的數字跟不設上限沒有差別，
// 但它會在畫面上假裝有一道防線。
const MaxEnrollmentLimitMachines = 10000

const (
	OperatorCodeEnrollmentLimitInvalid      = "ENROLLMENT_LIMIT_INVALID"
	OperatorCodeEnrollmentLimitPreviewStale = "ENROLLMENT_LIMIT_PREVIEW_STALE"
	OperatorCodeEnrollmentLimitReached      = "ENROLLMENT_LIMIT_REACHED"
)

const (
	operatorEnrollmentLimitOperation = "enrollment-limit-set:v1"
	operatorEnrollmentLimitVersion   = "v1"
)

// EnrollmentLimitState 是「現在有幾台、上限幾台」這一個問題的完整答案。
type EnrollmentLimitState struct {
	// Set 說有沒有設上限。沒設的時候 MaxMachines 沒有意義。
	Set bool `json:"set"`
	// MaxMachines 是上限。0 是合法的：誰都不准再納管。
	MaxMachines int `json:"max_machines"`
	// Revision 是這個上限被改過幾次；沒設過就是 0。
	Revision  int64     `json:"revision"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
	// InDenominator 是名冊上沒有退役的列數，也就是註冊報告上的分母。
	InDenominator int `json:"in_denominator"`
	// Retired 是已退役的列數，它們不算分母。
	Retired int `json:"retired"`
	// Headroom 是還可以再納管幾台。沒設上限的時候沒有意義。
	Headroom int `json:"headroom"`
	// AtLimit 說現在還開不開得了票。
	AtLimit bool `json:"at_limit"`
}

// countEnrollmentDenominator 數分母。
//
// ⚠ 它跟註冊報告共用 enrollmentDenominatorPredicate，不是自己寫一次條件。
func countEnrollmentDenominator(q operatorRowQuerier) (int, int, error) {
	var inDenominator, retired int
	if err := q.QueryRow(`SELECT
	 COALESCE(SUM(CASE WHEN `+enrollmentDenominatorPredicate+` THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN `+enrollmentDenominatorPredicate+` THEN 0 ELSE 1 END),0)
	 FROM machine_registry m`).Scan(&inDenominator, &retired); err != nil {
		return 0, 0, fmt.Errorf("store: count enrollment denominator: %w", err)
	}
	return inDenominator, retired, nil
}

func readEnrollmentLimit(q operatorRowQuerier) (EnrollmentLimitState, error) {
	var state EnrollmentLimitState
	var updatedAt string
	var limitSet int
	err := q.QueryRow(`SELECT limit_set,max_machines,revision,reason,updated_at,updated_by
	 FROM enrollment_limit WHERE singleton=1`).Scan(
		&limitSet, &state.MaxMachines, &state.Revision, &state.Reason, &updatedAt, &state.UpdatedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 從來沒設定過。revision 0 是它唯一會出現的地方。
		return EnrollmentLimitState{}, nil
	case err != nil:
		return EnrollmentLimitState{}, fmt.Errorf("store: read enrollment limit: %w", err)
	}
	state.Set, state.UpdatedAt = limitSet == 1, parseTime(updatedAt)
	// 取消上限之後那個台數不再有意義，不可以讓它從欄位裡漏到畫面上。
	if !state.Set {
		state.MaxMachines = 0
	}
	return state, nil
}

func enrollmentLimitState(q operatorRowQuerier) (EnrollmentLimitState, error) {
	state, err := readEnrollmentLimit(q)
	if err != nil {
		return EnrollmentLimitState{}, err
	}
	state.InDenominator, state.Retired, err = countEnrollmentDenominator(q)
	if err != nil {
		return EnrollmentLimitState{}, err
	}
	if state.Set {
		state.Headroom = state.MaxMachines - state.InDenominator
		if state.Headroom < 0 {
			state.Headroom = 0
		}
		state.AtLimit = state.InDenominator >= state.MaxMachines
	}
	return state, nil
}

// EnrollmentLimit 回「現在有幾台、上限幾台、還開不開得了票」。
func (s *Store) EnrollmentLimit() (EnrollmentLimitState, error) {
	return enrollmentLimitState(s.db)
}

// EnrollmentLimitPreviewDigest 把「這份預覽是照著哪一版上限做的」釘進去。
//
// ⚠ 它 digest 的是 revision 與要改成什麼，**不** digest 現在有幾台。分母每分鐘都
// 在變，把它放進去等於每一份預覽在按下送出之前就過期了。誰改得動上限才是預覽要
// 擋的那件事：兩個人同時把上限往兩個方向改，後送出的那一個必須重看一次。
//
// ⚠ 這裡也不放「現在的上限是幾台、是第幾版」。revision 只往前走，而送出時的
// ExpectedRevision 已經逐字比對過它——把同一件事再 digest 一次，只會多一個遲早
// 跟它對不上的地方。這個 digest 回答的是另一個問題：你送出的，是不是你看過的
// 那一個改法。
func EnrollmentLimitPreviewDigest(_ EnrollmentLimitState, set bool, maxMachines int) string {
	body := struct {
		Version     string `json:"version"`
		Set         bool   `json:"set"`
		MaxMachines int    `json:"max_machines"`
	}{Version: operatorEnrollmentLimitVersion, Set: set, MaxMachines: maxMachines}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// OperatorEnrollmentLimitPreviewResult 說「照這樣改，會變成什麼」。
type OperatorEnrollmentLimitPreviewResult struct {
	Current EnrollmentLimitState `json:"current"`
	// Set/MaxMachines 是要改成什麼。Set=false 表示要取消上限。
	Set         bool `json:"set"`
	MaxMachines int  `json:"max_machines"`
	// AlreadyOver 說改完之後現在的台數就已經超過新上限。這不會退役任何一台，
	// 它只是讓下一次開票被擋。
	AlreadyOver   bool      `json:"already_over"`
	PreviewedAt   time.Time `json:"previewed_at"`
	PreviewDigest string    `json:"preview_digest"`
}

type OperatorEnrollmentLimitRequest struct {
	// Set=false 表示取消上限。
	Set              bool
	MaxMachines      int
	Reason           string
	ExpectedRevision int64
	PreviewDigest    string
	IdempotencyKey   string
	RequestDigest    string
	UpdatedBy        string
	Audit            AuditEntry
}

type OperatorEnrollmentLimitResult struct {
	State EnrollmentLimitState `json:"state"`
	// PreviousSet/PreviousMax 是改之前的樣子。
	PreviousSet bool `json:"previous_set"`
	PreviousMax int  `json:"previous_max"`
	Replayed    bool `json:"replayed"`
	Audited     bool `json:"-"`
}

type operatorEnrollmentLimitReceipt struct {
	ReceiptVersion string    `json:"receipt_version"`
	Set            bool      `json:"set"`
	MaxMachines    int       `json:"max_machines"`
	PreviousSet    bool      `json:"previous_set"`
	PreviousMax    int       `json:"previous_max"`
	Revision       int64     `json:"revision"`
	AppliedAt      time.Time `json:"applied_at"`
}

// PreviewOperatorEnrollmentLimit 驗的是跟 commit 同一套意圖，不寫任何東西。
func (s *Store) PreviewOperatorEnrollmentLimit(set bool, maxMachines int) (OperatorEnrollmentLimitPreviewResult, error) {
	if err := validateEnrollmentLimitIntent(set, maxMachines); err != nil {
		return OperatorEnrollmentLimitPreviewResult{}, err
	}
	current, err := s.EnrollmentLimit()
	if err != nil {
		return OperatorEnrollmentLimitPreviewResult{}, err
	}
	return OperatorEnrollmentLimitPreviewResult{
		Current: current, Set: set, MaxMachines: maxMachines,
		AlreadyOver:   set && current.InDenominator > maxMachines,
		PreviewedAt:   s.now().UTC(),
		PreviewDigest: EnrollmentLimitPreviewDigest(current, set, maxMachines),
	}, nil
}

func validateEnrollmentLimitIntent(set bool, maxMachines int) error {
	if !set {
		return nil
	}
	if maxMachines < 0 || maxMachines > MaxEnrollmentLimitMachines {
		return operatorError(OperatorCodeEnrollmentLimitInvalid,
			fmt.Sprintf("上限必須介於 0 與 %d 之間", MaxEnrollmentLimitMachines))
	}
	return nil
}

func canonicalEnrollmentLimitRejection(code, detail string) bool {
	switch code {
	case OperatorCodeEnrollmentLimitInvalid:
		return detail == fmt.Sprintf("上限必須介於 0 與 %d 之間", MaxEnrollmentLimitMachines)
	case OperatorCodeReasonRequired:
		return detail == "reason 不可省略，最多 500 bytes"
	case OperatorCodeEnrollmentLimitPreviewStale:
		return detail == "上限已經被別人改過；請重新預覽"
	case OperatorCodePreconditionFailed:
		return detail == "上限 revision 已變更；請重新預覽"
	default:
		return false
	}
}

// ApplyOperatorEnrollmentLimit 是改上限的唯一入口。
//
// ⚠ 讀「現在的上限」與寫新的上限在同一個 writer transaction 裡，因為預覽的
// precondition 就是那個讀出來的 revision。分兩次讀寫的話，兩個人同時改會有一個人的
// 判斷是照著另一個人已經覆蓋掉的狀態做的。
func (s *Store) ApplyOperatorEnrollmentLimit(req OperatorEnrollmentLimitRequest) (OperatorEnrollmentLimitResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorEnrollmentLimitResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorEnrollmentLimitResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorEnrollmentLimitResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditEnrollmentLimit
	audit.Subject = enrollmentLimitSubject(req.Set, req.MaxMachines)
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorEnrollmentLimitResult{}, fmt.Errorf("store: begin enrollment limit: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	switch {
	case err == nil:
		return s.replayOperatorEnrollmentLimit(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorEnrollmentLimitResult{}, fmt.Errorf("store: inspect enrollment limit idempotency key: %w", err)
	}

	reject := func(code, detail string) (OperatorEnrollmentLimitResult, error) {
		if !canonicalEnrollmentLimitRejection(code, detail) {
			return OperatorEnrollmentLimitResult{}, errors.New("store: invalid enrollment limit rejection")
		}
		audit.OK, audit.Detail = false, detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorEnrollmentLimitOperation,
			req.RequestDigest, code, detail, fmtTime(writerNow)); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorEnrollmentLimitResult{}, rejection
	}

	if validationErr := validateEnrollmentLimitIntent(req.Set, req.MaxMachines); validationErr != nil {
		var rejection *OperatorRequestError
		if !errors.As(validationErr, &rejection) {
			return OperatorEnrollmentLimitResult{}, validationErr
		}
		return reject(rejection.Code, rejection.Detail)
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > 500 {
		return reject(OperatorCodeReasonRequired, "reason 不可省略，最多 500 bytes")
	}

	current, err := enrollmentLimitState(tx)
	if err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	if req.ExpectedRevision != current.Revision {
		return reject(OperatorCodePreconditionFailed, "上限 revision 已變更；請重新預覽")
	}
	if req.PreviewDigest == "" ||
		req.PreviewDigest != EnrollmentLimitPreviewDigest(current, req.Set, req.MaxMachines) {
		return reject(OperatorCodeEnrollmentLimitPreviewStale, "上限已經被別人改過；請重新預覽")
	}

	// ⚠ 取消上限也是一次寫入，不是刪除：revision 只會往前走，而「誰把上限拿掉、
	// 為什麼」跟設上限一樣留在這一列上。
	revision, maxMachines, limitSet := current.Revision+1, req.MaxMachines, 0
	if req.Set {
		limitSet = 1
	} else {
		maxMachines = 0
	}
	if _, err := tx.Exec(`INSERT INTO enrollment_limit
	 (singleton,limit_set,max_machines,revision,reason,updated_at,updated_by) VALUES (1,?,?,?,?,?,?)
	 ON CONFLICT(singleton) DO UPDATE SET
	   limit_set=excluded.limit_set, max_machines=excluded.max_machines, revision=excluded.revision,
	   reason=excluded.reason, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		limitSet, maxMachines, revision, reason, fmtTime(writerNow), req.UpdatedBy); err != nil {
		return OperatorEnrollmentLimitResult{}, fmt.Errorf("store: write enrollment limit: %w", err)
	}

	applied, err := enrollmentLimitState(tx)
	if err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	result := OperatorEnrollmentLimitResult{
		State: applied, PreviousSet: current.Set, PreviousMax: current.MaxMachines,
	}
	receipt := operatorEnrollmentLimitReceipt{
		ReceiptVersion: operatorEnrollmentLimitVersion, Set: req.Set, MaxMachines: req.MaxMachines,
		PreviousSet: current.Set, PreviousMax: current.MaxMachines,
		Revision: revision, AppliedAt: writerNow,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorEnrollmentLimitOperation,
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	audit.OK, audit.Detail = true, enrollmentLimitSuccessAuditDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	result.Audited = true
	return result, nil
}

func (s *Store) replayOperatorEnrollmentLimit(tx *sql.Tx, req OperatorEnrollmentLimitRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorEnrollmentLimitResult, error) {
	if cached.Operation != operatorEnrollmentLimitOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorEnrollmentLimitResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!canonicalEnrollmentLimitRejection(cached.ErrorCode.String, cached.ErrorDetail.String) ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidEnrollmentLimitCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + cached.ErrorDetail.String
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollmentLimitResult{}, err
		}
		return OperatorEnrollmentLimitResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: cached.ErrorDetail.String,
			Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid ||
		cached.ErrorCode.Valid || cached.ErrorDetail.Valid ||
		!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidEnrollmentLimitCache(tx, audit)
	}
	var receipt operatorEnrollmentLimitReceipt
	if err := json.Unmarshal([]byte(cached.ResponseJSON.String), &receipt); err != nil ||
		receipt.ReceiptVersion != operatorEnrollmentLimitVersion || receipt.Revision <= 0 ||
		receipt.Set != req.Set || receipt.MaxMachines != req.MaxMachines {
		return s.rejectInvalidEnrollmentLimitCache(tx, audit)
	}
	// ⚠ 回放讀的是現在的狀態，不是收據裡那一份。收據說的是「當時改成什麼」，而呼叫端
	// 拿這個結果去畫畫面；把當時的數字重播成現在，畫面會停在一個已經不成立的上限上。
	state, err := enrollmentLimitState(tx)
	if err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	audit.OK, audit.Detail = true, OperatorIdempotencyReplayPrefix+"沒有再次變更註冊上限"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	return OperatorEnrollmentLimitResult{
		State: state, PreviousSet: receipt.PreviousSet, PreviousMax: receipt.PreviousMax,
		Replayed: true, Audited: true,
	}, nil
}

func (s *Store) rejectInvalidEnrollmentLimitCache(tx *sql.Tx, audit AuditEntry) (OperatorEnrollmentLimitResult, error) {
	detail := "enrollment limit idempotency cache invalid；未回放結果"
	audit.OK, audit.Detail = false, detail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollmentLimitResult{}, err
	}
	rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
	rejection.Audited = true
	return OperatorEnrollmentLimitResult{}, rejection
}

func enrollmentLimitSubject(set bool, maxMachines int) string {
	if !set {
		return "取消上限"
	}
	return fmt.Sprintf("上限 %d 台", maxMachines)
}

func enrollmentLimitSuccessAuditDetail(receipt operatorEnrollmentLimitReceipt) string {
	previous := "原本沒有上限"
	if receipt.PreviousSet {
		previous = fmt.Sprintf("原本上限 %d 台", receipt.PreviousMax)
	}
	now := "取消上限"
	if receipt.Set {
		now = fmt.Sprintf("上限 %d 台", receipt.MaxMachines)
	}
	return truncAudit(fmt.Sprintf("%s；改成%s（revision %d）", previous, now, receipt.Revision), auditMaxReason)
}

// enrollmentLimitReachedDetail 講的是這個判決當下的事實，不是一句通則。
//
// 兩個下一步都是操作員自己做得到的：退役一台不用的，或把上限調高。
func enrollmentLimitReachedDetail(limit EnrollmentLimitState) string {
	return fmt.Sprintf("名冊上已有 %d 台（不含已退役 %d 台），上限 %d 台；"+
		"請先退役不用的機器，或把上限調高",
		limit.InDenominator, limit.Retired, limit.MaxMachines)
}
