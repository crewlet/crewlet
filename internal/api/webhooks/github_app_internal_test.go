package webhooks

import "github.com/crewlet/crewlet/internal/api/setupapi"

// THE SETUP SURFACE'S FAILURE IS WHAT THE GITHUB APP PAGE READS, held where
// the compiler sees both. The page finds it by errors.As through
// [toldFailure], an interface of its own, so a method one side renamed, or one
// the interface grew and the setup surface never did, matches nothing at run
// time: every stop is then told this page's own "ask an admin", headed "App
// not created", and nothing fails. Here, in a test, because this package does
// not import the setup surface ([AppCompleter]).
var _ toldFailure = (*setupapi.AppFailure)(nil)
