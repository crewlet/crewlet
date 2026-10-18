package cliagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// auxiliaryTimeout bounds one AUXILIARY command — a short run of the CLI that
// is not the call itself but brackets it: a [cliprofile.CredentialStore]'s
// import before a seat's generation and export after it, and a
// [cliprofile.Profile.UsageArgs] report after a call.
//
// Measured against OpenCode 2.0.32 on an idle host, each takes about half a
// second — most of it the CLI starting the private server `--standalone`
// gives it. Thirty seconds is sixty times that, so a host already running
// max_concurrent coding CLIs does not fail a seat's generation or lose a
// call's real counts over a slow start, and a tenth of the default per-call
// cap (cli.timeout_seconds, 300), so a wedged command cannot quietly spend
// most of the call it brackets: every one of them runs inside the call's
// concurrency slot.
//
// A constant rather than a setting because nothing about a deployment moves
// it: the work is a fixed-size local round trip, and the per-call cap is the
// setting an operator already reaches for when a host is slow.
const auxiliaryTimeout = 30 * time.Second

// runAuxiliary runs one auxiliary command and returns its stdout.
//
// Its stdout is NEVER part of an error. A credential store's export prints
// every secret the CLI holds, and nothing an auxiliary command prints is worth
// that risk, so a failure names the command, its exit and its stderr —
// redacted all the same, because a CLI that rejects a document may well quote
// the part it rejected.
func (p *Provider) runAuxiliary(
	ctx context.Context, args []string, dir string, env []string, stdin string,
) (string, error) {
	command := p.profile.Binary + " " + strings.Join(args, " ")
	res, err := run(ctx, invocation{
		binary:  p.profile.Binary,
		args:    args,
		stdin:   stdin,
		dir:     dir,
		env:     env,
		timeout: auxiliaryTimeout,
		grace:   p.termGrace,
	})
	switch {
	case err != nil:
		return "", err
	case res.timedOut:
		return "", fmt.Errorf("%s did not finish within %s%s",
			command, auxiliaryTimeout, stderrDetail(redact.Secrets(res.stderr)))
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s: %w", command, ctx.Err())
	case res.exitCode != 0:
		return "", fmt.Errorf("%s exited %d%s",
			command, res.exitCode, stderrDetail(redact.Secrets(res.stderr)))
	case res.droppedStdout > 0:
		// A clipped document is a login with its end missing, or a report
		// with its counts cut off, and either read as whole is wrong.
		return "", fmt.Errorf("%s printed more than %d bytes, so what it printed is "+
			"incomplete", command, maxOutput)
	}
	return res.stdout, nil
}
