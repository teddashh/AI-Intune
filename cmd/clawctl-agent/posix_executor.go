package main

import (
	"context"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/model"
)

type unsupportedPlatformExecutor struct {
	kind string
	goos string
}

func (e unsupportedPlatformExecutor) Run(_ context.Context, _ model.JobResponse) ([]model.JobVerificationRequest, error) {
	return nil, fmt.Errorf("%w: %s job cannot run on %s", errUnsupported, e.kind, e.goos)
}

func executorsForGOOS(goos, hubURL, token string, now func() time.Time) (openclaw, nodeRuntime, hermes, claude, codex, grok, batServer, antigravity executor) {
	if now == nil {
		now = time.Now
	}
	openclaw = posixExecutorOrUnsupported(agentadapter.ExecutorKindOpenClaw, goos, func() executor {
		e := defaultOpenClawExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	nodeRuntime = posixExecutorOrUnsupported(agentadapter.ExecutorKindNodeRuntime, goos, func() executor {
		e := defaultNodeRuntimeExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	hermes = posixExecutorOrUnsupported(agentadapter.ExecutorKindHermes, goos, func() executor {
		e := defaultHermesExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	claude = posixExecutorOrUnsupported(agentadapter.ExecutorKindClaudeCode, goos, func() executor {
		e := defaultClaudeCodeExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	codex = posixExecutorOrUnsupported(agentadapter.ExecutorKindCodex, goos, func() executor {
		e := defaultCodexExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	grok = posixExecutorOrUnsupported(agentadapter.ExecutorKindGrok, goos, func() executor {
		e := defaultGrokExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	batServer = posixExecutorOrUnsupported(agentadapter.ExecutorKindBATServer, goos, func() executor {
		e := defaultBATServerExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	antigravity = posixExecutorOrUnsupported(agentadapter.ExecutorKindAntigravity, goos, func() executor {
		e := defaultAntigravityExecutor(hubURL, token)
		e.deps.now = now
		return e
	})
	return
}

func posixExecutorOrUnsupported(kind, goos string, construct func() executor) executor {
	if agentadapter.ExecutorAvailableOnGOOS(kind, goos) {
		return construct()
	}
	return unsupportedPlatformExecutor{kind: kind, goos: goos}
}
