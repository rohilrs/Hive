package tabs

// TabGraduateRequest is the Projects tab → root request to open the Graduate
// modal for a project (Projects tab 'G' key). Mirrors
// TabFeatureBranchHealthRequest: only emitted for projects that have a feature
// branch configured (a project with no feature branch has nothing to
// graduate). The modal opens in its confirm state (mode selector defaulting to
// Dry-run); submit fires the async project.graduate RPC.
type TabGraduateRequest struct {
	Slug    string
	Feature string
	Target  string
}
