package host_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY TRANSFER CARRIES ITS CREDENTIAL'S SHARE (gitproc.Cmd.Share): a
// person's push, clone and fetch — the advertisement each begins with, the
// receive-pack and the upload-pack — are charged to the PERSON, by the handle
// of the human seat their credential acts as (codetest's persons are each a
// Tier A token the directory binds to a human seat nobody else holds, named
// after it: `token:ceo` acts as the seat `ceo`), never by the credential's
// name; a credential bound to nobody's clone and fetch to the credential's
// NAME, `token:ci`, as a machine. What the transfer lane grants a contended
// slot by is only as true as what the host tells it.
//
// Mutations: share() naming the credential for a bound one too, or the
// machine class for every credential — red; any of the four transfer sites
// handing the pool no Share — the pool refuses the run, and the person's git
// fails.
func TestEveryTransferCarriesItsCredentialsShare(t *testing.T) {
	t.Parallel()
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	ran := host.RecordGit(c.Fleet.Nodes()[0].Host())

	ceo := codetest.NewPerson(t, c.Fleet, "ceo")
	work := ceo.Init()
	ceo.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(ceo, work, "main")
	clone := ceo.Clone(c.URL(0))
	ceo.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(ceo, work, "main")
	ceo.Git().Run(t, clone, "fetch", "--quiet", "origin")
	persons := ran()

	ci := codetest.NewPerson(t, c.Fleet, "")
	machine := ci.Clone(c.URL(0))
	ceo.Commit(work, map[string]string{"README.md": "three\n"}, "three")
	c.MustPush(ceo, work, "main")
	from := len(ran())
	ci.Git().Run(t, machine, "fetch", "--quiet", "origin")
	machines := ran()[len(persons):]
	fetched := ran()[from:]

	person := gitproc.Share{Class: gitproc.SharePerson, Principal: "ceo"}
	unbound := gitproc.Share{Class: gitproc.ShareMachine, Principal: iam.TokenLogin("ci")}
	for _, step := range []struct {
		name  string
		cmds  []gitproc.Cmd
		match func(gitproc.Cmd) bool
		want  gitproc.Share
	}{
		{"a person's push advertisement", persons, advertising("receive-pack"), person},
		{"a person's receive-pack", persons, transferring("receive-pack"), person},
		{"a person's fetch advertisement", persons, advertising("upload-pack"), person},
		{"a person's upload-pack", persons, transferring("upload-pack"), person},
		{"an unbound token's fetch advertisement", fetched, advertising("upload-pack"), unbound},
		{"an unbound token's upload-pack", fetched, transferring("upload-pack"), unbound},
	} {
		i := slices.IndexFunc(step.cmds, step.match)
		if i < 0 {
			t.Errorf("%s ran no such git: %v", step.name, argsOf(step.cmds))
			continue
		}
		if got := step.cmds[i].Share; got != step.want {
			t.Errorf("%s is charged to %+v, want %+v", step.name, got, step.want)
		}
	}
	// EVERY TRANSFER OF EACH CREDENTIAL, not just the first of each kind: the
	// machine's steps hold a person's push between them, so each transfer is
	// judged by the credential whose request ran it.
	for _, cmd := range persons {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != person {
			t.Errorf("the person's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, person)
		}
	}
	for _, cmd := range fetched {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != unbound {
			t.Errorf("the unbound token's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, unbound)
		}
	}
	for _, cmd := range append(persons, machines...) {
		if cmd.Class != gitproc.ClassTransfer && cmd.Share != (gitproc.Share{}) {
			t.Errorf("git %v in the %s lane carries a share, %+v", cmd.Args, cmd.Class, cmd.Share)
		}
	}
}

// advertising matches the advertisement git sub runs for a smart HTTP
// client's first request.
func advertising(sub string) func(gitproc.Cmd) bool {
	return func(c gitproc.Cmd) bool {
		return c.Class == gitproc.ClassTransfer && len(c.Args) > 0 && c.Args[0] == sub &&
			slices.Contains(c.Args, "--advertise-refs")
	}
}

// transferring matches git sub serving a transfer itself, past its
// advertisement.
func transferring(sub string) func(gitproc.Cmd) bool {
	return func(c gitproc.Cmd) bool {
		return c.Class == gitproc.ClassTransfer && len(c.Args) > 0 && c.Args[0] == sub &&
			!slices.Contains(c.Args, "--advertise-refs")
	}
}
