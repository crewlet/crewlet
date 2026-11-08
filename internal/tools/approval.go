package tools

import "context"

// Approval is a person's answer to a held call, as the call made on it reads
// it: who answered, and the held call's id.
type Approval struct {
	By     string
	HeldID string
}

type approvalKey struct{}

// WithApproval is ctx carrying the answer a held call is being made on.
func WithApproval(ctx context.Context, a Approval) context.Context {
	return context.WithValue(ctx, approvalKey{}, a)
}

// ApprovalOf is the answer the call in ctx is made on, false for a call no
// person approved.
func ApprovalOf(ctx context.Context) (Approval, bool) {
	a, ok := ctx.Value(approvalKey{}).(Approval)
	return a, ok && a.By != ""
}
