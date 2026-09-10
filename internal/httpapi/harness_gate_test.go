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
			name:    "ganglion does not serve projects",
			agent:   config.Agent{Key: "zcrab-g", Harness: config.HarnessGanglion},
			feature: featureProjects,
			allowed: false,
		},
		{
			name:    "ganglion does not serve personal models",
			agent:   config.Agent{Key: "zcrab-g", Harness: config.HarnessGanglion},
			feature: featurePersonalModel,
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
			if !strings.Contains(msg, "ganglion") {
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
