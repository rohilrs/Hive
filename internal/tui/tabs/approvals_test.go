package tabs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeApprovalsSnap struct{ pending []PendingApproval }

func (f *fakeApprovalsSnap) PendingApprovals() []PendingApproval { return f.pending }

func TestApprovalsViewAndApprove(t *testing.T) {
	snap := &fakeApprovalsSnap{pending: []PendingApproval{
		{ApprovalID: "ap-1", RunID: "r1", Stage: "implement", ToolName: "Bash", Arg: "make all", Tier: "bash"},
	}}
	a := NewApprovals(snap)

	view := a.View()
	if !strings.Contains(view, "Bash") || !strings.Contains(view, "make all") {
		t.Errorf("view missing pending request: %q", view)
	}

	// 'a' approves the cursor item.
	_, cmd := a.Update(tea.KeyMsg{Runes: []rune{'a'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("a should emit a resolve cmd")
	}
	req, ok := cmd().(TabApprovalResolveRequest)
	if !ok {
		t.Fatalf("got %T want TabApprovalResolveRequest", cmd())
	}
	if req.ApprovalID != "ap-1" || req.Decision != "approve" || req.Remember {
		t.Errorf("unexpected resolve req: %+v", req)
	}
}

func TestApprovalsApproveAndRememberDerivesGlob(t *testing.T) {
	snap := &fakeApprovalsSnap{pending: []PendingApproval{
		{ApprovalID: "ap-1", ToolName: "Bash", Arg: "make all", Tier: "bash"},
	}}
	a := NewApprovals(snap)
	_, cmd := a.Update(tea.KeyMsg{Runes: []rune{'A'}, Type: tea.KeyRunes})
	req := cmd().(TabApprovalResolveRequest)
	if !req.Remember || req.ArgMatcher != "make *" {
		t.Errorf("approve+remember should derive 'make *'; got %+v", req)
	}
}

func TestApprovalsEmptyState(t *testing.T) {
	a := NewApprovals(&fakeApprovalsSnap{})
	if !strings.Contains(a.View(), "No pending approvals") {
		t.Errorf("empty state missing; got %q", a.View())
	}
}
