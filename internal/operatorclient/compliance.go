package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// The wire bodies are declared here rather than shared with the Hub package so
// that a field renamed on one side becomes a compile-or-decode failure instead
// of a silently dropped value.

type ComplianceRule struct {
	Kind           string `json:"kind"`
	MaxAgeSeconds  int    `json:"max_age_seconds,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
	MinFreePercent int    `json:"min_free_percent,omitempty"`
}

// ComplianceAction is one consequence of failing the policy.
type ComplianceAction struct {
	Kind         string `json:"kind"`
	GraceSeconds int    `json:"grace_seconds,omitempty"`
}

type CompliancePolicyPreviewRequest struct {
	PolicyID string             `json:"policy_id"`
	Rules    []ComplianceRule   `json:"rules"`
	Actions  []ComplianceAction `json:"actions,omitempty"`
}

type CompliancePolicyPublishRequest struct {
	PolicyID         string             `json:"policy_id"`
	Rules            []ComplianceRule   `json:"rules"`
	Actions          []ComplianceAction `json:"actions,omitempty"`
	ExpectedRevision *int64             `json:"expected_revision"`
	PreviewDigest    string             `json:"preview_digest"`
	ConfirmPolicyID  string             `json:"confirm_policy_id"`
	Reason           string             `json:"reason"`
}

type ComplianceAssignmentPreviewRequest struct {
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id"`
	PolicyID string `json:"policy_id"`
	Revision int64  `json:"policy_revision"`
}

type ComplianceAssignmentRequest struct {
	Scope          string `json:"scope"`
	ScopeID        string `json:"scope_id"`
	PolicyID       string `json:"policy_id"`
	Revision       int64  `json:"policy_revision"`
	PreviewDigest  string `json:"preview_digest"`
	ConfirmScopeID string `json:"confirm_scope_id"`
	Reason         string `json:"reason"`
}

func (c *Client) ComplianceBoard(ctx context.Context) (operator.ComplianceBoardResult, error) {
	var out operator.ComplianceBoardResult
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/compliance", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: compliance board returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "compliance board", &out); err != nil {
		return out, err
	}
	if err := validateComplianceBoard(out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) PreviewCompliancePolicy(ctx context.Context, body CompliancePolicyPreviewRequest) (operator.CompliancePolicyPreviewResult, error) {
	var out operator.CompliancePolicyPreviewResult
	if strings.TrimSpace(body.PolicyID) != body.PolicyID || body.PolicyID == "" {
		return out, errors.New("operator client: compliance policy id 不可空白或含前後空白")
	}
	if len(body.Rules) == 0 {
		return out, errors.New("operator client: compliance policy 至少要有一條規則")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/compliance-policies/preview", "", body)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: compliance policy preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "compliance policy preview", &out); err != nil {
		return out, err
	}
	if out.PolicyID != body.PolicyID || !validSHA256Digest(out.Digest) ||
		!validSHA256Digest(out.PreviewDigest) || out.CurrentRev < 0 ||
		out.Unchanged != (out.NextRev == out.CurrentRev) {
		return out, errors.New("operator client: compliance policy preview identity 不一致")
	}
	// 規則的說明文字跟規則本體必須是同一份，否則畫面說的與送出的會是兩件事。
	if len(out.Rules) != len(out.Policy.Rules) {
		return out, errors.New("operator client: compliance policy preview 的規則說明與規則不符")
	}
	for i, rule := range out.Policy.Rules {
		if out.Rules[i].Kind != rule.Kind || out.Rules[i].Label == "" {
			return out, errors.New("operator client: compliance policy preview 的規則說明與規則不符")
		}
	}
	if err := validateComplianceActionDescriptions(out.Policy, out.Actions); err != nil {
		return out, err
	}
	return out, nil
}

// validateComplianceActionDescriptions checks that the words the console shows
// next to a consequence belong to the consequence actually being published.
// 動作決定的是「機器會不會被停掉工作單」，所以確認頁上那句話跟送出去的那一個
// 動作必須是同一件事。
func validateComplianceActionDescriptions(p compliance.Policy, described []operator.ComplianceAction) error {
	if len(described) != len(p.Actions) {
		return errors.New("operator client: compliance preview 的動作說明與動作不符")
	}
	for i, action := range p.Actions {
		if described[i].Kind != action.Kind || described[i].Label == "" ||
			described[i].Effect == "" || described[i].Grace == "" {
			return errors.New("operator client: compliance preview 的動作說明與動作不符")
		}
	}
	return nil
}

func (c *Client) PublishCompliancePolicy(ctx context.Context, key string, body CompliancePolicyPublishRequest) (store.OperatorCompliancePolicyResult, error) {
	var out store.OperatorCompliancePolicyResult
	if !validSettingIdempotencyKey(key) || body.ExpectedRevision == nil || *body.ExpectedRevision < 0 ||
		!validSHA256Digest(body.PreviewDigest) || body.ConfirmPolicyID == "" ||
		strings.TrimSpace(body.Reason) != body.Reason || body.Reason == "" || len(body.Rules) == 0 {
		return out, errors.New("operator client: compliance policy publish coordinates 不合法")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/compliance-policies", key, body)
	if err != nil {
		return out, err
	}
	replayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "compliance policy publish", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, errors.New("operator client: compliance policy replay evidence mismatch")
	}
	// A fresh decision is 201; a replay or an unchanged republish is 200. Any
	// other pairing means the two sides disagree about what just happened.
	if fresh := response.status == http.StatusCreated; fresh == (out.Replayed || out.Unchanged) {
		return out, fmt.Errorf("operator client: compliance policy publish HTTP %d 與結果不符", response.status)
	}
	if out.PolicyID != body.PolicyID || out.Revision <= 0 || !validSHA256Digest(out.Digest) {
		return out, errors.New("operator client: compliance policy publish identity 不一致")
	}
	if err := out.Policy.Validate(); err != nil {
		return out, fmt.Errorf("operator client: compliance policy publish 回傳了不合法的規則：%w", err)
	}
	return out, nil
}

func (c *Client) PreviewComplianceAssignment(ctx context.Context, body ComplianceAssignmentPreviewRequest) (operator.ComplianceAssignmentPreviewResult, error) {
	var out operator.ComplianceAssignmentPreviewResult
	if body.Revision <= 0 || body.ScopeID == "" || body.PolicyID == "" {
		return out, errors.New("operator client: compliance assignment preview coordinates 不合法")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/compliance-assignments/preview", "", body)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: compliance assignment preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "compliance assignment preview", &out); err != nil {
		return out, err
	}
	if string(out.Scope) != body.Scope || out.ScopeID != body.ScopeID || out.Revision != body.Revision ||
		!validSHA256Digest(out.Digest) || !validSHA256Digest(out.PreviewDigest) {
		return out, errors.New("operator client: compliance assignment preview identity 不一致")
	}
	if err := validateComplianceActionDescriptions(out.Policy, out.Actions); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) AssignCompliancePolicy(ctx context.Context, key string, body ComplianceAssignmentRequest) (store.OperatorComplianceAssignmentResult, error) {
	var out store.OperatorComplianceAssignmentResult
	if !validSettingIdempotencyKey(key) || !validSHA256Digest(body.PreviewDigest) ||
		body.ConfirmScopeID == "" || body.Revision <= 0 ||
		strings.TrimSpace(body.Reason) != body.Reason || body.Reason == "" {
		return out, errors.New("operator client: compliance assignment coordinates 不合法")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/compliance-assignments", key, body)
	if err != nil {
		return out, err
	}
	replayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "compliance assignment", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, errors.New("operator client: compliance assignment replay evidence mismatch")
	}
	if fresh := response.status == http.StatusCreated; fresh == (out.Replayed || out.Unchanged) {
		return out, fmt.Errorf("operator client: compliance assignment HTTP %d 與結果不符", response.status)
	}
	if string(out.Scope) != body.Scope || out.ScopeID != body.ScopeID ||
		out.PolicyID != body.PolicyID || out.PolicyRev != body.Revision ||
		out.AssignmentID == "" || !validSHA256Digest(out.Digest) {
		return out, errors.New("operator client: compliance assignment identity 不一致")
	}
	return out, nil
}

// validateComplianceBoard keeps the verdict counts, the rows and the evidence
// one fact. A board whose totals disagree with its rows would be read as a
// fleet fact, and a row whose verdict disagrees with its own rule results would
// be a verdict nobody can check.
// boardActionOutcomes drops back to the domain type so the client decides
// "is work being withheld" with the same function the Hub used.
func boardActionOutcomes(machine operator.ComplianceBoardMachine) []compliance.ActionOutcome {
	out := make([]compliance.ActionOutcome, 0, len(machine.Actions))
	for _, a := range machine.Actions {
		out = append(out, compliance.ActionOutcome{Kind: a.Kind, State: a.State})
	}
	return out
}

func validateComplianceBoard(board operator.ComplianceBoardResult) error {
	counted := map[compliance.Verdict]int{}
	blocked := 0
	for _, machine := range board.Machines {
		if machine.MachineID == "" || machine.VerdictLabel == "" {
			return errors.New("operator client: compliance board row 沒有身分")
		}
		if (machine.Source == compliance.SourceNone) != (machine.PolicyID == "") {
			return errors.New("operator client: compliance board row 的來源與原則不一致")
		}
		results := make([]compliance.RuleResult, 0, len(machine.Results))
		for _, r := range machine.Results {
			if r.Label == "" || r.Detail == "" {
				return errors.New("operator client: compliance board 的規則判決沒有說明")
			}
			results = append(results, compliance.RuleResult{Kind: r.Kind, Outcome: r.Outcome, Detail: r.Detail})
		}
		// ⚠ 用戶端自己重算一次判決。判決是規則結果的函數，如果 Hub 送來的
		// 判決跟它自己列出來的證據推不出來，那份證據就不能用來做決定。
		want := compliance.Judge(machine.Source != compliance.SourceNone,
			machine.ReportedAt != nil, results)
		if want != machine.Verdict {
			return fmt.Errorf("operator client: compliance board 的 %s 判決與它自己列的證據不符",
				machine.MachineID)
		}
		counted[machine.Verdict]++
		// 動作的狀態也是判決的函數：一台不是「不符合」的機器不可能有動作生效中。
		for _, action := range machine.Actions {
			if action.Label == "" || action.StateLabel == "" {
				return errors.New("operator client: compliance board 的動作沒有說明")
			}
			if action.State != compliance.ActionStateNotTriggered &&
				machine.Verdict != compliance.VerdictNoncompliant {
				return fmt.Errorf("operator client: %s 不是不符合，卻有動作被觸發",
					machine.MachineID)
			}
			if (action.State == compliance.ActionStateInGrace) != (action.DueAt != nil) {
				return fmt.Errorf("operator client: %s 的動作狀態與生效時間不一致",
					machine.MachineID)
			}
		}
		if compliance.Blocks(boardActionOutcomes(machine)) {
			blocked++
		}
	}
	if blocked != board.Blocked {
		return errors.New("operator client: compliance board 的停發計數與列不符")
	}
	for verdict, n := range board.Counts {
		if counted[verdict] != n {
			return fmt.Errorf("operator client: compliance board 的 %s 計數與列不符", verdict)
		}
	}
	for verdict := range counted {
		if _, ok := board.Counts[verdict]; !ok {
			return fmt.Errorf("operator client: compliance board 沒有回報 %s 的計數", verdict)
		}
	}
	if board.EvaluatedAt.IsZero() {
		return errors.New("operator client: compliance board 沒有說判決是什麼時候算的")
	}
	return nil
}
