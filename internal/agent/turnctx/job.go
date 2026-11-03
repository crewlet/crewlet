package turnctx

import "context"

// jobKey carries the launch a bridged call is made from.
type jobKey struct{}

// WithJob is ctx naming the JOB a call is made from: the id of the launch
// whose bridge session serves the call (internal/api/mcpbridge sets it on
// every bridged call of a session, from the session's own job).
//
// A JOB AND NOT ONLY A RUN, because a run is launched again (a reviewer's send
// back, an answer's fresh run) under the same run id, and a write a call makes
// on the run's row must land on the job that made it or on nothing: a call
// that outlived its job would otherwise act on the next one ([JobOf]).
func WithJob(ctx context.Context, launch string) context.Context {
	return context.WithValue(ctx, jobKey{}, launch)
}

// JobOf is the job a call is made from, empty for a call no executor made.
func JobOf(ctx context.Context) string {
	launch, _ := ctx.Value(jobKey{}).(string)
	return launch
}
