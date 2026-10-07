package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 註冊上限：這個 Hub 最多納管幾台。
//
// 它擋的是開票那一刻。一份事後把超額的列標紅的報告救不回任何東西——機器已經進來
// 了，而把它弄出去要退役。所以上限住在 writer transaction 裡，跟開票是同一筆。
//
// ⚠ 上限算的是註冊報告上的那個分母：名冊上沒有退役的列。這裡不另外數一次；
// EnrollmentLimitState 已經是 store 用同一條 predicate 數出來的。
type EnrollmentLimitState = store.EnrollmentLimitState

// EnrollmentLimitResult 是「上限現在長什麼樣」這一頁。
type EnrollmentLimitResult struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedAt   time.Time            `json:"generated_at"`
	State         EnrollmentLimitState `json:"state"`
	// Headline 是這一頁最上面那一句，畫面、CLI、client 都印同一句。
	Headline string `json:"headline"`
	// NextStep 是現在該做什麼。沒有事情要做的時候是空的。
	NextStep string `json:"next_step"`
}

// EnrollmentLimitSchemaVersion 釘住 client 讀得懂的版本。
const EnrollmentLimitSchemaVersion = 1

// MaxEnrollmentLimitMachines 是上限本身的上限。
const MaxEnrollmentLimitMachines = store.MaxEnrollmentLimitMachines

var ErrInvalidEnrollmentLimit = errors.New("operator: invalid enrollment limit request")

type EnrollmentLimitPreviewRequest struct {
	Set         bool `json:"set"`
	MaxMachines int  `json:"max_machines"`
}

type EnrollmentLimitPreview struct {
	store.OperatorEnrollmentLimitPreviewResult
	// Headline 講的是按下去會發生什麼，不是現在的狀態。
	Headline string `json:"headline"`
	// ExpectedRevision 是這份預覽看到的版本，送出時要帶回來。
	ExpectedRevision int64 `json:"expected_revision"`
}

type EnrollmentLimitApplyRequest struct {
	EnrollmentLimitPreviewRequest
	Reason           string
	ExpectedRevision int64
	PreviewDigest    string
	IdempotencyKey   string
	Actor            Actor
}

type EnrollmentLimitApplyResult = store.OperatorEnrollmentLimitResult

type EnrollmentLimitTransportRejectionCode string

const (
	EnrollmentLimitTransportRejectionFormInvalid     EnrollmentLimitTransportRejectionCode = "ENROLLMENT_LIMIT_FORM_INVALID"
	EnrollmentLimitTransportRejectionRevisionInvalid EnrollmentLimitTransportRejectionCode = "ENROLLMENT_LIMIT_FORM_REVISION_INVALID"
)

type EnrollmentLimitTransportRejectionRequest struct {
	Code           EnrollmentLimitTransportRejectionCode
	IdempotencyKey string
	Actor          Actor
}

// EnrollmentLimit reads the limit and what it currently allows.
func (s *Service) EnrollmentLimit(now time.Time) (EnrollmentLimitResult, error) {
	state, err := s.store.EnrollmentLimit()
	if err != nil {
		return EnrollmentLimitResult{}, err
	}
	return EnrollmentLimitResult{
		SchemaVersion: EnrollmentLimitSchemaVersion, GeneratedAt: now.UTC().Truncate(time.Second),
		State: state, Headline: EnrollmentLimitHeadline(state), NextStep: enrollmentLimitNextStep(state),
	}, nil
}

// EnrollmentLimitHeadline 是這個上限現在的一句話。
//
// ⚠ 沒有設上限要講成「沒有上限」，不可以講成「上限 0」。上限 0 是一個真的可以設
// 的值，意思是誰都不准再納管——兩句話在畫面上是相反的意思。
func EnrollmentLimitHeadline(state EnrollmentLimitState) string {
	if !state.Set {
		return fmt.Sprintf("沒有設註冊上限；名冊上 %d 台（已退役 %d 台不算）。",
			state.InDenominator, state.Retired)
	}
	if state.AtLimit {
		return fmt.Sprintf("上限 %d 台，名冊上已有 %d 台（已退役 %d 台不算）——現在開不了新的票。",
			state.MaxMachines, state.InDenominator, state.Retired)
	}
	return fmt.Sprintf("上限 %d 台，名冊上 %d 台（已退役 %d 台不算），還可以再納管 %d 台。",
		state.MaxMachines, state.InDenominator, state.Retired, state.Headroom)
}

func enrollmentLimitNextStep(state EnrollmentLimitState) string {
	if !state.Set || !state.AtLimit {
		return ""
	}
	return "要再納管就先退役不用的機器，或把上限調高。"
}

// PreviewEnrollmentLimit says what changing the limit would do.
func (s *Service) PreviewEnrollmentLimit(req EnrollmentLimitPreviewRequest) (EnrollmentLimitPreview, error) {
	preview, err := s.store.PreviewOperatorEnrollmentLimit(req.Set, req.MaxMachines)
	if err != nil {
		return EnrollmentLimitPreview{}, err
	}
	return EnrollmentLimitPreview{
		OperatorEnrollmentLimitPreviewResult: preview,
		Headline:                             enrollmentLimitPreviewHeadline(preview),
		ExpectedRevision:                     preview.Current.Revision,
	}, nil
}

// enrollmentLimitPreviewHeadline 講的是按下去之後會怎樣，包含它擋不住已經在名冊
// 上的那幾台。
func enrollmentLimitPreviewHeadline(preview store.OperatorEnrollmentLimitPreviewResult) string {
	current := preview.Current
	if !preview.Set {
		if !current.Set {
			return "現在就沒有註冊上限，這次送出不會改變任何事。"
		}
		return fmt.Sprintf("取消上限（現在是 %d 台）；之後開票不再受台數限制。", current.MaxMachines)
	}
	if preview.AlreadyOver {
		return fmt.Sprintf("上限設成 %d 台，名冊上已有 %d 台——不會退役任何一台，"+
			"但在退役到 %d 台以下之前開不了新的票。",
			preview.MaxMachines, current.InDenominator, preview.MaxMachines)
	}
	return fmt.Sprintf("上限設成 %d 台，名冊上 %d 台，之後還可以再納管 %d 台。",
		preview.MaxMachines, current.InDenominator, preview.MaxMachines-current.InDenominator)
}

// SetEnrollmentLimit changes the limit.
//
// ⚠ 它跟開票一樣不在進 Store 之前先跑一次今天的驗證：同一把 idempotency key 必須
// 回放得出當時的判決。Store 只在 cache miss 時驗，而且跟寫入、稽核同一筆交易。
func (s *Service) SetEnrollmentLimit(req EnrollmentLimitApplyRequest) (EnrollmentLimitApplyResult, error) {
	digest := EnrollmentLimitSemanticDigest(req)
	audit := auditFromActor(req.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, digest
	updatedBy := firstNonEmpty(req.Actor.AuthSubject, req.Actor.WhoUser, req.Actor.WhoNode, req.Actor.SourceAddr, "operator")
	result, err := s.store.ApplyOperatorEnrollmentLimit(store.OperatorEnrollmentLimitRequest{
		Set: req.Set, MaxMachines: req.MaxMachines, Reason: strings.TrimSpace(req.Reason),
		ExpectedRevision: req.ExpectedRevision, PreviewDigest: req.PreviewDigest,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, UpdatedBy: updatedBy, Audit: audit,
	})
	if err != nil {
		alreadyAudited := result.Audited
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) {
			alreadyAudited = rejection.Audited
		}
		if !alreadyAudited {
			audit.Action = store.AuditEnrollmentLimit
			audit.Subject, audit.OK, audit.Detail = enrollmentLimitAuditSubject(req), false, err.Error()
			if auditErr := s.store.RecordAudit(audit); auditErr != nil {
				log.Printf("operator enrollment limit audit write failed: %v", auditErr)
			}
		}
	}
	return result, err
}

// RecordEnrollmentLimitTransportRejection owns the audit shape for a Web form
// that could not become a canonical enrollment-limit request. It deliberately
// leaves request digest and reason empty and does not occupy an idempotency key.
func (s *Service) RecordEnrollmentLimitTransportRejection(req EnrollmentLimitTransportRejectionRequest) error {
	switch req.Code {
	case EnrollmentLimitTransportRejectionFormInvalid,
		EnrollmentLimitTransportRejectionRevisionInvalid:
	default:
		return fmt.Errorf("operator: invalid enrollment limit transport rejection code %q", req.Code)
	}
	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditEnrollmentLimit
	entry.Subject = "註冊上限"
	entry.IdempotencyKey = req.IdempotencyKey
	entry.Detail = store.OperatorTransportRejectionPrefix + string(req.Code) + ": canonical request digest 無法取得"
	return s.store.RecordAudit(entry)
}

func enrollmentLimitAuditSubject(req EnrollmentLimitApplyRequest) string {
	if !req.Set {
		return "取消上限"
	}
	return fmt.Sprintf("上限 %d 台", req.MaxMachines)
}

// EnrollmentLimitSemanticDigest digests what the operator asked for, not how it
// was spelled on the wire.
func EnrollmentLimitSemanticDigest(req EnrollmentLimitApplyRequest) string {
	body := struct {
		Set              bool   `json:"set"`
		MaxMachines      int    `json:"max_machines"`
		ExpectedRevision int64  `json:"expected_revision"`
		PreviewDigest    string `json:"preview_digest"`
		Reason           string `json:"reason"`
	}{
		Set: req.Set, MaxMachines: req.MaxMachines, ExpectedRevision: req.ExpectedRevision,
		PreviewDigest: req.PreviewDigest, Reason: strings.TrimSpace(req.Reason),
	}
	// 沒有設上限時 MaxMachines 沒有意義，不可以讓它把兩個一樣的請求 digest 成兩個。
	if !body.Set {
		body.MaxMachines = 0
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
