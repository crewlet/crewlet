package engine

import "sync"

// identityLookups bounds how many vendor identity lookups run at once.
//
// The three resolvers below — GitHub, GitLab, Jira — each fan out one HTTPS
// call per distinct seat credential, and they do it on the CONFIG APPLY path.
// Unbounded, a company of thirty seats with thirty tokens opened thirty
// simultaneous connections to one vendor at every boot and every apply, which
// is the shape a vendor's abuse detection is built to notice. github.go's own
// comment asserted a bound ("bounded by the number of distinct credentials")
// that is not a bound on anything the host controls.
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
const identityLookups = 8

// resolveConcurrently runs fn for each index, at most identityLookups at once.
//
// One helper rather than the same semaphore written three times: the three
// resolvers are the same loop over different credential shapes, and a bound
// that is only applied in two of them is the drift this exists to prevent.
func resolveConcurrently(n int, fn func(i int)) {
	if n <= 0 {
		return
	}
	slots := make(chan struct{}, identityLookups)
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
