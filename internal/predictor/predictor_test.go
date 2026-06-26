package predictor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

type fakeHaikuClient struct {
	candidates []anthropic.Candidate
	err        error
	calls      int
}

func (f *fakeHaikuClient) PredictFiles(_ context.Context, _ anthropic.PredictionRequest) ([]anthropic.Candidate, error) {
	f.calls++
	return f.candidates, f.err
}

type fakeFetcher struct {
	capsules map[string]*capsule.Capsule
	errs     map[string]error
}

func (f *fakeFetcher) Fetch(_ context.Context, req capsule.Req) (*capsule.Capsule, error) {
	key := req.File
	if req.Symbol != "" {
		key += ":" + req.Symbol
	}
	if err, ok := f.errs[key]; ok && err != nil {
		return nil, err
	}
	c, ok := f.capsules[key]
	if !ok {
		c = &capsule.Capsule{Raw: "default fixture", TokenEstimate: 100}
	}
	return c, nil
}

func TestPredictHappyPath(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir() // contents irrelevant — only path is used
	// Stage a couple of repo files so the listing isn't empty.
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("package x"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, "b.go"), []byte("package x"), 0o644)

	p := &Predictor{
		SDK: &fakeHaikuClient{candidates: []anthropic.Candidate{
			{File: "a.go", Symbol: "Run", Score: 0.9, Reason: "primary"},
			{File: "b.go", Score: 0.7, Reason: "helper"},
		}},
		Fetcher: &fakeFetcher{capsules: map[string]*capsule.Capsule{
			"a.go:Run": {Target: "func Run(...)", Body: "func Run(...){}", Raw: "[TARGET]\nfunc Run(...)\n[BODY]\nfunc Run(...){}", TokenEstimate: 50},
			"b.go":     {Target: "package b", Body: "...", Raw: "[TARGET]\npackage b", TokenEstimate: 30},
		}},
		Cfg: Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second, HaikuTimeout: time.Second},
	}

	res, err := p.Predict(context.Background(), "fix the dispatch race", repoRoot, bundleDir)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(res.Files) != 2 {
		t.Errorf("Files=%v want 2 entries", res.Files)
	}
	if len(res.InlineCapsules) != 2 {
		t.Errorf("InlineCapsules len=%d want 2 (both fit under cap)", len(res.InlineCapsules))
	}
	if res.FullBundlePath == "" {
		t.Errorf("FullBundlePath empty; expected prefetch.md path")
	}
	body, err := os.ReadFile(res.FullBundlePath)
	if err != nil {
		t.Fatalf("read prefetch.md: %v", err)
	}
	if !strings.Contains(string(body), "func Run(...)") {
		t.Errorf("prefetch.md missing capsule content: %s", body)
	}
}

func TestPredictCapsCapsulesAtTokenBudget(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	// 5 candidates, each capsule estimated at 300 tokens, cap at 700:
	// expect top 2 to land inline, remaining 3 in overflow.
	cands := []anthropic.Candidate{
		{File: "a.go", Symbol: "S1", Score: 0.9, Reason: ""},
		{File: "a.go", Symbol: "S2", Score: 0.8, Reason: ""},
		{File: "a.go", Symbol: "S3", Score: 0.7, Reason: ""},
		{File: "a.go", Symbol: "S4", Score: 0.6, Reason: ""},
		{File: "a.go", Symbol: "S5", Score: 0.5, Reason: ""},
	}
	caps := map[string]*capsule.Capsule{}
	for _, c := range cands {
		caps["a.go:"+c.Symbol] = &capsule.Capsule{
			Target: c.Symbol, Body: "body", Raw: "raw " + c.Symbol, TokenEstimate: 300,
		}
	}
	p := &Predictor{
		SDK:     &fakeHaikuClient{candidates: cands},
		Fetcher: &fakeFetcher{capsules: caps},
		Cfg:     Config{BundleTokenCap: 700, MaxCandidates: 10, PerCallTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "x", repoRoot, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.InlineCapsules) != 2 {
		t.Errorf("InlineCapsules len=%d want 2 (300+300<=700; 3rd would exceed)", len(res.InlineCapsules))
	}
	if len(res.Overflow) != 3 {
		t.Errorf("Overflow len=%d want 3 (the remaining candidates)", len(res.Overflow))
	}
	if !res.Metrics.Truncated {
		t.Errorf("Metrics.Truncated should be true when overflow exists")
	}
}

func TestPredictDegradesOnHaikuError(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	p := &Predictor{
		SDK:     &fakeHaikuClient{err: context.DeadlineExceeded},
		Fetcher: &fakeFetcher{},
		Cfg:     Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "x", repoRoot, bundleDir)
	if err != nil {
		t.Errorf("expected graceful degrade, got err=%v", err)
	}
	if res != nil {
		t.Errorf("expected nil result on Haiku error, got %+v", res)
	}
}

func TestPredictSkipsCapsuleFetchErrors(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	cands := []anthropic.Candidate{
		{File: "good.go", Score: 0.9, Reason: ""},
		{File: "bad.go", Score: 0.8, Reason: ""},
	}
	p := &Predictor{
		SDK: &fakeHaikuClient{candidates: cands},
		Fetcher: &fakeFetcher{
			capsules: map[string]*capsule.Capsule{
				"good.go": {Raw: "good", TokenEstimate: 50, Target: "g"},
			},
			errs: map[string]error{"bad.go": context.DeadlineExceeded},
		},
		Cfg: Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "x", repoRoot, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	// Both candidates show up in Files (Files = the raw candidate list).
	if len(res.Files) != 2 {
		t.Errorf("Files=%v want 2 (both candidates regardless of fetch outcome)", res.Files)
	}
	// Only good.go has an inline capsule; bad.go is silently skipped.
	if len(res.InlineCapsules) != 1 {
		t.Errorf("InlineCapsules len=%d want 1 (bad.go skipped)", len(res.InlineCapsules))
	}
}

func TestPredictDefensiveMaxCandidatesCap(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	// Haiku ignores MaxCandidates and returns 15 — predictor must cap to 5.
	var cands []anthropic.Candidate
	for i := 0; i < 15; i++ {
		cands = append(cands, anthropic.Candidate{
			File: "a.go", Symbol: fmt.Sprintf("S%d", i), Score: float64(15-i) / 15.0,
		})
	}
	p := &Predictor{
		SDK:     &fakeHaikuClient{candidates: cands},
		Fetcher: &fakeFetcher{},
		Cfg:     Config{BundleTokenCap: 100000, MaxCandidates: 5, PerCallTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "x", repoRoot, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) > 5 {
		t.Errorf("Files len=%d want <=5 (capped at MaxCandidates)", len(res.Files))
	}
}

func TestPredictBundleHeadersMatchCapsulesUnderSkip(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	// Haiku returns 3 candidates; fetcher fails on the MIDDLE one.
	// The 3rd candidate's header should match the 3rd candidate's
	// capsule body, NOT the 2nd (which got dropped).
	cands := []anthropic.Candidate{
		{File: "first.go", Score: 0.9, Reason: "primary"},
		{File: "middle.go", Score: 0.8, Reason: "skipped due to fetch fail"},
		{File: "last.go", Score: 0.7, Reason: "tertiary"},
	}
	p := &Predictor{
		SDK: &fakeHaikuClient{candidates: cands},
		Fetcher: &fakeFetcher{
			capsules: map[string]*capsule.Capsule{
				"first.go": {Target: "FIRST_TARGET", Raw: "FIRST_BODY", TokenEstimate: 50},
				"last.go":  {Target: "LAST_TARGET", Raw: "LAST_BODY", TokenEstimate: 50},
			},
			errs: map[string]error{"middle.go": context.DeadlineExceeded},
		},
		Cfg: Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "x", repoRoot, bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(res.FullBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)

	// last.go's section MUST contain its own reason, not middle.go's.
	if !strings.Contains(bodyStr, "LAST_BODY") {
		t.Errorf("missing LAST_BODY in bundle:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "last.go") {
		t.Errorf("missing last.go in bundle:\n%s", bodyStr)
	}
	if strings.Contains(bodyStr, "skipped due to fetch fail") {
		t.Errorf("bundle should NOT contain middle.go's reason next to last.go's capsule:\n%s", bodyStr)
	}
	// The reasoning that matters: middle.go's metadata should never
	// appear in the bundle since its capsule wasn't fetched.
	if strings.Contains(bodyStr, "middle.go") {
		t.Errorf("middle.go should not appear in bundle at all:\n%s", bodyStr)
	}
}

// --- cache tests ---

// newCachingTestPredictor returns a Predictor with a 10-slot LRU cache,
// matching what NewPredictor wires for production but with test-injected
// SDK + Fetcher.
func newCachingTestPredictor(sdk SDK, fetcher capsule.Fetcher) *Predictor {
	return &Predictor{
		SDK:     sdk,
		Fetcher: fetcher,
		Cfg:     Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second, HaikuTimeout: time.Second},
		cache:   newPredictorCache(10),
	}
}

func TestPredictReturnsCachedResultOnSecondCall(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	sdk := &fakeHaikuClient{candidates: []anthropic.Candidate{
		{File: "a.go", Score: 0.9, Reason: "primary"},
	}}
	fetcher := &fakeFetcher{capsules: map[string]*capsule.Capsule{
		"a.go": {Raw: "CAPSULE_A", TokenEstimate: 50},
	}}
	p := newCachingTestPredictor(sdk, fetcher)

	res1, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir)
	if err != nil || res1 == nil {
		t.Fatalf("first Predict failed: res=%v err=%v", res1, err)
	}
	if sdk.calls != 1 {
		t.Fatalf("first Predict should have called SDK once, got calls=%d", sdk.calls)
	}

	res2, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir)
	if err != nil || res2 == nil {
		t.Fatalf("second Predict failed: res=%v err=%v", res2, err)
	}
	if sdk.calls != 1 {
		t.Errorf("second Predict should hit cache; SDK calls=%d want 1", sdk.calls)
	}
	if res2.Metrics.HaikuLatency != 0 {
		t.Errorf("cached HaikuLatency should be zero; got %v", res2.Metrics.HaikuLatency)
	}
	if res2.Metrics.FetchLatency != 0 {
		t.Errorf("cached FetchLatency should be zero; got %v", res2.Metrics.FetchLatency)
	}
	if len(res2.Files) != 1 || res2.Files[0] != "a.go" {
		t.Errorf("cached Files=%v want [a.go]", res2.Files)
	}
}

func TestPredictCacheRewritesBundleToNewBundleDir(t *testing.T) {
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	sdk := &fakeHaikuClient{candidates: []anthropic.Candidate{
		{File: "a.go", Score: 0.9, Reason: "primary"},
	}}
	fetcher := &fakeFetcher{capsules: map[string]*capsule.Capsule{
		"a.go": {Raw: "CAPSULE_A_RAW", TokenEstimate: 50},
	}}
	p := newCachingTestPredictor(sdk, fetcher)

	bundleDir1 := t.TempDir()
	res1, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir1)
	if err != nil || res1 == nil {
		t.Fatalf("first Predict failed: res=%v err=%v", res1, err)
	}

	bundleDir2 := t.TempDir()
	res2, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir2)
	if err != nil || res2 == nil {
		t.Fatalf("second Predict failed: res=%v err=%v", res2, err)
	}
	if sdk.calls != 1 {
		t.Errorf("second Predict should hit cache; SDK calls=%d", sdk.calls)
	}
	// New bundlePath under bundleDir2.
	wantPath := filepath.Join(bundleDir2, "prefetch.md")
	if res2.FullBundlePath != wantPath {
		t.Errorf("FullBundlePath=%s want %s", res2.FullBundlePath, wantPath)
	}
	body, err := os.ReadFile(res2.FullBundlePath)
	if err != nil {
		t.Fatalf("read new bundle: %v", err)
	}
	if !strings.Contains(string(body), "CAPSULE_A_RAW") {
		t.Errorf("cached bundle missing capsule content:\n%s", body)
	}
	// Sanity: original bundle still exists, separate.
	if _, err := os.Stat(filepath.Join(bundleDir1, "prefetch.md")); err != nil {
		t.Errorf("original bundle disappeared: %v", err)
	}
}

func TestPredictCacheKeyVariesByRepoRoot(t *testing.T) {
	repoRoot1 := t.TempDir()
	repoRoot2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot1, "a.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot2, "a.go"), []byte("x"), 0o644)

	sdk := &fakeHaikuClient{candidates: []anthropic.Candidate{
		{File: "a.go", Score: 0.9},
	}}
	fetcher := &fakeFetcher{}
	p := newCachingTestPredictor(sdk, fetcher)

	bundleDir := t.TempDir()
	if _, err := p.Predict(context.Background(), "same task", repoRoot1, bundleDir); err != nil {
		t.Fatal(err)
	}
	bundleDir2 := t.TempDir()
	if _, err := p.Predict(context.Background(), "same task", repoRoot2, bundleDir2); err != nil {
		t.Fatal(err)
	}
	if sdk.calls != 2 {
		t.Errorf("different repoRoot should miss cache; SDK calls=%d want 2", sdk.calls)
	}
}

func TestPredictCacheNotPopulatedOnHaikuDegrade(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()

	sdk := &fakeHaikuClient{err: context.DeadlineExceeded}
	fetcher := &fakeFetcher{}
	p := newCachingTestPredictor(sdk, fetcher)

	res, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir)
	if err != nil || res != nil {
		t.Fatalf("expected (nil,nil) on degrade; got res=%v err=%v", res, err)
	}
	if sdk.calls != 1 {
		t.Fatalf("first call should have hit SDK; calls=%d", sdk.calls)
	}
	// Second call must NOT short-circuit on cache — degrade results shouldn't
	// be memoized, so the SDK is hit again.
	res, err = p.Predict(context.Background(), "task body", repoRoot, bundleDir)
	if err != nil || res != nil {
		t.Fatalf("expected (nil,nil) on second degrade; got res=%v err=%v", res, err)
	}
	if sdk.calls != 2 {
		t.Errorf("Haiku degrade must not poison cache; SDK calls=%d want 2", sdk.calls)
	}
}

func TestPredictWithoutCacheStillWorks(t *testing.T) {
	bundleDir := t.TempDir()
	repoRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("x"), 0o644)

	// Construct directly with zero-value cache field (nil) — back-compat.
	p := &Predictor{
		SDK: &fakeHaikuClient{candidates: []anthropic.Candidate{
			{File: "a.go", Score: 0.9},
		}},
		Fetcher: &fakeFetcher{capsules: map[string]*capsule.Capsule{
			"a.go": {Raw: "x", TokenEstimate: 50},
		}},
		Cfg: Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second, HaikuTimeout: time.Second},
	}
	res, err := p.Predict(context.Background(), "task body", repoRoot, bundleDir)
	if err != nil {
		t.Fatalf("nil-cache Predict errored: %v", err)
	}
	if res == nil {
		t.Fatalf("nil-cache Predict returned nil result")
	}
	if len(res.Files) != 1 {
		t.Errorf("Files=%v want 1", res.Files)
	}
}

func TestNewPredictorWiresCache(t *testing.T) {
	// Sanity: production constructor wires a non-nil cache so cache-hit
	// behavior is on by default. We don't exercise the cache here — just
	// confirm the field is populated.
	p := NewPredictor(
		&fakeHaikuClient{},
		&fakeFetcher{},
		Config{BundleTokenCap: 1000, MaxCandidates: 10, PerCallTimeout: time.Second, HaikuTimeout: time.Second},
	)
	if p.cache == nil {
		t.Errorf("NewPredictor should wire a cache")
	}
}
