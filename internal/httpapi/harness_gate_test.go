package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// The failure this prevents has a receipt: the Hermes harness stored projects
// and model selections it never read, and reported success. A member was told
// it worked.
func TestRequireHarnessFeature(t *testing.T) {
	for _, tc := range []struct {
		name    string
		agent   config.Agent
		feature harnessFeature
		allowed bool
	}{
		{
			name:    "picoclaw serves projects",
			agent:   config.Agent{Key: "alpha", Harness: config.HarnessPicoclaw},
			feature: featureProjects,
			allowed: true,
		},
		{
			name:    "an empty harness is picoclaw",
			agent:   config.Agent{Key: "alpha"},
			feature: featureProjects,
			allowed: true,
		},
		{
			// DF-3, closed by ganglion-projects slice A: the harness takes the
			// project as a header and keeps its subtree under the workspace it
			// already mounts, so a project scopes the transcripts, the window
			// and the files without a second bind. This row was `false` and the
			// change of fact is the feature.
			name:    "ganglion now serves projects",
			agent:   config.Agent{Key: "zcrab-g", Harness: config.HarnessGanglion},
			feature: featureProjects,
			allowed: true,
		},
		{
			// DF-4, closed by ganglion-model-registry: a ganglion container
			// reads a materialized registry file written from the same cascade
			// picoclaw's config.json comes from, so a member's own model
			// reaches it. This row was `false` and the change of fact is the
			// feature.
			name:    "ganglion now serves personal models",
			agent:   config.Agent{Key: "zcrab-g", Harness: config.HarnessGanglion},
			feature: featurePersonalModel,
			allowed: true,
		},
		{
			// DF-1, closed by ganglion-projects slice C: the harness has an MCP
			// client of its own and the proxy writes the server block into its
			// config.json, so it reaches the same graph picoclaw reaches from
			// the same workspace. This row was `false` and the change of fact is
			// the feature.
			name:    "ganglion now serves the memory graph",
			agent:   config.Agent{Key: "zcrab-g", Harness: config.HarnessGanglion},
			feature: featureMemoryGraph,
			allowed: true,
		},
		{
			// And the allowlist still refuses everything not declared: the
			// exemption is per (feature, harness) pair, not per harness and not
			// per feature. A third harness is refused by default, which is the
			// direction this table was written to fail in -- every row above
			// records a capability somebody BUILT.
			name:    "a third harness serves none of it",
			agent:   config.Agent{Key: "zcrab-x", Harness: "hermes"},
			feature: featureMemoryGraph,
			allowed: false,
		},
		{
			name:    "a third harness does not serve projects either",
			agent:   config.Agent{Key: "zcrab-x", Harness: "hermes"},
			feature: featureProjects,
			allowed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := requireHarnessFeature(rec, tc.agent, tc.feature)

			if got != tc.allowed {
				t.Fatalf("allowed = %v, want %v", got, tc.allowed)
			}
			if tc.allowed {
				if rec.Code != 200 || rec.Body.Len() != 0 {
					t.Errorf("an allowed feature wrote a response: %d %s", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != 501 {
				t.Errorf("code = %d, want 501", rec.Code)
			}
			// The message has to name the harness AND the agent, or the operator
			// reading it cannot tell which of several agents refused.
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body)
			}
			msg := strings.ToLower(rec.Body.String())
			// The harness from the CASE, not a literal: every refusing row used
			// to be ganglion, so a hardcoded name passed by coincidence and
			// would have gone on passing for a harness it never mentioned.
			if !strings.Contains(msg, strings.ToLower(tc.agent.Harness)) {
				t.Errorf("the refusal does not name the harness: %s", rec.Body)
			}
			if !strings.Contains(msg, tc.agent.Key) {
				t.Errorf("the refusal does not name the agent: %s", rec.Body)
			}
		})
	}
}

// The gate is an allowlist of what works, not a denylist of what does not: an
// unknown harness added later is refused by default rather than silently
// inheriting every picoclaw feature.
func TestRequireHarnessFeature_UnknownHarnessIsRefusedByDefault(t *testing.T) {
	rec := httptest.NewRecorder()
	ok := requireHarnessFeature(rec, config.Agent{Key: "x", Harness: "something-new"}, featureProjects)
	if ok {
		t.Error("an unknown harness was allowed a picoclaw-only feature")
	}
	if rec.Code != 501 {
		t.Errorf("code = %d, want 501", rec.Code)
	}
}

// SZ-2, as originally written, refused the cron routes on a scale-to-zero
// agent. That was wrong and the gate is gone: the routes are read-only, need
// no container, and cron.go already recorded that "a task the member cannot
// see is a task they cannot stop". What the mode actually breaks is the
// firing, which cronTasksResponse.Fires reports.
//
// This test remains to pin the part that survived: cron must NOT be
// harness-gated either, or a continuous ganglion agent -- which can serve it
// perfectly well -- would be refused.
func TestCronIsNotHarnessGated(t *testing.T) {
	rec := httptest.NewRecorder()
	if !requireHarnessFeature(rec, config.Agent{Key: "gamma", Harness: config.HarnessGanglion}, featureCron) {
		t.Error("cron is harness-gated; it is neither harness- nor mode-gated")
	}
}

// A client cannot tell whether a harness streams by looking at the frames --
// a slow terminal answer and a fast native one look alike for the first
// second. The header states it.
//
// The webapp's reveal driver depends on this: it exists only because picoclaw
// does not stream, and run over a harness that does, two mechanisms paint the
// same text and the reply visibly rewrites itself.
func TestStreamingModeFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent config.Agent
		want  string
	}{
		{"picoclaw answers in one frame", config.Agent{Harness: config.HarnessPicoclaw}, streamingTerminal},
		{"an empty harness is picoclaw", config.Agent{}, streamingTerminal},
		{"ganglion streams deltas", config.Agent{Harness: config.HarnessGanglion}, streamingNative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamingModeFor(tc.agent); got != tc.want {
				t.Errorf("streamingModeFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// The default must be the SAFE one: a harness nobody taught this function
// about is assumed not to stream, so a client keeps simulating rather than
// showing a reply that never animates.
func TestStreamingModeFor_UnknownHarnessIsTerminal(t *testing.T) {
	if got := streamingModeFor(config.Agent{Harness: "something-new"}); got != streamingTerminal {
		t.Errorf("streamingModeFor = %q, want the safe %q", got, streamingTerminal)
	}
}
