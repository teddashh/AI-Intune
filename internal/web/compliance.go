package web

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

const complianceBack = "/machines/compliance"

// compliancePageView is the Devices > Compliance projection. Every verdict on
// it is computed from what machines reported; no machine gets to assert that
// it complies.
type compliancePageView struct {
	Policies     []store.CompliancePolicySummary
	Machines     []complianceMachineRow
	Compliant    int
	Noncompliant int
	Unmeasured   int
	Unevaluated  int
	// Blocked is how many machines a compliance action is withholding work
	// from right now. HasActions says whether any published policy carries a
	// consequence at all; without one there is no number worth showing.
	Blocked       int
	HasActions    bool
	NextPolicy    string
	RuleChoices   []complianceRuleChoice
	ActionChoices []complianceActionChoice
	EvaluatedAt   time.Time
}

// complianceActionChoice is one row of the publish form's consequences. The
// form offers every action kind once, so a policy cannot carry the same
// consequence with two different grace periods.
type complianceActionChoice struct {
	Kind    compliance.ActionKind
	Label   string
	Effect  string
	Max     int
	Default int
	Help    string
}

// complianceRuleChoice is one row of the publish form. The form offers every
// rule kind once, so a policy cannot be written with the same rule twice.
type complianceRuleChoice struct {
	Kind        compliance.RuleKind
	Label       string
	Field       string
	Unit        string
	Min         int
	Max         int
	Default     int
	TextDefault string
	Help        string
}

type complianceMachineRow struct {
	MachineID   string
	DisplayName string
	Channel     string
	SourceLabel string
	PolicyID    string
	Revision    int64
	Verdict     compliance.Verdict
	Label       string
	Results     []operator.ComplianceRuleResult
	Actions     []operator.ComplianceActionOutcome
	ReportedAt  *time.Time
	Tone        string
}

type compliancePolicyReviewPage struct {
	Preview        operator.CompliancePolicyPreviewResult
	Reason         string
	IdempotencyKey string
}

type complianceAssignmentReviewPage struct {
	Preview        operator.ComplianceAssignmentPreviewResult
	Reason         string
	IdempotencyKey string
	ScopeLabel     string
}

// complianceRuleChoices is the publish form's vocabulary. It is derived from
// the domain's rule list, so a rule kind added to the core shows up here
// instead of being quietly unreachable from the console.
func complianceRuleChoices() []complianceRuleChoice {
	out := make([]complianceRuleChoice, 0, len(compliance.RuleKinds()))
	for _, kind := range compliance.RuleKinds() {
		choice := complianceRuleChoice{Kind: kind, Label: compliance.RuleLabel(kind)}
		switch kind {
		case compliance.RuleCheckinMaxAge:
			choice.Field, choice.Unit = "max_age_seconds", "秒"
			choice.Min, choice.Max = compliance.MinCheckinMaxAgeSeconds, compliance.MaxCheckinMaxAgeSeconds
			choice.Default = 900
			choice.Help = "距離上次報到超過這個秒數就是不符合。"
		case compliance.RuleAgentVersion:
			choice.Field = "agent_version"
			choice.Help = "回報的版本不是這一個就是不符合。"
		case compliance.RuleDiskFreeMinPercent:
			choice.Field, choice.Unit = "min_free_percent", "%"
			choice.Min, choice.Max = compliance.MinFreePercent, compliance.MaxFreePercent
			choice.Default = 10
			choice.Help = "剩餘空間低於這個百分比就是不符合。"
		case compliance.RuleSettingsApplied:
			choice.Help = "裝置回報的設定必須就是 Hub 指派的那一份。"
		case compliance.RuleJobsEnabled:
			choice.Help = "裝置必須回報它收工作單。"
		}
		out = append(out, choice)
	}
	return out
}

// complianceActionChoices is derived from the domain's action list for the
// same reason the rule rows are: a consequence added to the core must not be
// reachable from the API and invisible in the console.
func complianceActionChoices() []complianceActionChoice {
	out := make([]complianceActionChoice, 0, len(compliance.ActionKinds()))
	for _, kind := range compliance.ActionKinds() {
		choice := complianceActionChoice{Kind: kind, Label: compliance.ActionLabel(kind),
			Effect: compliance.ActionEffect(kind), Max: compliance.MaxGraceSeconds}
		switch kind {
		case compliance.ActionBlockJobs:
			choice.Default = 3600
			choice.Help = "連續不符合超過這個秒數，Hub 就不再把新工作單交給它；判定一恢復就自動解除。0 表示立即。"
		}
		out = append(out, choice)
	}
	return out
}

func (s *Server) compliance(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	board, err := s.operator.ComplianceBoard()
	if err != nil {
		s.fail(w, "讀取合規性失敗", err)
		return
	}
	view := &compliancePageView{
		Policies:     board.Policies,
		Machines:     make([]complianceMachineRow, 0, len(board.Machines)),
		Compliant:    board.Counts[compliance.VerdictCompliant],
		Noncompliant: board.Counts[compliance.VerdictNoncompliant],
		Unmeasured: board.Counts[compliance.VerdictUnmeasured] +
			board.Counts[compliance.VerdictNeverReported],
		Unevaluated:   board.Counts[compliance.VerdictNotEvaluated],
		Blocked:       board.Blocked,
		HasActions:    compliancePoliciesCarryActions(board.Policies),
		NextPolicy:    nextCompliancePolicyID(board.Policies),
		RuleChoices:   complianceRuleChoices(),
		ActionChoices: complianceActionChoices(),
		EvaluatedAt:   board.EvaluatedAt,
	}
	for _, machine := range board.Machines {
		view.Machines = append(view.Machines, complianceMachineRow{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName,
			Channel: machine.Channel, SourceLabel: complianceSourceLabel(machine.Source),
			PolicyID: machine.PolicyID, Revision: machine.Revision,
			Verdict: machine.Verdict, Label: machine.VerdictLabel,
			Results: machine.Results, Actions: machine.Actions,
			ReportedAt: machine.ReportedAt,
			Tone:       complianceVerdictTone(machine.Verdict),
		})
	}
	s.render(w, r, "compliance.html", page{
		Title: "裝置合規性", Nav: "machines-compliance",
		Now: now.Local().Format("2006-01-02 15:04"), Compliance: view,
	})
}

func complianceSourceLabel(source compliance.Source) string {
	switch source {
	case compliance.SourceMachine:
		return "裝置指派"
	case compliance.SourceChannel:
		return "通道指派"
	default:
		return "未指派"
	}
}

// complianceVerdictTone maps the verdict onto the console's status colours.
// Everything the Hub cannot measure shares one tone, so a page that looks calm
// always means measured.
func complianceVerdictTone(verdict compliance.Verdict) string {
	switch verdict {
	case compliance.VerdictCompliant:
		return "green"
	case compliance.VerdictNoncompliant:
		return "red"
	default:
		return "grey"
	}
}

func complianceOutcomeTone(outcome compliance.Outcome) string {
	switch outcome {
	case compliance.OutcomePass:
		return "green"
	case compliance.OutcomeFail:
		return "red"
	default:
		return "grey"
	}
}

// nextCompliancePolicyID prefills the publish form with the policy an operator
// is most likely editing: the one already judging machines.
func nextCompliancePolicyID(policies []store.CompliancePolicySummary) string {
	best := ""
	var reach int64 = -1
	for _, policy := range policies {
		if policy.Assignments > reach {
			best, reach = policy.PolicyID, policy.Assignments
		}
	}
	return best
}

// compliancePolicyFromForm builds the rule list from the checked rows. Each
// rule kind appears at most once in the form, so a duplicate is not
// expressible here.
func compliancePolicyFromForm(r *http.Request) (compliance.Policy, bool) {
	p := compliance.Policy{SchemaVersion: compliance.SchemaVersion}
	for _, kind := range compliance.RuleKinds() {
		if r.FormValue("rule_"+string(kind)) == "" {
			continue
		}
		rule := compliance.Rule{Kind: kind}
		switch kind {
		case compliance.RuleCheckinMaxAge:
			value, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_age_seconds")))
			if err != nil {
				return compliance.Policy{}, false
			}
			rule.MaxAgeSeconds = value
		case compliance.RuleDiskFreeMinPercent:
			value, err := strconv.Atoi(strings.TrimSpace(r.FormValue("min_free_percent")))
			if err != nil {
				return compliance.Policy{}, false
			}
			rule.MinFreePercent = value
		case compliance.RuleAgentVersion:
			rule.AgentVersion = strings.TrimSpace(r.FormValue("agent_version"))
		}
		p.Rules = append(p.Rules, rule)
	}
	for _, kind := range compliance.ActionKinds() {
		if r.FormValue("action_"+string(kind)) == "" {
			continue
		}
		grace, err := strconv.Atoi(strings.TrimSpace(r.FormValue("grace_" + string(kind))))
		if err != nil {
			return compliance.Policy{}, false
		}
		p.Actions = append(p.Actions, compliance.Action{Kind: kind, GraceSeconds: grace})
	}
	return p, true
}

func compliancePoliciesCarryActions(policies []store.CompliancePolicySummary) bool {
	for _, p := range policies {
		if len(p.Policy.Actions) > 0 {
			return true
		}
	}
	return false
}

// complianceGraceWords is the grace period in the words next to the action.
// Seconds are what the form takes; they are not what an operator reads.
func complianceGraceWords(seconds int) string {
	if seconds <= 0 {
		return "立即生效"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("連續 %d 天後生效", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("連續 %d 小時後生效", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("連續 %d 分鐘後生效", int(d/time.Minute))
	default:
		return fmt.Sprintf("連續 %d 秒後生效", seconds)
	}
}

// complianceActionTone keeps an action that is actually withholding work from
// looking like an ordinary row. 寬限中 is not calm either: it is a countdown.
func complianceActionTone(state compliance.ActionState) string {
	switch state {
	case compliance.ActionStateEnforced:
		return "red"
	case compliance.ActionStateInGrace:
		return "amber"
	default:
		return "grey"
	}
}

func (s *Server) previewCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	policyID := strings.TrimSpace(r.FormValue("policy_id"))
	policy, ok := compliancePolicyFromForm(r)
	if !ok {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, policyID, "未建立合規性原則預覽",
			"勾選的規則門檻必須是整數。")
		return
	}
	reason := r.FormValue("reason")
	if !validWebBoundedText(reason, 500) {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, policyID, "未建立合規性原則預覽",
			"理由不可省略、前後不可有空白，且最多 500 字。")
		return
	}
	preview, err := s.operator.PreviewCompliancePolicy(operator.CompliancePolicyPreviewRequest{
		PolicyID: policyID, Policy: policy,
	})
	if err != nil {
		s.renderComplianceError(w, r, err, policyID, "未建立合規性原則預覽", "compliance policy preview")
		return
	}
	key, err := operator.NewIdempotencyKey("web-compliance-policy")
	if err != nil {
		s.renderComplianceStatus(w, r, http.StatusInternalServerError, policyID,
			"未建立合規性原則預覽", "無法建立 request key。")
		return
	}
	s.render(w, r, "compliance_policy_review.html", page{
		Title: "確認合規性原則", Nav: "machines-compliance",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		CompliancePolicyReview: &compliancePolicyReviewPage{
			Preview: preview, Reason: reason, IdempotencyKey: key,
		},
	})
}

func (s *Server) publishCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	policyID := strings.TrimSpace(r.FormValue("policy_id"))
	policy, ok := compliancePolicyFromForm(r)
	expected, revisionErr := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	if !ok || revisionErr != nil || expected < 0 {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, policyID, "未發佈合規性原則",
			"規則門檻與 revision 必須是整數。")
		return
	}
	result, err := s.operator.PublishCompliancePolicy(operator.CompliancePolicyPublishRequest{
		PolicyID: policyID, Policy: policy, ExpectedRevision: &expected,
		PreviewDigest:   r.FormValue("preview_digest"),
		ConfirmPolicyID: r.FormValue("confirm_policy_id"), Reason: r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderComplianceError(w, r, err, policyID, "未發佈合規性原則", "compliance policy publish")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, complianceBack, http.StatusSeeOther)
}

func (s *Server) previewComplianceAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scopeID := strings.TrimSpace(r.FormValue("scope_id"))
	revision, revisionErr := strconv.ParseInt(r.FormValue("policy_revision"), 10, 64)
	if revisionErr != nil || revision <= 0 {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, scopeID, "未建立指派預覽",
			"policy revision 必須是正整數。")
		return
	}
	reason := r.FormValue("reason")
	if !validWebBoundedText(reason, 500) {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, scopeID, "未建立指派預覽",
			"理由不可省略、前後不可有空白，且最多 500 字。")
		return
	}
	preview, err := s.operator.PreviewComplianceAssignment(operator.ComplianceAssignmentPreviewRequest{
		Scope: compliance.Scope(r.FormValue("scope")), ScopeID: scopeID,
		PolicyID: strings.TrimSpace(r.FormValue("policy_id")), Revision: revision,
	})
	if err != nil {
		s.renderComplianceError(w, r, err, scopeID, "未建立指派預覽", "compliance assignment preview")
		return
	}
	key, err := operator.NewIdempotencyKey("web-compliance-assign")
	if err != nil {
		s.renderComplianceStatus(w, r, http.StatusInternalServerError, scopeID,
			"未建立指派預覽", "無法建立 request key。")
		return
	}
	s.render(w, r, "compliance_assignment_review.html", page{
		Title: "確認合規性指派", Nav: "machines-compliance",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		ComplianceAssignmentReview: &complianceAssignmentReviewPage{
			Preview: preview, Reason: reason, IdempotencyKey: key,
			ScopeLabel: preview.ScopeLabel,
		},
	})
}

func (s *Server) applyComplianceAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scopeID := strings.TrimSpace(r.FormValue("scope_id"))
	revision, revisionErr := strconv.ParseInt(r.FormValue("policy_revision"), 10, 64)
	if revisionErr != nil || revision <= 0 {
		s.renderComplianceStatus(w, r, http.StatusBadRequest, scopeID, "未指派合規性原則",
			"policy revision 必須是正整數。")
		return
	}
	result, err := s.operator.AssignCompliancePolicy(operator.ComplianceAssignmentRequest{
		Scope: compliance.Scope(r.FormValue("scope")), ScopeID: scopeID,
		PolicyID: strings.TrimSpace(r.FormValue("policy_id")), Revision: revision,
		PreviewDigest: r.FormValue("preview_digest"), ConfirmScopeID: r.FormValue("confirm_scope_id"),
		Reason: r.FormValue("reason"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderComplianceError(w, r, err, scopeID, "未指派合規性原則", "compliance assignment")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, complianceBack, http.StatusSeeOther)
}

func (s *Server) renderComplianceError(w http.ResponseWriter, r *http.Request, err error,
	subject, headline, what string,
) {
	status, _, detail := operator.HTTPError(err)
	if status == http.StatusInternalServerError {
		log.Printf("operator %s failed: %v", what, err)
	}
	s.renderComplianceStatus(w, r, status, subject, headline, detail)
}

func (s *Server) renderComplianceStatus(w http.ResponseWriter, r *http.Request, status int,
	subject, headline, detail string,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.render(w, r, "action.html", page{
		Title: headline, Nav: "machines-compliance",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{
			Subject: subject, Headline: headline, Detail: detail,
			Back: complianceBack, BackLabel: "回裝置合規性",
		},
	})
}
