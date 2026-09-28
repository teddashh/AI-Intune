package web

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

const configurationBack = "/machines/configuration"

// configurationPageView is the Devices > Configuration projection. The applied
// column is the digest a machine echoed back, never an inference from how
// often it checked in.
type configurationPageView struct {
	Defaults    settingpolicy.Settings
	Policies    []store.SettingPolicySummary
	Machines    []configurationMachineRow
	Applied     int
	Pending     int
	Unreported  int
	Mismatch    int
	NextPolicy  string
	DefaultRows bool
}

type configurationMachineRow struct {
	MachineID   string
	DisplayName string
	Channel     string
	SourceLabel string
	PolicyID    string
	Revision    int64
	Checkin     int
	Observation int
	Verdict     settingpolicy.Verdict
	Label       string
	ReportedAt  *time.Time
	Tone        string
}

type settingPolicyReviewPage struct {
	Preview        operator.SettingPolicyPreviewResult
	Reason         string
	IdempotencyKey string
}

type settingAssignmentReviewPage struct {
	Preview        operator.SettingAssignmentPreviewResult
	Reason         string
	IdempotencyKey string
	ScopeLabel     string
}

func (s *Server) configuration(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	board, err := s.operator.SettingBoard()
	if err != nil {
		s.fail(w, "讀取裝置組態失敗", err)
		return
	}
	view := &configurationPageView{
		Defaults: board.Defaults, Policies: board.Policies,
		Machines:   make([]configurationMachineRow, 0, len(board.Machines)),
		Applied:    board.Counts[settingpolicy.VerdictApplied],
		Pending:    board.Counts[settingpolicy.VerdictPending],
		Unreported: board.Counts[settingpolicy.VerdictUnknown] + board.Counts[settingpolicy.VerdictNeverReported],
		Mismatch:   board.Counts[settingpolicy.VerdictMismatch],
		NextPolicy: nextConfigurationPolicyID(board.Policies),
	}
	for _, machine := range board.Machines {
		view.Machines = append(view.Machines, configurationMachineRow{
			MachineID: machine.MachineID, DisplayName: machine.DisplayName, Channel: machine.Channel,
			SourceLabel: configurationSourceLabel(machine.Source), PolicyID: machine.PolicyID,
			Revision: machine.Revision, Checkin: machine.Settings.CheckinIntervalSeconds,
			Observation: machine.Settings.ObservationIntervalSeconds, Verdict: machine.Verdict,
			Label: machine.VerdictLabel, ReportedAt: machine.ReportedAt,
			Tone: configurationVerdictTone(machine.Verdict),
		})
		if machine.Source == settingpolicy.SourceDefault {
			view.DefaultRows = true
		}
	}
	s.render(w, r, "configuration.html", page{
		Title: "裝置組態", Nav: "machines-configuration",
		Now: now.Local().Format("2006-01-02 15:04"), Configuration: view,
	})
}

func configurationSourceLabel(source settingpolicy.Source) string {
	switch source {
	case settingpolicy.SourceMachine:
		return "裝置指派"
	case settingpolicy.SourceChannel:
		return "通道指派"
	default:
		return "預設值"
	}
}

// configurationVerdictTone maps the verdict onto the console's status colours.
// Everything the Hub cannot measure shares one tone, so a page that looks calm
// always means measured.
func configurationVerdictTone(verdict settingpolicy.Verdict) string {
	switch verdict {
	case settingpolicy.VerdictApplied:
		return "green"
	case settingpolicy.VerdictPending:
		return "blue"
	case settingpolicy.VerdictMismatch:
		return "red"
	default:
		return "grey"
	}
}

// nextConfigurationPolicyID prefills the publish form with the policy an
// operator is most likely editing: the one already carrying machines.
func nextConfigurationPolicyID(policies []store.SettingPolicySummary) string {
	best := ""
	var reach int64 = -1
	for _, policy := range policies {
		if policy.Assignments > reach {
			best, reach = policy.PolicyID, policy.Assignments
		}
	}
	return best
}

func (s *Server) previewSettingPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	policyID := strings.TrimSpace(r.FormValue("policy_id"))
	checkin, checkinErr := strconv.Atoi(r.FormValue("checkin_interval_seconds"))
	observation, observationErr := strconv.Atoi(r.FormValue("observation_interval_seconds"))
	if checkinErr != nil || observationErr != nil {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, policyID, "未建立設定原則預覽",
			"報到與量測間隔都必須是整數秒。")
		return
	}
	reason := r.FormValue("reason")
	if !validWebBoundedText(reason, 500) {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, policyID, "未建立設定原則預覽",
			"理由不可省略、前後不可有空白，且最多 500 字。")
		return
	}
	preview, err := s.operator.PreviewSettingPolicy(operator.SettingPolicyPreviewRequest{
		PolicyID: policyID, Settings: settingpolicy.Settings{
			SchemaVersion: settingpolicy.SchemaVersion, CheckinIntervalSeconds: checkin,
			ObservationIntervalSeconds: observation,
		},
	})
	if err != nil {
		s.renderConfigurationError(w, r, err, policyID, "未建立設定原則預覽", "setting policy preview")
		return
	}
	key, err := operator.NewIdempotencyKey("web-setting-policy")
	if err != nil {
		s.renderConfigurationStatus(w, r, http.StatusInternalServerError, policyID,
			"未建立設定原則預覽", "無法建立 request key。")
		return
	}
	s.render(w, r, "configuration_policy_review.html", page{
		Title: "確認設定原則", Nav: "machines-configuration",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		SettingPolicyReview: &settingPolicyReviewPage{
			Preview: preview, Reason: reason, IdempotencyKey: key,
		},
	})
}

func (s *Server) publishSettingPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	policyID := strings.TrimSpace(r.FormValue("policy_id"))
	checkin, checkinErr := strconv.Atoi(r.FormValue("checkin_interval_seconds"))
	observation, observationErr := strconv.Atoi(r.FormValue("observation_interval_seconds"))
	expected, revisionErr := strconv.ParseInt(r.FormValue("expected_revision"), 10, 64)
	if checkinErr != nil || observationErr != nil || revisionErr != nil || expected < 0 {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, policyID, "未發佈設定原則",
			"間隔與 revision 必須是整數。")
		return
	}
	result, err := s.operator.PublishSettingPolicy(operator.SettingPolicyPublishRequest{
		PolicyID: policyID, Settings: settingpolicy.Settings{
			SchemaVersion: settingpolicy.SchemaVersion, CheckinIntervalSeconds: checkin,
			ObservationIntervalSeconds: observation,
		},
		ExpectedRevision: &expected, PreviewDigest: r.FormValue("preview_digest"),
		ConfirmPolicyID: r.FormValue("confirm_policy_id"), Reason: r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderConfigurationError(w, r, err, policyID, "未發佈設定原則", "setting policy publish")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, configurationBack, http.StatusSeeOther)
}

func (s *Server) previewSettingAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scopeID := strings.TrimSpace(r.FormValue("scope_id"))
	revision, revisionErr := strconv.ParseInt(r.FormValue("policy_revision"), 10, 64)
	if revisionErr != nil || revision <= 0 {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, scopeID, "未建立指派預覽",
			"policy revision 必須是正整數。")
		return
	}
	reason := r.FormValue("reason")
	if !validWebBoundedText(reason, 500) {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, scopeID, "未建立指派預覽",
			"理由不可省略、前後不可有空白，且最多 500 字。")
		return
	}
	preview, err := s.operator.PreviewSettingAssignment(operator.SettingAssignmentPreviewRequest{
		Scope: settingpolicy.Scope(r.FormValue("scope")), ScopeID: scopeID,
		PolicyID: strings.TrimSpace(r.FormValue("policy_id")), Revision: revision,
	})
	if err != nil {
		s.renderConfigurationError(w, r, err, scopeID, "未建立指派預覽", "setting assignment preview")
		return
	}
	key, err := operator.NewIdempotencyKey("web-setting-assign")
	if err != nil {
		s.renderConfigurationStatus(w, r, http.StatusInternalServerError, scopeID,
			"未建立指派預覽", "無法建立 request key。")
		return
	}
	s.render(w, r, "configuration_assignment_review.html", page{
		Title: "確認設定指派", Nav: "machines-configuration",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		SettingAssignmentReview: &settingAssignmentReviewPage{
			Preview: preview, Reason: reason, IdempotencyKey: key,
			ScopeLabel: preview.ScopeLabel,
		},
	})
}

func (s *Server) applySettingAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scopeID := strings.TrimSpace(r.FormValue("scope_id"))
	revision, revisionErr := strconv.ParseInt(r.FormValue("policy_revision"), 10, 64)
	if revisionErr != nil || revision <= 0 {
		s.renderConfigurationStatus(w, r, http.StatusBadRequest, scopeID, "未指派設定原則",
			"policy revision 必須是正整數。")
		return
	}
	result, err := s.operator.AssignSettingPolicy(operator.SettingAssignmentRequest{
		Scope: settingpolicy.Scope(r.FormValue("scope")), ScopeID: scopeID,
		PolicyID: strings.TrimSpace(r.FormValue("policy_id")), Revision: revision,
		PreviewDigest: r.FormValue("preview_digest"), ConfirmScopeID: r.FormValue("confirm_scope_id"),
		Reason: r.FormValue("reason"), IdempotencyKey: r.FormValue("idempotency_key"),
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderConfigurationError(w, r, err, scopeID, "未指派設定原則", "setting assignment")
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	http.Redirect(w, r, configurationBack, http.StatusSeeOther)
}

func (s *Server) renderConfigurationError(w http.ResponseWriter, r *http.Request, err error,
	subject, headline, what string,
) {
	status, _, detail := operator.HTTPError(err)
	if status == http.StatusInternalServerError {
		log.Printf("operator %s failed: %v", what, err)
	}
	s.renderConfigurationStatus(w, r, status, subject, headline, detail)
}

func (s *Server) renderConfigurationStatus(w http.ResponseWriter, r *http.Request, status int,
	subject, headline, detail string,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.render(w, r, "action.html", page{
		Title: headline, Nav: "machines-configuration",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{
			Subject: subject, Headline: headline, Detail: detail,
			Back: configurationBack, BackLabel: "回裝置組態",
		},
	})
}
