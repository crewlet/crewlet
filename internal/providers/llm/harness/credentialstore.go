package harness

import (
	"context"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// importLogin implements [loginStore]: the profile's import command, in home,
// with the document on stdin.
//
// A CREDENTIAL CHILD: it is handed every credential the login holds, and its
// stderr may quote them, so its output is captured in memory on linux and
// reaches no disk ([childSpec.credential]).
func (p *Provider) importLogin(ctx context.Context, home, cache string, document []byte) error {
	captures, err := p.ws.capturesDir()
	if err != nil {
		return err
	}
	_, err = p.runAuxiliary(ctx, auxiliary{args: p.profile.CredentialStore.ImportArgs,
		dir: home, outDir: captures, env: p.homeEnv(home, cache), stdin: string(document),
		credential: true})
	return err
}

// exportLogin implements [loginStore]: the profile's export command, in home,
// and what it printed — every credential the harness holds, so captured in
// memory on linux, as the import is.
func (p *Provider) exportLogin(ctx context.Context, home, cache string) ([]byte, error) {
	captures, err := p.ws.capturesDir()
	if err != nil {
		return nil, err
	}
	out, err := p.runAuxiliary(ctx, auxiliary{args: p.profile.CredentialStore.ExportArgs,
		dir: home, outDir: captures, env: p.homeEnv(home, cache), credential: true})
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
// THE HARNESS ITSELF READS THE HOME, as it would for the human: a login held in
// its own database has no file to copy, and a copy of the database would be
// every session they ever had. The environment is a call's except TMPDIR, which
// stays this process's own: a call's points inside the home, and a runtime
// unpacking native libraries into it would litter somebody's home directory.
//
// The login passes through stdout, which the doctor's host-login check reads
// and discards: captured in memory on linux, it reaches no disk
// ([childSpec.credential]).
func (p *Provider) exportHostLogin(ctx context.Context, home string) ([]byte, error) {
	env := childEnv(p.profile, &Checkout{Home: home, Cache: filepath.Join(home, ".cache")}, p.env, p.auth)
	env["TMPDIR"] = os.TempDir()
	captures, err := p.ws.capturesDir()
	if err != nil {
		return nil, err
	}
	out, err := p.runAuxiliary(ctx, auxiliary{args: p.profile.CredentialStore.ExportArgs,
		dir: home, outDir: captures, env: hostbox.Environ(env), credential: true})
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}
