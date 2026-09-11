package main

import "context"

type stubAgyExecutor struct {
	execute func(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (string, error)
}

func (s stubAgyExecutor) Execute(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (string, error) {
	if s.execute == nil {
		return "", nil
	}
	return s.execute(ctx, prompt, conversationID, opts)
}
