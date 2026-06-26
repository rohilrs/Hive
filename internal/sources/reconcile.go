package sources

import "github.com/rohilrs/Hive/internal/store"

type OpKind int

const (
	OpInsert OpKind = iota
	OpUpdate
	OpClose
)

// Op is a single reconciliation action. Insert/Update carry Item; Update/Close
// carry the existing TaskID.
type Op struct {
	Kind   OpKind
	Item   SourceItem
	TaskID string
}

// Reconcile diffs fetched items against existing tasks for one (project,
// source), matched on source_id. Pure — no I/O. Rules:
//   - new open source_id            -> Insert (closed-new items are ignored)
//   - existing PENDING task:
//       item State=="closed" OR absent -> Close
//       title/body changed              -> Update
//   - any task with a run (status != "pending") -> untouched
func Reconcile(existing []store.Task, items []SourceItem) []Op {
	byID := make(map[string]store.Task, len(existing))
	for _, t := range existing {
		if t.SourceID == "" {
			continue
		}
		byID[t.SourceID] = t
	}
	seen := make(map[string]bool, len(items))
	var ops []Op

	for _, it := range items {
		seen[it.SourceID] = true
		cur, ok := byID[it.SourceID]
		if !ok {
			if it.State != "closed" {
				ops = append(ops, Op{Kind: OpInsert, Item: it})
			}
			continue
		}
		if cur.Status != "pending" {
			continue
		}
		if it.State == "closed" {
			ops = append(ops, Op{Kind: OpClose, TaskID: cur.ID})
			continue
		}
		if it.Title != cur.Title || it.Body != cur.Body {
			ops = append(ops, Op{Kind: OpUpdate, Item: it, TaskID: cur.ID})
		}
	}
	for _, t := range existing {
		if t.SourceID == "" {
			continue
		}
		if t.Status == "pending" && !seen[t.SourceID] {
			ops = append(ops, Op{Kind: OpClose, TaskID: t.ID})
		}
	}
	return ops
}
