package httpapi

import (
	"fmt"
	"net/http"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Feature gating by harness.
//
// A feature a harness cannot serve answers 501, naming the harness. It does
// NOT quietly succeed.
//
// This is not defensiveness, it is a lesson with a receipt. The Hermes harness
// shipped with projects and personal models unimplemented, and both were
// picoclaw config.json constructs it never read -- so a project could be
// created, stored, listed and reported active while changing nothing about the
// agent that answered. The member is told it worked. A 501 is worse to receive
// and far better to debug.
//
// Every entry here corresponds to a DF-* row in
// .specs/features/crab-ganglion-harness/spec.md, so the gap is tracked rather
// than forgotten.

// requireContinuousMode writes a 501 and returns false when this agent's MODE
// cannot support the feature.
//
// Scheduled tasks are the case this exists for, and the property is about the
// MODE, not the harness: a schedule lives in in-process timers, so a stopped
// container fires nothing whatever runtime is inside it. Offering the feature
// on a scale-to-zero agent would store a task that never runs -- the same
// "reports success, changes nothing" failure the harness gate above exists to
// prevent, arrived at from the other direction.
//
// It refuses a scale-to-zero picoclaw agent too. None exists today; the rule is
// about the mode and pretending otherwise would leave a trap for the first one.
func requireContinuousMode(w http.ResponseWriter, a config.Agent, f harnessFeature) bool {
	if a.Mode == config.ModeContinuous {
		return true
	}
	writeJSON(w, http.StatusNotImplemented, errBody(fmt.Sprintf(
		"%s requires an agent in %q mode; agent %q runs in %q, and a stopped container fires no schedule",
		f, config.ModeContinuous, a.Key, a.Mode)))
	return false
}

// harnessFeature names a capability that not every harness provides.
type harnessFeature string

const (
	featureProjects      harnessFeature = "projects"
	featurePersonalModel harnessFeature = "personal model selection"
	featureCron          harnessFeature = "scheduled tasks"
	featureMemoryGraph   harnessFeature = "the memory graph"
)

// picoclawOnly lists what only the picoclaw harness serves today.
//
// Deliberately an allowlist of what WORKS rather than a denylist of what does
// not: a third harness added later is refused by default and has to be
// declared feature by feature, which fails in the safe direction.
//
// featureCron is deliberately ABSENT: it is gated by mode, not by harness --
// see requireContinuousMode.
var picoclawOnly = map[harnessFeature]bool{
	featureProjects:      true,
	featurePersonalModel: true,
	featureMemoryGraph:   true,
}

// requireHarnessFeature writes a 501 and returns false when this agent's
// harness cannot serve the feature.
func requireHarnessFeature(w http.ResponseWriter, agent config.Agent, f harnessFeature) bool {
	harness := agent.Harness
	if harness == "" {
		harness = config.HarnessPicoclaw
	}
	if harness == config.HarnessPicoclaw || !picoclawOnly[f] {
		return true
	}
	writeJSON(w, http.StatusNotImplemented, errBody(fmt.Sprintf(
		"%s is not available on the %q harness (agent %q)", f, harness, agent.Key)))
	return false
}
