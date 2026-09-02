package provision

import "sync"

// IdentityLookups bounds how many vendor identity lookups run at once.
//
// FIVE call sites fan out one HTTPS call per seat: internal/engine's three
// credential resolvers — GitHub, GitLab, Jira — on the CONFIG APPLY path, and
// internal/github's and internal/jira's reconcile, on the operator-invoked
// `crewlet <vendor> provision`. Unbounded, a company of thirty seats with
// thirty tokens opened thirty simultaneous connections to one vendor at every
// boot and every apply, which is the shape a vendor's abuse detection is built
// to notice. engine/github.go's own comment asserted a bound ("bounded by the
// number of distinct credentials") that is not a bound on anything the host
// controls.
//
// EIGHT, and the arithmetic is the point rather than the number. GitHub's
// secondary-rate-limit guidance is explicitly to avoid concurrent requests
// against the same account, so the value wants to be small; but sequential is
// the other failure — thirty seats against a hung API is thirty full timeouts
// end to end, on the path that decides whether a revision applies. At eight,
// thirty seats is four waves: roughly 40 s worst case against a dead API where
// sequential would be 300 s, and a company of two hundred seats pays 25 waves
// rather than opening two hundred sockets.
//
// A CONSTANT rather than Tier A config: an operator has no way to know a
// better value than "few enough not to look like abuse", and a knob with no
// honest reason to differ is one more thing to keep in step.
const IdentityLookups = 8

// ResolveConcurrently runs fn for each index, at most [IdentityLookups] at once.
//
// One helper rather than the same semaphore written five times: every caller
// is the same loop over a different credential shape, and a bound applied in
// only some of them is the drift this exists to prevent. It lives HERE rather
// than in internal/engine, where it started, because engine imports the vendor
// packages — so a helper defined there is unreachable from exactly the two
// call sites that were left unbounded, and the constant would have had to be
// written down three times to reach them all.
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
