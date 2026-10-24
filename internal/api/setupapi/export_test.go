package setupapi

import "github.com/crewlet/crewlet/internal/iam"

// MintState is the begin route's own state, for a case about the callback
// that has no reason to stand a company up to begin one.
func (f *AppFlow) MintState(handle string, by iam.Actor) (string, error) {
	return f.mintState(handle, by)
}
