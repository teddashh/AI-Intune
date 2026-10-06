package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type deploymentCreateReview struct {
	Preview           operator.DeploymentCreatePreviewResult
	Reason            string
	IdempotencyKey    string
	CapabilityAllowed bool
}

type deploymentActionReview struct {
	Preview           operator.DeploymentActionPreviewResult
	ActionView        deploymentActionEligibility
	Reason            string
	IdempotencyKey    string
	ApplyPath         string
	Title             string
	Button            string
	CapabilityAllowed bool
}

const deploymentWebFormMaxBytes int64 = 64 << 10

func (s *Server) previewDeploymentCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, reason, err := parseDeploymentCreateForm(w, r, false)
	if err != nil {
		s.renderActionStatus(w, r, http.StatusBadRequest, "deployment create",
			"沒有建立 deployment preview", err.Error(), "/deployments?view=new")
		return
	}
	preview, err := s.operator.PreviewDeploymentCreateContext(r.Context(), request, time.Now().UTC())
	if err != nil {
		s.renderDeploymentWebError(w, r, "create preview", "deployment create",
			"沒有建立 deployment preview", "/deployments?view=new", err)
		return
	}
	key, err := operator.NewIdempotencyKey("web-deployment-create")
	if err != nil {
		log.Printf("deployment create preview 產生 idempotency key 失敗: %v", err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, "deployment create",
			"沒有建立 deployment preview", "無法產生這次確認所需的 request key。", "/deployments?view=new")
		return
	}
	s.render(w, r, "deployment_create_review.html", page{
		Title: "確認建立 deployment", Nav: "deployments",
		Now: time.Now().Local().Format("2006-01-02 15:04"),
		DeploymentCreateReview: &deploymentCreateReview{
			Preview: preview, Reason: reason, IdempotencyKey: key,
			CapabilityAllowed: accessFromRequest(r).CanAdmin,
		},
	})
}

func (s *Server) applyDeploymentCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	planning, reason, err := parseDeploymentCreateForm(w, r, true)
	if err != nil {
		s.rejectDeploymentWebForm(w, r, operator.DeploymentTransportRejectionCreate, "deployment create", err)
		return
	}
	values := r.PostForm
	result, err := s.operator.ApplyDeploymentCreateContext(r.Context(), operator.DeploymentCreateApplyRequest{
		DeploymentCreatePreviewRequest: planning,
		PreviewDigest:                  values.Get("preview_digest"), ConfirmChannel: values.Get("confirm_channel"),
		ConfirmVersion: values.Get("confirm_version"), Reason: reason,
		IdempotencyKey: values.Get("idempotency_key"),
		Actor:          operator.ActorFromRequest(r, operator.SourceKindWeb),
	})
	if err != nil {
		s.renderDeploymentWebError(w, r, "create", "deployment create", "沒有建立 deployment",
			"/deployments?view=new", err)
		return
	}
	s.renderDeploymentMutation(w, r, result, "已建立 deployment", "已確認原本的 deployment create 結果（replay）")
}

func (s *Server) previewDeploymentContinue(w http.ResponseWriter, r *http.Request) {
	s.previewDeploymentControl(w, r, "continue")
}

func (s *Server) previewDeploymentRetry(w http.ResponseWriter, r *http.Request) {
	s.previewDeploymentControl(w, r, "retry")
}

func (s *Server) previewDeploymentAbandon(w http.ResponseWriter, r *http.Request) {
	s.previewDeploymentControl(w, r, "abandon")
}

func (s *Server) previewDeploymentSkipFailedBatch(w http.ResponseWriter, r *http.Request) {
	s.previewDeploymentControl(w, r, "skip_failed_batch")
}

func (s *Server) previewDeploymentControl(w http.ResponseWriter, r *http.Request, action string) {
	w.Header().Set("Cache-Control", "no-store")
	values, err := parseDeploymentWebForm(w, r, map[string]bool{"reason": true}, nil)
	if err != nil {
		s.renderActionStatus(w, r, http.StatusBadRequest, r.PathValue("id"),
			"沒有建立 deployment action preview", err.Error(), "/deployments/"+r.PathValue("id"))
		return
	}
	reason := strings.TrimSpace(values.Get("reason"))
	if action == "skip_failed_batch" && reason == "" {
		s.renderActionStatus(w, r, http.StatusBadRequest, r.PathValue("id"),
			"沒有建立 skip failed batch preview", "skip failed batch 必須留下理由。", "/deployments/"+r.PathValue("id"))
		return
	}
	if len(reason) > 500 {
		s.renderActionStatus(w, r, http.StatusBadRequest, r.PathValue("id"),
			"沒有建立 deployment action preview", "變更理由最多 500 bytes。", "/deployments/"+r.PathValue("id"))
		return
	}
	id, now := r.PathValue("id"), time.Now().UTC()
	var preview operator.DeploymentActionPreviewResult
	switch action {
	case "continue":
		preview, err = s.operator.PreviewDeploymentContinueContext(r.Context(), operator.DeploymentContinuePreviewRequest{DeploymentID: id}, now)
	case "retry":
		preview, err = s.operator.PreviewDeploymentRetryContext(r.Context(), operator.DeploymentRetryPreviewRequest{DeploymentID: id}, now)
	case "abandon":
		preview, err = s.operator.PreviewDeploymentAbandonContext(r.Context(), operator.DeploymentAbandonPreviewRequest{DeploymentID: id}, now)
	case "skip_failed_batch":
		preview, err = s.operator.PreviewDeploymentSkipFailedBatchContext(r.Context(), operator.DeploymentContinuePreviewRequest{DeploymentID: id}, now)
	default:
		err = errors.New("invalid deployment action")
	}
	if err != nil {
		s.renderDeploymentWebError(w, r, action+" preview", id,
			"沒有建立 deployment action preview", "/deployments/"+id, err)
		return
	}
	key, err := operator.NewIdempotencyKey("web-deployment-" + action)
	if err != nil {
		log.Printf("deployment %s preview 產生 idempotency key 失敗: %v", action, err)
		s.renderActionStatus(w, r, http.StatusInternalServerError, id,
			"沒有建立 deployment action preview", "無法產生這次確認所需的 request key。", "/deployments/"+id)
		return
	}
	access := accessFromRequest(r)
	review := &deploymentActionReview{
		Preview: preview, ActionView: deploymentActionView(action, preview.Eligibility), Reason: reason, IdempotencyKey: key,
		ApplyPath: "/deployments/" + id + "/" + action,
		Title:     strings.ToUpper(action[:1]) + action[1:] + " deployment",
		Button:    "確認並 " + action, CapabilityAllowed: access.CanOperate,
	}
	if action == "abandon" {
		review.CapabilityAllowed = access.CanAdmin
	}
	if action == "skip_failed_batch" {
		review.ApplyPath = "/deployments/" + id + "/skip-failed-batch"
		review.Title = "skip failed batch"
		review.Button = "skip failed batch"
	}
	s.render(w, r, "deployment_action_review.html", page{
		Title: "確認 " + action + " deployment", Nav: "deployments-detail",
		Now: time.Now().Local().Format("2006-01-02 15:04"), DeploymentActionReview: review,
	})
}

func (s *Server) applyDeploymentContinue(w http.ResponseWriter, r *http.Request) {
	s.applyDeploymentControl(w, r, "continue")
}

func (s *Server) applyDeploymentRetry(w http.ResponseWriter, r *http.Request) {
	s.applyDeploymentControl(w, r, "retry")
}

func (s *Server) applyDeploymentAbandon(w http.ResponseWriter, r *http.Request) {
	s.applyDeploymentControl(w, r, "abandon")
}

func (s *Server) applyDeploymentSkipFailedBatch(w http.ResponseWriter, r *http.Request) {
	s.applyDeploymentControl(w, r, "skip_failed_batch")
}

func (s *Server) applyDeploymentControl(w http.ResponseWriter, r *http.Request, action string) {
	w.Header().Set("Cache-Control", "no-store")
	allowed := map[string]bool{
		"preview_digest": true, "expected_control_revision": true, "expected_opened_batch": true,
		"reason": true, "idempotency_key": true,
	}
	switch action {
	case "continue", "skip_failed_batch":
		allowed["confirm_channel"] = true
	case "retry":
		allowed["confirm_channel"], allowed["confirm_version"] = true, true
	case "abandon":
		allowed["confirm_deployment_id"] = true
	}
	required := []string{"preview_digest", "expected_control_revision", "expected_opened_batch", "idempotency_key"}
	if action == "continue" || action == "skip_failed_batch" {
		required = append(required, "confirm_channel")
	} else if action == "retry" {
		required = append(required, "confirm_channel", "confirm_version")
	} else {
		required = append(required, "confirm_deployment_id")
	}
	values, err := parseDeploymentWebForm(w, r, allowed, required)
	if err != nil {
		s.rejectDeploymentWebForm(w, r, deploymentTransportRejectionAction(action), r.PathValue("id"), err)
		return
	}
	expectedRevision, err := parseCanonicalInt64(values.Get("expected_control_revision"), 0, 1<<62)
	if err != nil {
		s.rejectDeploymentWebForm(w, r, deploymentTransportRejectionAction(action), r.PathValue("id"),
			errors.New("expected control revision 不合法"))
		return
	}
	expectedOpened, err := parseCanonicalInt(values.Get("expected_opened_batch"), 0, 1<<30)
	if err != nil {
		s.rejectDeploymentWebForm(w, r, deploymentTransportRejectionAction(action), r.PathValue("id"),
			errors.New("expected opened batch 不合法"))
		return
	}
	reason := strings.TrimSpace(values.Get("reason"))
	if action == "skip_failed_batch" && reason == "" {
		s.rejectDeploymentWebForm(w, r, operator.DeploymentTransportRejectionSkipFailedBatch, r.PathValue("id"),
			errors.New("skip failed batch 必須留下理由"))
		return
	}
	if len(reason) > 500 {
		s.rejectDeploymentWebForm(w, r, deploymentTransportRejectionAction(action), r.PathValue("id"),
			errors.New("變更理由最多 500 bytes"))
		return
	}
	id := r.PathValue("id")
	actor := operator.ActorFromRequest(r, operator.SourceKindWeb)
	var result operator.DeploymentMutationResult
	switch action {
	case "continue":
		result, err = s.operator.ApplyDeploymentContinueContext(r.Context(), operator.DeploymentContinueApplyRequest{
			DeploymentID: id, PreviewDigest: values.Get("preview_digest"),
			ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
			ConfirmChannel: values.Get("confirm_channel"), Reason: reason,
			IdempotencyKey: values.Get("idempotency_key"), Actor: actor,
		})
	case "retry":
		result, err = s.operator.ApplyDeploymentRetryContext(r.Context(), operator.DeploymentRetryApplyRequest{
			DeploymentID: id, PreviewDigest: values.Get("preview_digest"),
			ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
			ConfirmChannel: values.Get("confirm_channel"), ConfirmVersion: values.Get("confirm_version"),
			Reason: reason, IdempotencyKey: values.Get("idempotency_key"), Actor: actor,
		})
	case "abandon":
		result, err = s.operator.ApplyDeploymentAbandonContext(r.Context(), operator.DeploymentAbandonApplyRequest{
			DeploymentID: id, PreviewDigest: values.Get("preview_digest"),
			ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
			ConfirmDeploymentID: values.Get("confirm_deployment_id"), Reason: reason,
			IdempotencyKey: values.Get("idempotency_key"), Actor: actor,
		})
	case "skip_failed_batch":
		result, err = s.operator.ApplyDeploymentSkipFailedBatchContext(r.Context(), operator.DeploymentContinueApplyRequest{
			DeploymentID: id, PreviewDigest: values.Get("preview_digest"),
			ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
			ConfirmChannel: values.Get("confirm_channel"), Reason: reason,
			IdempotencyKey: values.Get("idempotency_key"), Actor: actor,
		})
	default:
		err = errors.New("invalid deployment action")
	}
	if err != nil {
		headline := "沒有 " + action + " deployment"
		if action == "skip_failed_batch" {
			headline = "沒有 skip failed batch"
		}
		s.renderDeploymentWebError(w, r, action, id, headline, "/deployments/"+id, err)
		return
	}
	fresh, replay := "已 "+action+" deployment", "已確認原本的 deployment "+action+" 結果（replay）"
	if action == "skip_failed_batch" {
		fresh, replay = "已 skip failed batch", "已確認原本的 skip failed batch 結果（replay）"
	}
	s.renderDeploymentMutation(w, r, result, fresh, replay)
}

func parseDeploymentCreateForm(w http.ResponseWriter, r *http.Request, apply bool) (operator.DeploymentCreatePreviewRequest, string, error) {
	allowed := map[string]bool{
		"channel": true, "version": true, "artifact_sha256": true, "batch_size": true,
		"execution_timeout_seconds": true, "irreversible": true, "reason": true,
	}
	required := []string{"channel", "version", "artifact_sha256", "batch_size", "execution_timeout_seconds", "irreversible"}
	if apply {
		allowed["preview_digest"], allowed["confirm_channel"], allowed["confirm_version"], allowed["idempotency_key"] = true, true, true, true
		required = append(required, "preview_digest", "confirm_channel", "confirm_version", "idempotency_key")
	}
	values, err := parseDeploymentWebForm(w, r, allowed, required)
	if err != nil {
		return operator.DeploymentCreatePreviewRequest{}, "", err
	}
	batchSize, err := parseCanonicalInt(values.Get("batch_size"), 1, store.MaxDeploymentBatchSize)
	if err != nil {
		return operator.DeploymentCreatePreviewRequest{}, "", errors.New("batch size 不合法")
	}
	timeout, err := parseCanonicalInt(values.Get("execution_timeout_seconds"), 1, 86400)
	if err != nil {
		return operator.DeploymentCreatePreviewRequest{}, "", errors.New("execution timeout 不合法")
	}
	irreversible, err := strconv.ParseBool(values.Get("irreversible"))
	if err != nil || (values.Get("irreversible") != "true" && values.Get("irreversible") != "false") {
		return operator.DeploymentCreatePreviewRequest{}, "", errors.New("irreversible 必須是 true 或 false")
	}
	reason := strings.TrimSpace(values.Get("reason"))
	if len(reason) > 500 {
		return operator.DeploymentCreatePreviewRequest{}, "", errors.New("變更理由最多 500 bytes")
	}
	return operator.DeploymentCreatePreviewRequest{
		Channel: values.Get("channel"), Version: values.Get("version"), ArtifactSHA256: values.Get("artifact_sha256"),
		BatchSize: batchSize, ExecutionTimeoutSeconds: timeout, Irreversible: irreversible,
	}, reason, nil
}

func parseDeploymentWebForm(w http.ResponseWriter, r *http.Request, allowed map[string]bool, required []string) (url.Values, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery || r.URL.RawQuery != "" {
		return nil, errors.New("deployment form 不接受 query parameters")
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return nil, errors.New("deployment form 只接受 application/x-www-form-urlencoded")
	}
	if r.Body == nil {
		return nil, errors.New("deployment form 缺少 request body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, deploymentWebFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		return nil, errors.New("deployment form 編碼不合法")
	}
	for key, values := range r.PostForm {
		if !allowed[key] || len(values) != 1 {
			return nil, fmt.Errorf("deployment form 欄位 %q 不合法或重複", key)
		}
	}
	for _, key := range required {
		values, ok := r.PostForm[key]
		if !ok || len(values) != 1 || values[0] == "" {
			return nil, fmt.Errorf("deployment form 缺少 %s", key)
		}
	}
	return r.PostForm, nil
}

func parseCanonicalInt(raw string, min, max int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max || strconv.Itoa(value) != raw {
		return 0, errors.New("not a canonical bounded integer")
	}
	return value, nil
}

func parseCanonicalInt64(raw string, min, max int64) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < min || value > max || strconv.FormatInt(value, 10) != raw {
		return 0, errors.New("not a canonical bounded integer")
	}
	return value, nil
}

func deploymentTransportRejectionAction(action string) operator.DeploymentTransportRejectionAction {
	switch action {
	case "continue":
		return operator.DeploymentTransportRejectionContinue
	case "skip_failed_batch":
		return operator.DeploymentTransportRejectionSkipFailedBatch
	case "retry":
		return operator.DeploymentTransportRejectionRetry
	case "abandon":
		return operator.DeploymentTransportRejectionAbandon
	default:
		return operator.DeploymentTransportRejectionAction(action)
	}
}

func (s *Server) rejectDeploymentWebForm(w http.ResponseWriter, r *http.Request,
	action operator.DeploymentTransportRejectionAction, subject string, cause error,
) {
	if err := s.operator.RecordDeploymentTransportRejection(operator.DeploymentTransportRejectionRequest{
		Action: action, DeploymentID: subject,
		Actor: operator.ActorFromRequest(r, operator.SourceKindWeb),
	}); err != nil {
		log.Printf("deployment web transport rejection audit 寫不進去 action=%s: %v", action, err)
	}
	s.renderActionStatus(w, r, http.StatusBadRequest, subject,
		"沒有執行 deployment action", cause.Error(), deploymentBack(subject))
}

func (s *Server) renderDeploymentWebError(w http.ResponseWriter, r *http.Request, action, subject,
	headline, back string, err error,
) {
	status, _, detail := operator.HTTPError(err)
	if errors.Is(err, operator.ErrInvalidDeploymentPreview) || errors.Is(err, operator.ErrInvalidDeploymentAction) {
		status, detail = http.StatusBadRequest, "deployment settings 或 action request 不合法。"
	} else if errors.Is(err, store.ErrDeploymentNotFound) {
		status, detail = http.StatusNotFound, "找不到指定的 deployment。"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		detail = "這是原 request 的回放判決，未重新評估目前 artifact 或 deployment 狀態：" + detail
	}
	if status == http.StatusInternalServerError {
		log.Printf("deployment web %s 失敗: %v", action, err)
	}
	s.renderActionStatus(w, r, status, subject, headline, detail, back)
}

func (s *Server) renderDeploymentMutation(w http.ResponseWriter, r *http.Request,
	result operator.DeploymentMutationResult, freshHeadline, replayHeadline string,
) {
	w.Header().Set("Cache-Control", "no-store")
	headline := freshHeadline
	if result.Replayed {
		headline = replayHeadline
	}
	detail := fmt.Sprintf("state=%s；desired revision=%d；control revision=%d；opened batch=%d；new jobs=%d。",
		result.State, result.DesiredRevision, result.ControlRevision, result.OpenedBatch, len(result.Jobs))
	s.render(w, r, "action.html", page{
		Title: headline, Nav: "deployments-detail", Now: time.Now().Local().Format("2006-01-02 15:04"),
		Action: &actionResult{
			Subject: result.DeploymentID, Headline: headline, Detail: detail,
			Back: "/deployments/" + result.DeploymentID, BackLabel: "回該部署", Changed: true,
		},
	})
}

func deploymentBack(subject string) string {
	if strings.HasPrefix(subject, "deployment ") || subject == "" {
		return "/deployments?view=new"
	}
	return "/deployments/" + subject
}
