package claudecode

import (
	"testing"

	"github.com/rohilrs/Hive/internal/chat"
)

// TestPlannerToolNamesMatchRegistry guards the static PlannerToolNames list
// against drift from chat.NewPlannerRegistry. The CC planner advertises tools
// from PlannerToolNames; if a tool is added to the registry but not here, the
// subscription planner gets "No such tool available" at runtime (caught only by
// live smoke otherwise — exactly what happened with hive_search_code).
func TestPlannerToolNamesMatchRegistry(t *testing.T) {
	// base=nil so we get only the planner's own tools (no inherited chat tools).
	reg := chat.NewPlannerRegistry("/tmp/x", nil, "", nil)

	registry := map[string]bool{}
	for _, d := range reg.Defs() {
		registry[d.Name] = true
	}
	advertised := map[string]bool{}
	for _, n := range PlannerToolNames {
		advertised[n] = true
	}

	for name := range registry {
		if !advertised[name] {
			t.Errorf("planner registry tool %q is missing from PlannerToolNames — the CC planner won't advertise it", name)
		}
	}
	for name := range advertised {
		if !registry[name] {
			t.Errorf("PlannerToolNames lists %q which is not in the planner registry", name)
		}
	}
}
