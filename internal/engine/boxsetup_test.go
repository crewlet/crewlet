package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SEAT'S SETUP FILE REACHES ITS BOX AS THE BODY SOMEBODY WROTE.
//
// A file is CONTENT: nothing on the way to a box expands a body, so a seat's
// `.npmrc` reaches the box with its own `${NPM_TOKEN}` and `${HOME}` left for
// the box's npm and shell. A body that is exactly one `${VAR}` is the one shape
// read through this node's resolver, and one nothing answers for reaches the
// box present and empty rather than as the reference's own text. A
// provider-wide file is read by the same rule, after the provider's steps and
// before the seat's own, and the manager's own steps are never written
// through.
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
      enabled: true
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
	if got, held := steps[1].Files["/root/.netrc"]; !held || got != "" {
		t.Errorf("a seat file naming a variable nothing answers for reaches the "+
			"box as (%q, %v), want present and empty", got, held)
	}
	if got := steps[0].Files["/etc/plain"]; got != "home is ${HOME}\n" {
		t.Errorf("a provider-wide file was expanded to %q — a ${…} inside a "+
			"body is the box's, never the engine host's", got)
	}
	if got, held := steps[0].Files["/etc/sealed"]; !held || got != "" {
		t.Errorf("a file naming a variable nothing answers for reaches the "+
			"box as (%q, %v), want present and empty", got, held)
	}
	if provider[0].Files["/etc/sealed"] != "${SHARED_FILE_UNSET}" {
		t.Errorf("reading the provider's step wrote through to the manager's "+
			"own copy: %v", provider[0].Files)
	}
}
