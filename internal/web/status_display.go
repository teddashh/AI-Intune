package web

import (
	"fmt"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
)

// statusDisplayLabel is UI-only; wire values and filter values stay canonical.
func statusDisplayLabel(s state.State) string {
	if s == state.Degraded {
		return "Needs attention"
	}
	return string(s)
}

// Build short reasons from structured states and finding categories, never from
// credential notes/errors or arbitrary diagnostic text (which may hold secrets).
func attentionReasons(s state.State, credentials []state.CredFact, findings []state.Finding, judgeReason ...string) string {
	if s != state.Degraded {
		return ""
	}
	var reasons []string
	add := func(reason string) {
		for _, existing := range reasons {
			if existing == reason {
				return
			}
		}
		reasons = append(reasons, reason)
	}
	for _, c := range credentials {
		provider := "Provider"
		switch c.Provider {
		case "grok", "claude", "codex", "gemini", "openai", "agy":
			provider = c.Provider
		}
		switch c.Status {
		case "expired":
			add(provider + " login expired")
		case "expires_soon":
			add(provider + " login expires soon")
		case "failed":
			add(provider + " request failed")
		}
	}
	credentialReasonCount := len(reasons)
	for _, f := range findings {
		if f.Severity < 2 && !f.Advisory {
			continue
		}
		reason := map[string]string{"memory": "Recent OOM kills", "disk": "Disk space low", "workload": "OpenClaw needs attention", "agent": "Agent needs attention", "clock": "Clock needs attention", "config": "Configuration needs attention", "credential": "Login needs attention", "machine": "Machine rebooting repeatedly"}[f.Kind]
		if f.Kind == "credential" && credentialReasonCount > 0 {
			continue
		}
		if f.Kind == "workload" && f.Advisory {
			reason = "OpenClaw advisory"
		}
		if reason == "" {
			reason = "Review diagnostic finding"
		}
		add(shortJudgeReason(f.Message, reason))
	}
	if len(reasons) == 0 {
		reason := "Review judge reason"
		if len(judgeReason) > 0 {
			reason = shortJudgeReason(judgeReason[0], reason)
		}
		add(reason)
	}
	if len(reasons) > 3 {
		return strings.Join(reasons[:3], ", ") + fmt.Sprintf(" +%d more", len(reasons)-3)
	}
	return strings.Join(reasons, ", ")
}

func detailAttentionReasons(j operator.MachineJudgement, e *operator.MachineEvidenceResult) string {
	var credentials []state.CredFact
	if e != nil {
		for _, c := range e.Credentials.Items {
			credentials = append(credentials, state.CredFact{Provider: c.Provider.Text, Status: string(c.Status), ExpiresAt: c.ExpiresAt})
		}
	}
	var findings []state.Finding
	for _, f := range j.Findings.Items {
		findings = append(findings, state.Finding{Kind: f.Kind, Severity: f.Severity, Advisory: f.Advisory, Message: f.Message.Text})
	}
	return attentionReasons(j.State, credentials, findings, j.Reason.Text)
}

// Recognize Hub judge wording but only emit fixed, safe summaries.
func shortJudgeReason(message, fallback string) string {
	for _, rule := range []struct{ match, label string }{
		{"OpenClaw process 不存在", "OpenClaw stopped"},
		{"狀態資料庫讀不到內容", "OpenClaw database unreadable"},
		{"找不到任何任務執行紀錄", "OpenClaw has no completed tasks"},
		{"沒有任何任務跑完", "OpenClaw tasks stalled"},
		{"crash-loop", "Agent restarting repeatedly"},
		{"還沒有送過完整觀測", "Agent observation missing"},
		{"狀態資料已經", "Agent observation stale"},
		{"未啟用 linger", "User services stop at logout"},
	} {
		if strings.Contains(message, rule.match) {
			return rule.label
		}
	}
	return fallback
}
