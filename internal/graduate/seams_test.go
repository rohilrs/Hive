package graduate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/config"
)

func osMkdirAll(p string) error     { return os.MkdirAll(filepath.Dir(p), 0o755) }
func osWriteFile(p, c string) error { return os.WriteFile(p, []byte(c), 0o644) }

// writeFixture writes files into a temp dir and returns the dir.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := dir + "/" + rel
		if err := osMkdirAll(p); err != nil {
			t.Fatal(err)
		}
		if err := osWriteFile(p, content); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func suspectKeys(s SeamScan) map[string]SeamSuspect {
	m := map[string]SeamSuspect{}
	for _, x := range s.Suspects {
		m[x.Key] = x
	}
	return m
}

func keysOf(m map[string]SeamSuspect) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExtractRouteSeam_MissingRegistration(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"proxy.ts":           `this.post('/select-payment-method', input);`,
		"adapter-factory.ts": `this.post('/pms/select-payment-method', input);`,
		"routes.ts":          "app.post('/select-room', h);\napp.post('/fill-guest-info-form', h);",
	})
	scan, err := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if err != nil {
		t.Fatal(err)
	}
	sus := suspectKeys(scan)
	if _, ok := sus["/select-payment-method"]; !ok {
		t.Fatalf("expected /select-payment-method suspect, got %v", keysOf(sus))
	}
	if _, ok := sus["/select-room"]; ok {
		t.Error("/select-room is registered; must not be a suspect")
	}
}

func TestExtractRouteSeam_Wired(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"client.ts": `this.post('/ok', body);`,
		"routes.ts": `app.post('/ok', handler);`,
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if len(scan.Suspects) != 0 {
		t.Fatalf("wired route must yield 0 suspects, got %v", scan.Suspects)
	}
}

func TestExtractRouteSeam_AmbiguousCountsAsCall(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"x.ts": `gateway.post('/maybe', body);`,
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if _, ok := suspectKeys(scan)["/maybe"]; !ok {
		t.Fatalf("ambiguous dispatch with no registration must be a suspect; got %v", scan.Suspects)
	}
}

func TestExtractRoute_ConstPlusDynamicKeepsSuspectWithNote(t *testing.T) {
	// Recall-first: even when a path matches a const value AND dynamic registrations
	// exist (so it MIGHT be wired at runtime), the call-without-static-registration
	// is NOT dropped — it stays a suspect with a Note for the verify pass.
	dir := writeFixture(t, map[string]string{
		"const.ts":  "const ROUTES = { pay: '/pay' };",
		"client.ts": "this.post('/pay', body);",
		"routes.ts": "app.post(ROUTES.pay, handler);",
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	s, ok := suspectKeys(scan)["/pay"]
	if !ok {
		t.Fatalf("/pay must remain a suspect (never silently dropped)")
	}
	if s.Note == "" {
		t.Errorf("const+dynamic suspect must carry a Note for the verifier")
	}
}

func TestExtractRoute_UnresolvableDynamicGetsNote(t *testing.T) {
	// Routes registered dynamically from an unresolved source; a call to '/dyn'
	// has no static registration → still a suspect, but carrying a Note.
	dir := writeFixture(t, map[string]string{
		"client.ts": "this.post('/dyn', body);",
		"routes.ts": "for (const r of loadRoutes()) { app.post(r.path, r.handler); }",
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	s, ok := suspectKeys(scan)["/dyn"]
	if !ok {
		t.Fatalf("/dyn must remain a suspect")
	}
	if s.Note == "" {
		t.Errorf("suspect must carry a dynamic-registration Note when unresolved dynamic registrations exist")
	}
}

func TestExtractRoute_UnrelatedDynamicRegDoesNotDropSuspect(t *testing.T) {
	// /x is declared as a const value and called but NEVER registered; a dynamic
	// registration in an UNRELATED file must NOT cause /x to be dropped.
	dir := writeFixture(t, map[string]string{
		"const.ts":  "const PATHS = { foo: '/x' };",
		"client.ts": "this.post('/x', body);",
		"other.ts":  "app.post(other.path, handler);", // unrelated dynamic reg
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if _, ok := suspectKeys(scan)["/x"]; !ok {
		t.Fatalf("/x is genuinely unwired; an unrelated dynamic reg must not drop it")
	}
}

func TestExtractRPCSeam_CaseSwitchRegistration(t *testing.T) {
	// An RPC method called but with no `case "m":` dispatch arm → suspect.
	dir := writeFixture(t, map[string]string{
		"client.go":   `c.call("project.ghost")`,
		"dispatch.go": "switch method {\ncase \"project.remediate\":\n\treturn h()\n}",
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	sus := suspectKeys(scan)
	s, ok := sus["project.ghost"]
	if !ok {
		t.Fatalf("project.ghost has no case arm → suspect; got %v", keysOf(sus))
	}
	if s.Kind != SeamRPC {
		t.Errorf("kind=%s want rpc", s.Kind)
	}
	if _, ok := sus["project.remediate"]; ok {
		t.Error("project.remediate has a case arm and no call → not a (call) suspect")
	}
}

func TestExtractRPC_UnrelatedCaseSwitchDoesNotSuppress(t *testing.T) {
	// "pending" is a called RPC method AND appears as a case arm in an UNRELATED
	// status switch (switch status). The unrelated case arm must NOT register it.
	dir := writeFixture(t, map[string]string{
		"client.go": `c.call("pending")`,
		"status.go": "switch status {\ncase \"pending\":\n\treturn wait()\n}",
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if _, ok := suspectKeys(scan)["pending"]; !ok {
		t.Fatalf("unrelated status-switch case arm must not suppress the unwired RPC call 'pending'")
	}
}

func TestExtractEventSeam_OnEmit(t *testing.T) {
	// An emitted event with no .on/.subscribe listener → suspect.
	dir := writeFixture(t, map[string]string{
		"emit.ts":   `bus.emit('order.created', payload);`,
		"listen.ts": `bus.on('order.updated', handler);`,
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	sus := suspectKeys(scan)
	s, ok := sus["order.created"]
	if !ok {
		t.Fatalf("emitted event with no listener → suspect; got %v", keysOf(sus))
	}
	if s.Kind != SeamEvent {
		t.Errorf("kind=%s want event", s.Kind)
	}
	if _, ok := sus["order.updated"]; ok {
		t.Error("order.updated has a listener and no emit → not a (call) suspect")
	}
}

func TestExtract_CoverageCounts(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"client.ts": "this.post('/a', b);",
		"routes.ts": "app.post('/a', h);\napp.post('/b', h);",
	})
	scan, _ := ExtractSeamSuspects(dir, config.SeamPatterns{})
	if scan.FilesScanned < 2 || scan.CallsSeen < 1 || scan.RegsSeen < 2 {
		t.Errorf("counts not populated: files=%d calls=%d regs=%d",
			scan.FilesScanned, scan.CallsSeen, scan.RegsSeen)
	}
}

func mustScan(t *testing.T, dir string, p config.SeamPatterns) SeamScan {
	t.Helper()
	s, err := ExtractSeamSuspects(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExtract_ConfigCustomRouterReceiver(t *testing.T) {
	// A project whose router is named `web` would false-positive '/x' as
	// unregistered; adding `web` as a router receiver wires it.
	files := map[string]string{
		"client.ts": "this.post('/x', b);",
		"routes.ts": "web.post('/x', h);",
	}
	if _, ok := suspectKeys(mustScan(t, writeFixture(t, files), config.SeamPatterns{}))["/x"]; !ok {
		t.Fatal("precondition: without config, web.post is ambiguous → /x is a suspect")
	}
	p := config.SeamPatterns{RouterReceivers: []string{"web"}}
	if _, ok := suspectKeys(mustScan(t, writeFixture(t, files), p))["/x"]; ok {
		t.Error("with web as a router receiver, /x must be wired (no suspect)")
	}
}
