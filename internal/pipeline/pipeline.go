package pipeline

import "context"

// Pipeline is the contract every pipeline state machine implements.
type Pipeline interface {
	Name() string
	Run(ctx context.Context, run *Run) (*Result, error)
}
