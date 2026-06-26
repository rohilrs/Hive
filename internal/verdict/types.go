// Package verdict carries the wire format for per-stage verdict capture.
// The forwarder side is invoked from a subprocess (e.g.,
// `hive mcp-stage-server`) when the worker calls a verdict tool. The
// listener side runs in the daemon-side adapter (binds the per-stage
// UDS socket before the worker subprocess is spawned, eliminating the
// race per spec §5.6).
package verdict

import "errors"

// FileRef anchors reviewer feedback to a specific file (and optionally
// a line). Required when Verdict=CHANGES_REQUESTED. The Comment field
// states what to change; the optional Reasoning field explains why so
// the next-iter implementer has context, not just a checklist.
type FileRef struct {
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
	Comment   string `json:"comment"`
	Reasoning string `json:"reasoning,omitempty"`
}

// Frame is the wire payload for one verdict submission.
//
// FileRefs is REQUIRED when Verdict=CHANGES_REQUESTED. The listener
// rejects frames violating this with ErrFileRefsMissing. Frames with
// Verdict=APPROVE may carry empty FileRefs.
//
// Summary is an optional one-paragraph holistic finding. It captures
// cross-cutting concerns that are not anchored to a specific file, so
// they survive even when FileRefs only covers per-file annotations.
type Frame struct {
	RunID      string    `json:"run_id"`
	Stage      string    `json:"stage"`
	Verdict    string    `json:"verdict"`
	Confidence int       `json:"confidence"`
	FileRefs   []FileRef `json:"file_refs,omitempty"`
	Summary    string    `json:"summary,omitempty"`
}

// AckErrFileRefsMissing is the canonical code field in Ack.Error when a
// CHANGES_REQUESTED frame is rejected for missing FileRefs. Adapter-side code
// matches on these to surface typed failures up the pipeline.
const (
	AckErrFileRefsMissing = "REVIEW_FEEDBACK_MISSING"
)

// ErrFileRefsMissing is the typed error returned by the adapter when
// the listener rejected a CHANGES_REQUESTED frame missing FileRefs.
var ErrFileRefsMissing = errors.New(AckErrFileRefsMissing)

type Ack struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
