package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SEAT'S SETUP FILE REACHES ITS BOX AS THE BODY SOMEBODY WROTE: nothing
// expands a body, so a `.npmrc`'s `${NPM_TOKEN}` and `${HOME}` are left for the
// box's npm and shell. Only a body that is exactly one `${VAR}` is read through
// this node's resolver, and one nothing answers for is not written at all,
// neither as its text nor empty (the run's gap reports it; the runenv tests).
// A provider-wide file follows the same rule, its step before the seat's own,
// and reading it never writes through to the manager's own steps.
func TestASeatsSetupFileReachesTheBoxAsWritten(t *testing.T) {
	t.Parallel()
	const (
		npmrc  = "registry=https://r.example.com\n//r.example.com/:_authToken=${NPM_TOKEN}\n"
		helper = "#!/bin/sh\n[ \"$1\" = get ] || exit 0\necho \"password=${GIT_TOKEN}\"\n"
	)
	e := newEngine(t, engine.Options{Company: parsedCompany(t, `
name: Acme
providers:
  sandbox:
    fake: true
roles:
  - name: Builder
    handle: builder
    sandbox:
      setup:
        - name: registry
          files:
            /root/.npmrc: |
              registry=https://r.example.com
              //r.example.com/:_authToken=${NPM_TOKEN}
            /usr/local/bin/h: |
              #!/bin/sh
              [ "$1" = get ] || exit 0
              echo "password=${GIT_TOKEN}"
            /root/.netrc: "${SEAT_FILE_UNSET}"
          env:
            NPM_TOKEN: "${NPM_TOKEN}"
`)})

	provider := []sandbox.SetupStep{{Name: "shared", Files: map[string]string{
		"/etc/plain":  "home is ${HOME}\n",
		"/etc/sealed": "${SHARED_FILE_UNSET}",
	}}}
	steps := engine.SeatBoxSetupForTest(e, provider, "builder")
	if len(steps) != 2 {
		t.Fatalf("the seat's box is given %+v, want the provider's step and then "+
			"the seat's own", steps)
	}
	if got := steps[1].Files["/root/.npmrc"]; got != npmrc {
		t.Errorf("the .npmrc reaches the box as %q, want %q", got, npmrc)
	}
	if got := steps[1].Files["/usr/local/bin/h"]; got != helper {
		t.Errorf("the helper script reaches the box as %q, want %q", got, helper)
	}
	if got, held := steps[1].Files["/root/.netrc"]; held {
		t.Errorf("a seat file naming a variable nothing answers for reaches the "+
			"box as %q, want it not written", got)
	}
	if got := steps[0].Files["/etc/plain"]; got != "home is ${HOME}\n" {
		t.Errorf("a provider-wide file was expanded to %q — a ${…} inside a "+
			"body is the box's, never the engine host's", got)
	}
	if got, held := steps[0].Files["/etc/sealed"]; held {
		t.Errorf("a file naming a variable nothing answers for reaches the "+
			"box as %q, want it not written", got)
	}
	if provider[0].Files["/etc/sealed"] != "${SHARED_FILE_UNSET}" {
		t.Errorf("reading the provider's step wrote through to the manager's "+
			"own copy: %v", provider[0].Files)
	}
}
