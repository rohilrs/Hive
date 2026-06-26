package sources

import (
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

func task(id, srcID, status, title, body string) store.Task {
	return store.Task{ID: id, SourceID: srcID, Status: status, Title: title, Body: body}
}
func item(srcID, title, body, state string) SourceItem {
	return SourceItem{SourceID: srcID, Title: title, Body: body, State: state}
}

func TestReconcileInsertsNewOpenItem(t *testing.T) {
	ops := Reconcile(nil, []SourceItem{item("1", "A", "b", "open")})
	if len(ops) != 1 || ops[0].Kind != OpInsert || ops[0].Item.SourceID != "1" {
		t.Fatalf("want one insert for src 1, got %+v", ops)
	}
}
func TestReconcileSkipsInsertForClosedNewItem(t *testing.T) {
	ops := Reconcile(nil, []SourceItem{item("1", "A", "b", "closed")})
	if len(ops) != 0 {
		t.Fatalf("closed new item should not insert, got %+v", ops)
	}
}
func TestReconcileUpdatesChangedPending(t *testing.T) {
	ex := []store.Task{task("t1", "1", "pending", "old", "oldbody")}
	ops := Reconcile(ex, []SourceItem{item("1", "new", "newbody", "open")})
	if len(ops) != 1 || ops[0].Kind != OpUpdate || ops[0].TaskID != "t1" {
		t.Fatalf("want update t1, got %+v", ops)
	}
}
func TestReconcileNoOpUnchangedPending(t *testing.T) {
	ex := []store.Task{task("t1", "1", "pending", "A", "b")}
	ops := Reconcile(ex, []SourceItem{item("1", "A", "b", "open")})
	if len(ops) != 0 {
		t.Fatalf("unchanged should be no-op, got %+v", ops)
	}
}
func TestReconcileClosesPendingOnStateClosed(t *testing.T) {
	ex := []store.Task{task("t1", "1", "pending", "A", "b")}
	ops := Reconcile(ex, []SourceItem{item("1", "A", "b", "closed")})
	if len(ops) != 1 || ops[0].Kind != OpClose || ops[0].TaskID != "t1" {
		t.Fatalf("want close t1, got %+v", ops)
	}
}
func TestReconcileClosesPendingOnAbsent(t *testing.T) {
	ex := []store.Task{task("t1", "1", "pending", "A", "b")}
	ops := Reconcile(ex, nil)
	if len(ops) != 1 || ops[0].Kind != OpClose || ops[0].TaskID != "t1" {
		t.Fatalf("want close t1 on absent, got %+v", ops)
	}
}
func TestReconcileLeavesStartedTaskUntouched(t *testing.T) {
	for _, st := range []string{"running", "done", "needs_attention", "abandoned"} {
		ex := []store.Task{task("t1", "1", st, "old", "old")}
		if ops := Reconcile(ex, []SourceItem{item("1", "new", "new", "closed")}); len(ops) != 0 {
			t.Errorf("status %s should be untouched on change, got %+v", st, ops)
		}
		if ops := Reconcile(ex, nil); len(ops) != 0 {
			t.Errorf("status %s should be untouched on absent, got %+v", st, ops)
		}
	}
}
func TestReconcileIgnoresAlreadyClosedTask(t *testing.T) {
	ex := []store.Task{task("t1", "1", "source_closed", "A", "b")}
	if ops := Reconcile(ex, nil); len(ops) != 0 {
		t.Fatalf("source_closed task absent should be no-op, got %+v", ops)
	}
}
func TestReconcileIgnoresEmptySourceIDTask(t *testing.T) {
	// A manually-added task (no source_id) must never be closed/touched,
	// even when no fetched item matches it.
	ex := []store.Task{task("t1", "", "pending", "manual", "body")}
	if ops := Reconcile(ex, nil); len(ops) != 0 {
		t.Fatalf("empty-SourceID task must be untouched, got %+v", ops)
	}
	if ops := Reconcile(ex, []SourceItem{item("1", "A", "b", "open")}); len(ops) != 1 || ops[0].Kind != OpInsert {
		t.Fatalf("want only the insert for item 1, empty-SourceID task untouched, got %+v", ops)
	}
}
