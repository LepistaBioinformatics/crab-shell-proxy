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
// featureCron is deliberately ABSENT, and not because it moved to a mode gate
// either -- that gate was tried and removed. The cron routes are READ-ONLY and
// need no running container, and cron.go records why hiding a schedule is
// worse than showing an inert one. cronTasksResponse.Fires reports the truth
// instead of refusing.
var picoclawOnly = map[harnessFeature]bool{
	featureProjects:      true,
	featurePersonalModel: true,
	featureMemoryGraph:   true,
}

// alsoServedBy records the non-picoclaw harnesses that have since GROWN one of
// the features above.
//
// A second table rather than a deletion from the first, because the two say
// different things and both stay true: featurePersonalModel is still a picoclaw
// construct in origin, and a harness that has not implemented it is still
// refused by default. Removing the row instead would have opened the feature to
// every harness that ever exists, which is the failure direction this file was
// written to avoid.
//
// featurePersonalModel/ganglion: DF-4 of the harness spec, closed by
// ganglion-model-registry. A ganglion container reads a materialized registry
// file written from the same cascade, so a member's own model reaches it.
var alsoServedBy = map[harnessFeature]map[string]bool{
	featurePersonalModel: {config.HarnessGanglion: true},
}

// requireHarnessFeature writes a 501 and returns false when this agent's
// harness cannot serve the feature.
func requireHarnessFeature(w http.ResponseWriter, agent config.Agent, f harnessFeature) bool {
	harness := agent.Harness
	if harness == "" {
		harness = config.HarnessPicoclaw
	}
	if harness == config.HarnessPicoclaw || !picoclawOnly[f] || alsoServedBy[f][harness] {
		return true
	}
	writeJSON(w, http.StatusNotImplemented, errBody(fmt.Sprintf(
		"%s is not available on the %q harness (agent %q)", f, harness, agent.Key)))
	return false
}
