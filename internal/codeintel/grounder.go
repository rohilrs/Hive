package codeintel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// ErrScavengerUnavailable indicates capsule grounding could not be served
// (scavenger disabled, not installed, or the grounding worktree failed).
// Callers degrade to search_code + read_doc rather than aborting.
var ErrScavengerUnavailable = errors.New("codeintel: scavenger grounding unavailable")

// groundingLocks serializes grounding-worktree mutations per groundDir across
// all Grounder instances (multiple sessions may ground the same project).
var groundingLocks sync.Map // groundDir -> *sync.Mutex

func lockGrounding(dir string) func() {
	mu, _ := groundingLocks.LoadOrStore(dir, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// Grounder grounds a project against its target-branch ref: git-grep search
// (no checkout) + scavenger capsules from a cached, indexed grounding worktree.
// Construct with NewGrounder. Safe for concurrent use.
type Grounder struct {
	repoPath    string
	ref         string
	groundDir   string
	scavEnabled bool

	fetcher capsule.Fetcher
	indexFn func(context.Context, string) error
}

// NewGrounder builds a Grounder. scavBinary/indexTimeout configure the real
// scavenger index+capsule path; scavEnabled=false makes Capsule a no-op.
func NewGrounder(repoPath, ref, groundDir, scavBinary string, scavEnabled bool, indexTimeout time.Duration) *Grounder {
	return &Grounder{
		repoPath:    repoPath,
		ref:         ref,
		groundDir:   groundDir,
		scavEnabled: scavEnabled,
		fetcher:     capsule.NewCLIFetcher(capsule.Config{Binary: scavBinary}),
		indexFn:     scavengerIndex(scavBinary, indexTimeout),
	}
}

// Ref returns the grounding ref (the project's target branch).
func (g *Grounder) Ref() string { return g.ref }

// Search runs git grep for pattern against the grounding ref.
func (g *Grounder) Search(ctx context.Context, pattern string, maxHits int, globs ...string) ([]Hit, error) {
	return SearchCode(ctx, g.repoPath, g.ref, pattern, maxHits, globs...)
}

// Capsule returns a scavenger capsule for file[/symbol] from the grounding
// worktree (ensured fresh first). Returns a wrapped ErrScavengerUnavailable
// when scavenger is disabled or grounding fails.
func (g *Grounder) Capsule(ctx context.Context, file, symbol string) (*capsule.Capsule, error) {
	if !g.scavEnabled {
		return nil, ErrScavengerUnavailable
	}
	unlock := lockGrounding(g.groundDir)
	defer unlock()
	dir, err := ensureGrounding(ctx, g.repoPath, g.ref, g.groundDir, g.indexFn)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScavengerUnavailable, err)
	}
	return g.fetcher.Fetch(ctx, capsule.Req{File: file, Symbol: symbol, Cwd: dir})
}
