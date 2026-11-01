package harness

import (
	"context"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// importLogin implements [loginStore]: the profile's import command, in home,
// with the document on stdin.
func (p *Provider) importLogin(ctx context.Context, home, cache string, document []byte) error {
	_, err := p.runAuxiliary(ctx, p.profile.CredentialStore.ImportArgs,
		home, p.homeEnv(home, cache), string(document))
	return err
}

// exportLogin implements [loginStore]: the profile's export command, in home,
// and what it printed.
func (p *Provider) exportLogin(ctx context.Context, home, cache string) ([]byte, error) {
	out, err := p.runAuxiliary(ctx, p.profile.CredentialStore.ExportArgs,
		home, p.homeEnv(home, cache), "")
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// providerKey implements [loginStore]: the providers.llm key, for the log lines
// a sync writes.
func (p *Provider) providerKey() string { return p.key }

// homeEnv is the child environment for a command run in home, from
// [buildEnv], so a credential-store command reads and writes the database the
// call itself will: an import that wrote into a different XDG_DATA_HOME than
// the call reads from would report success and sign nobody in. It shares the
// home's variables and the profile's isolation switches with the call, and
// NOT the call's settings, which a call reads from its own working directory
// ([harnessprofile.Profile.WorkConfigEnv]) — a command in a home has none, so it
// runs with no settings file at all, and the import and export need none.
func (p *Provider) homeEnv(home, cache string) []string {
	return buildEnv(p.profile, &Checkout{Home: home, Cache: cache}, p.env, p.auth)
}

// exportHostLogin runs the profile's export command against a HUMAN'S home,
// for `crewlet llm login -from-host` and the doctor's "host login" line.
//
// THE CLI ITSELF READS THE HOME, as it would for the human: a login held in its
// own database has no file to copy, and a copy of the database would be every
// session they ever had. The environment is a call's with one exception —
// TMPDIR stays this process's own, because a call's points inside the home it
// is given, and a runtime that unpacks itself into TMPDIR would leave a
// directory of native libraries in somebody's home directory.
func (p *Provider) exportHostLogin(ctx context.Context, home string) ([]byte, error) {
	env := childEnv(p.profile, &Checkout{Home: home, Cache: filepath.Join(home, ".cache")}, p.env, p.auth)
	env["TMPDIR"] = os.TempDir()
	out, err := p.runAuxiliary(ctx, p.profile.CredentialStore.ExportArgs,
		home, hostbox.Environ(env), "")
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}
