package provision

import "sync"

// IdentityLookups bounds how many identity lookups run at once against one
// third-party app.
//
// Five call sites fan out one HTTPS call per seat (config apply's resolvers,
// github's and jira's reconcile). Unbounded, thirty seats opened thirty
// simultaneous connections to one app, the shape its abuse detection notices.
//
// EIGHT: GitHub's rate-limit guidance is to avoid concurrent requests against
// one account, but sequential is the other failure: thirty seats against a
// hung API is thirty full timeouts on the path deciding whether a revision
// applies. At eight that is four waves, about 40 s against a dead API where
// sequential would be 300 s. A CONSTANT: no operator can know a better value.
const IdentityLookups = 8

// ResolveConcurrently runs fn for each index, at most [IdentityLookups] at once.
//
// One helper rather than the same semaphore written five times: every caller
// is the same loop over a different credential shape, and a bound applied in
// only some of them is the drift this exists to prevent. It lives HERE rather
// than in internal/engine, where it started, because engine imports the
// integration packages, so a helper defined there is unreachable from exactly
// the two call sites that were left unbounded, and the constant would have
// had to be written down three times to reach them all.
func ResolveConcurrently(n int, fn func(i int)) {
	if n <= 0 {
		return
	}
	slots := make(chan struct{}, IdentityLookups)
	var wg sync.WaitGroup
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			fn(i)
		})
	}
	wg.Wait()
}
