// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

type aSpec = v1alpha1.AgentManifestSpec

func f(v float64) *float64 { return &v }
func i(v int64) *int64     { return &v }
func h(n byte) string      { return "sha256:" + strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

func base() *v1alpha1.AgentManifestSpec {
	at := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	return &v1alpha1.AgentManifestSpec{
		Agent: "support-agent",
		Code:  v1alpha1.CodeSpec{Image: "registry.internal/support-agent@" + h(1)},
		Model: v1alpha1.ModelSpec{Provider: "anthropic", ID: "claude-sonnet-4-6", Pin: "2026-05-01",
			Params: v1alpha1.ModelParams{Temperature: f(0.2), TopP: f(1), MaxTokens: i(4096)}},
		Prompts: []v1alpha1.PromptRef{{Name: "system", Source: "git+ssh://git.internal/prompts@a1b2c3d", Digest: h(2)}},
		Tools: []v1alpha1.ToolRef{
			{Name: "jira.create_issue", Server: "https://jira-gw.internal/mcp", ContractDigest: h(4),
				Effect: v1alpha1.EffectWrite, ReplayPolicy: v1alpha1.ReplayNoOp},
			{Name: "docs.search", Server: "https://docs-gw.internal/mcp", ContractDigest: h(3),
				Effect: v1alpha1.EffectRead, ReplayPolicy: v1alpha1.ReplaySnapshot},
		},
		Retrieval: &v1alpha1.RetrievalSpec{IndexSnapshot: "snap-2026-08-19T04:00Z", EmbeddingModel: "voyage-3"},
		Policy:    &v1alpha1.PolicySpec{BundleDigest: h(5)},
		Judges: []v1alpha1.JudgeSpec{{Name: "task-completion", Model: "claude-opus-4-1", RubricDigest: h(6),
			Calibration: &v1alpha1.JudgeCalibration{Kappa: f(0.71), GoldSetDigest: h(7), MeasuredAt: &at}}},
	}
}

// scenarioBehavioural is the worked example from the design: a model pin bump,
// a temperature change, a rewritten tool description, a new write tool, a
// changed prompt, and a judge swapped without re-calibration.
func scenarioBehavioural() (*v1alpha1.AgentManifestSpec, *v1alpha1.AgentManifestSpec) {
	a, b := base(), base()
	b.Model.Pin = "2026-08-01"
	b.Model.Params.Temperature = f(0.7)
	b.Tools[0].ContractDigest = h(9)
	b.Tools = append(b.Tools, v1alpha1.ToolRef{
		Name: "slack.post_message", Server: "https://slack-gw.internal/mcp", ContractDigest: h(10),
		Effect: v1alpha1.EffectWrite, ReplayPolicy: v1alpha1.ReplayNoOp})
	b.Prompts[0].Digest = h(11)
	b.Judges[0].Model = "claude-opus-5"
	return a, b
}

// scenarioProvenance is the registry migration: identical bytes, new registry,
// plus a prompt moved between repositories and a re-measured judge.
func scenarioProvenance() (*v1alpha1.AgentManifestSpec, *v1alpha1.AgentManifestSpec) {
	a, b := base(), base()
	b.Code.Image = "mirror.example.com/team/support-agent@" + h(1)
	b.Prompts[0].Source = "git+ssh://git.internal/platform-prompts@a1b2c3d"
	at := metav1.NewTime(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	b.Judges[0].Calibration.Kappa = f(0.78)
	b.Judges[0].Calibration.MeasuredAt = &at
	return a, b
}

func scenarioIdentical() (*v1alpha1.AgentManifestSpec, *v1alpha1.AgentManifestSpec) {
	return base(), base()
}

func scenarioCrossAgent() (*v1alpha1.AgentManifestSpec, *v1alpha1.AgentManifestSpec) {
	a, b := base(), base()
	b.Agent = "billing-agent"
	return a, b
}
