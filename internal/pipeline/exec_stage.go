package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// RunShellStage executes `bash -c <command>` in cwd under a context
// timeout, capturing combined stdout+stderr (truncated at maxBytes).
// Returns (output, success, err). success=true iff the command exited
// with code 0. err is non-nil only for setup failures (e.g., bash not
// found); a non-zero exit is NOT an error — it's a normal-mode failure
// surfaced via success=false.
//
// Timeout enforcement: when the context-derived timeout fires, the
// process is killed (SIGKILL via CommandContext). Output captured up
// to that point is returned with success=false.
//
// Truncation: when the output exceeds maxBytes, the LAST maxBytes
// bytes are returned with a leading "[truncated head; showing last N
// of M bytes]" marker. Test/validate runners print passing output and
// progress first and the failures + diffs last, so keeping the tail
// preserves the diagnostic detail the worker prompt actually needs.
func RunShellStage(ctx context.Context, command, cwd string, timeout time.Duration, maxBytes int) (string, bool, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = cwd

	out, runErr := cmd.CombinedOutput()
	output := string(out)
	if maxBytes > 0 && len(output) > maxBytes {
		output = fmt.Sprintf("[truncated head; showing last %d of %d bytes]\n", maxBytes, len(output)) +
			output[len(output)-maxBytes:]
	}

	if runErr == nil {
		return output, true, nil
	}
	if _, ok := runErr.(*exec.ExitError); ok {
		return output, false, nil
	}
	return output, false, fmt.Errorf("RunShellStage: %w", runErr)
}
