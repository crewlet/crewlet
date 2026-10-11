package setupapi

// StateSubject is what a begun GitHub App creation signs, for a case that
// mints a state of its own rather than through the begin route.
var StateSubject = stateSubject

// StateDomain is the domain a GitHub App state is signed in, for a case that
// mints one of its own: a literal copy in a test would keep passing against a
// domain the flow no longer signs in.
const StateDomain = tokenDomain
