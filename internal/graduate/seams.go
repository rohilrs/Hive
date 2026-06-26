package graduate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rohilrs/Hive/internal/config"
)

// SeamKind classifies the cross-boundary dispatch mechanism.
type SeamKind string

const (
	SeamRoute SeamKind = "route"
	SeamRPC   SeamKind = "rpc"
	SeamEvent SeamKind = "event"
)

// FileLine is an evidence location (1-based line).
type FileLine struct {
	File string
	Line int
}

// SeamSuspect is a dispatch key CALLED with no confidently-detected registration.
type SeamSuspect struct {
	Key       string
	Kind      SeamKind
	CallSites []FileLine
	Note      string // e.g. unresolved-dynamic registrations nearby
}

// SeamScan is the extractor result + coverage counts (observability; no silent under-scan).
type SeamScan struct {
	Suspects     []SeamSuspect
	FilesScanned int
	CallsSeen    int
	RegsSeen     int
}

var scanExts = map[string]bool{".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true}

var skipDirs = map[string]bool{"node_modules": true, ".git": true, "dist": true, "build": true, "vendor": true, ".next": true}

// dispatchRe captures an optional receiver, a verb, and the first string-literal
// argument: [recv.]verb('key') / "key" / `key`.
var dispatchRe = regexp.MustCompile(
	"(?:(\\w+)\\s*\\.\\s*)?([A-Za-z_]\\w*)\\s*\\(\\s*['\"`]([^'\"`]+)['\"`]")

// decoratorRe captures HTTP-method decorators: @Post('/x').
var decoratorRe = regexp.MustCompile("@(Post|Get|Put|Patch|Delete|All)\\s*\\(\\s*['\"`]([^'\"`]+)['\"`]")

// constAssignRe captures `name: '/value'` (object property) and `const NAME = '/value'`.
var constAssignRe = regexp.MustCompile("(?:(\\w+)\\s*:\\s*|(?:const|let|var)\\s+(\\w+)\\s*=\\s*)['\"`](/[^'\"`]+)['\"`]")

// dynamicRegRe detects a registration verb whose key arg is NOT a string literal
// (e.g. app.post(r.path, ...)) — an unresolvable dynamic registration.
var dynamicRegRe = regexp.MustCompile(
	"\\b(?:app|router|r|fastify|server|mux)\\s*\\.\\s*(?:post|get|put|patch|delete|all|use|route)\\s*\\(\\s*[A-Za-z_]")

// caseLabelRe captures a string dispatch arm: case "method": (Go/TS switch).
var caseLabelRe = regexp.MustCompile("case\\s+['\"`]([^'\"`]+)['\"`]\\s*:")

// dispatchSwitchRe detects a switch on a dispatch-like variable, e.g.
// `switch method {`, `switch req.Method {`, `switch cmd {`. Case arms only count
// as registrations in a file that has one (recall-safe: a file with an unusual
// dispatch var loses its case-arm registrations → its handlers become suspects
// the verify pass refutes, rather than an unrelated switch silently masking a
// real unwired call).
var dispatchSwitchRe = regexp.MustCompile("(?i)\\bswitch\\s+[\\w.]*\\b(method|cmd|command|action|rpc|msgtype|msg_type|opcode|op|kind|type|route|event|name)\\b[\\w.]*\\s*\\{")

var (
	// regVerbsStrong: verbs that count as a registration regardless of receiver.
	// NOTE: on/once/subscribe are treated as registrations even when used as a
	// consumer-side subscription — a deliberate (spec'd) tradeoff that can suppress
	// an event suspect; revisit if event false-negatives show up in practice.
	regVerbsStrong  = set("handlefunc", "handle", "route", "use", "on", "once", "subscribe", "addeventlistener")
	httpVerbs       = set("post", "get", "put", "patch", "delete", "all")
	callVerbsStrong = set("request", "fetch", "axios", "emit", "publish", "dispatch", "call", "invoke", "rpccall", "send")
	// routerRecv: receiver names that make an ambiguous HTTP verb (post/get/...) a
	// REGISTRATION. Kept deliberately conservative (recall-first): a client wrapper
	// named like a router here would mask a real unwired call (false negative). Common
	// double-use names (api, server, r) are EXCLUDED for that reason — e.g. `r.Get("/x")`
	// is often a resty/http client, not a chi router. A project whose real router uses
	// one of these (e.g. chi's `r`) adds it back via SeamPatterns.RouterReceivers config.
	routerRecv = set("app", "router", "fastify", "mux", "route", "routes")
	eventVerbs = set("on", "once", "subscribe", "addeventlistener", "emit", "publish", "dispatch")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

type occRole int

const (
	roleCall occRole = iota
	roleReg
)

// classify decides call vs registration, biased toward CALL. An occurrence is a
// registration ONLY on a strong registration verb or an ambiguous HTTP verb with
// a known router receiver. Everything else is a call.
func classify(recv, verb string, p config.SeamPatterns) occRole {
	v := strings.ToLower(verb)
	rc := strings.ToLower(recv)
	if regVerbsStrong[v] || contains(p.RegVerbs, v) {
		return roleReg
	}
	if httpVerbs[v] && (routerRecv[rc] || contains(p.RouterReceivers, rc)) {
		return roleReg
	}
	return roleCall
}

// kindOf infers the seam kind from verb + key shape.
func kindOf(verb, key string) SeamKind {
	v := strings.ToLower(verb)
	switch {
	case eventVerbs[v] && !strings.HasPrefix(key, "/"):
		return SeamEvent
	case httpVerbs[v] || strings.HasPrefix(key, "/"):
		return SeamRoute
	default:
		return SeamRPC
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

type keyOcc struct {
	kind  SeamKind
	calls []FileLine
	regs  int
}

// ExtractSeamSuspects scans worktree repo-wide and returns suspect unwired seams:
// keys with >=1 call and 0 confident registrations. Deterministic, recall-biased.
// No suspect is ever silently dropped — when dynamic registrations exist, suspects
// carry a Note for the verify pass to resolve against the real route table.
func ExtractSeamSuspects(worktree string, patterns config.SeamPatterns) (SeamScan, error) {
	byKey := map[string]*keyOcc{}
	scan := SeamScan{}
	constPaths := map[string]bool{}
	hasDynamicReg := false

	walkErr := filepath.WalkDir(worktree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !scanExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel, _ := filepath.Rel(worktree, path)
		if matchesAnyGlob(rel, patterns.ExcludeGlobs) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		scan.FilesScanned++
		content := string(data)
		for _, m := range constAssignRe.FindAllStringSubmatch(content, -1) {
			constPaths[m[3]] = true
		}
		if dynamicRegRe.MatchString(content) {
			hasDynamicReg = true
		}
		hasDispatchSwitch := dispatchSwitchRe.MatchString(content)
		scanFileInto(rel, content, patterns, hasDispatchSwitch, byKey, &scan)
		return nil
	})
	if walkErr != nil {
		return scan, walkErr
	}

	for key, occ := range byKey {
		if len(occ.calls) > 0 && occ.regs == 0 {
			s := SeamSuspect{Key: key, Kind: occ.kind, CallSites: occ.calls}
			// NEVER drop a call-without-static-registration (the extractor's core
			// recall guarantee). When dynamic registrations exist the suspect may be
			// wired at runtime, so attach a Note for the verify pass instead of
			// dropping — only the verifier, reading the real route table, can confirm.
			switch {
			case constPaths[key] && hasDynamicReg:
				s.Note = "path matches a const value and dynamic registrations are present; likely wired at runtime — verify the route table"
			case hasDynamicReg:
				s.Note = "dynamic registrations present that the extractor could not resolve; verify the route table"
			}
			scan.Suspects = append(scan.Suspects, s)
		}
	}
	return scan, nil
}

func scanFileInto(rel, content string, p config.SeamPatterns, hasDispatchSwitch bool, byKey map[string]*keyOcc, scan *SeamScan) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lineNo := i + 1
		if hasDispatchSwitch {
			for _, m := range caseLabelRe.FindAllStringSubmatch(line, -1) {
				key := m[1]
				occ := ensureOcc(byKey, key, kindOf("", key))
				occ.regs++
				scan.RegsSeen++
			}
		}
		for _, m := range decoratorRe.FindAllStringSubmatch(line, -1) {
			key := m[2]
			occ := ensureOcc(byKey, key, kindOf(m[1], key))
			occ.regs++
			scan.RegsSeen++
		}
		for _, m := range dispatchRe.FindAllStringSubmatch(line, -1) {
			recv, verb, key := m[1], m[2], m[3]
			v := strings.ToLower(verb)
			if strings.Contains(key, "://") {
				continue // external URL, never an internal seam
			}
			if !isDispatchVerb(v, p) && !strings.HasPrefix(key, "/") {
				continue
			}
			occ := ensureOcc(byKey, key, kindOf(verb, key))
			if classify(recv, verb, p) == roleReg {
				occ.regs++
				scan.RegsSeen++
			} else {
				occ.calls = append(occ.calls, FileLine{File: rel, Line: lineNo})
				scan.CallsSeen++
			}
		}
	}
}

func isDispatchVerb(v string, p config.SeamPatterns) bool {
	return regVerbsStrong[v] || httpVerbs[v] || callVerbsStrong[v] || eventVerbs[v] ||
		contains(p.CallVerbs, v) || contains(p.RegVerbs, v)
}

func ensureOcc(byKey map[string]*keyOcc, key string, kind SeamKind) *keyOcc {
	if o, ok := byKey[key]; ok {
		return o
	}
	o := &keyOcc{kind: kind}
	byKey[key] = o
	return o
}

func matchesAnyGlob(rel string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, rel); ok {
			return true
		}
	}
	return false
}
