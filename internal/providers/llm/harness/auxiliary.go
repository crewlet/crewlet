package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// auxiliaryTimeout bounds one AUXILIARY command — a short run of the harness that
// is not the call itself but brackets it: a [harnessprofile.CredentialStore]'s
// import before a seat's generation and export after it, and a
// [harnessprofile.Profile.UsageArgs] report after a call.
//
// Measured against OpenCode 2.0.32 on an idle host, each takes about half a
// second — most of it the harness starting the private server `--standalone`
// gives it. Thirty seconds is sixty times that, so a host already running
// max_concurrent harnesses does not fail a seat's generation or lose a
// call's real counts over a slow start, and a twentieth of the default
// per-call cap (timeout_seconds, 600), so a wedged command cannot quietly
// spend much of the call it brackets: every one of them runs inside the
// call's concurrency slot.
//
// A constant rather than a setting because nothing about a deployment moves
// it: the work is a fixed-size local round trip, and the per-call cap is the
// setting an operator already reaches for when a host is slow.
const auxiliaryTimeout = 30 * time.Second

// runAuxiliary runs one auxiliary command and returns its stdout.
//
// NOTHING IT WAS HANDED OR PRINTED IS EVER PART OF AN ERROR. A credential
// store's export prints every secret the harness holds, and its import is handed
// them on stdin, so a failure names the command, its exit and its stderr —
// and the stderr is SCRUBBED of what stdin and stdout carried first
// ([scrubAuxiliary]), because a harness that rejects a document may well quote the
// part it rejected, and the shape-based [redact.Secrets] recognises none of
// an OAuth access token, a refresh token or a Google key by sight.
//
// TYPED, because what failed decides what the caller does about it — see
// [acquireFailureKind]:
//
//   - the CALLER's context ending is checked FIRST and wrapped as itself. The
//     child's own bound is derived from that context, so a parent deadline
//     also reads as the child timing out, and checked second it reported
//     "did not finish within 30s" for an import the call's own deadline had
//     stopped in under a second;
//   - running out of [auxiliaryTimeout] is an [auxiliaryTimeoutError], which
//     is a [context.DeadlineExceeded] to errors.Is;
//   - exiting non-zero is an [auxiliaryExitError];
//   - exiting 0 without being seen to its end — a stdout something it left
//     running still held, a wait that failed — is an
//     [auxiliaryUnfinishedError], because what it printed may be a login
//     with its end missing;
//   - a child that could not be started at all is the error [run] gave.
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
	case err == nil && !res.timedOut && res.exitCode == 0 && res.unfinished() == "":
		// Ran to the end — whatever the caller's context did meanwhile,
		// since what it was waiting for is done.
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s was stopped by its caller before it finished: %w", command, ctx.Err())
	case err != nil:
		return "", err
	default:
		return "", auxiliaryFailure(command, res, stdin)
	}
	if res.droppedStdout > 0 {
		// A clipped document is a login with its end missing, or a report
		// with its counts cut off, and either read as whole is wrong.
		return "", fmt.Errorf("%s printed more than %d bytes, so what it printed is "+
			"incomplete", command, maxOutput)
	}
	return res.stdout, nil
}

// auxiliaryFailure is the error for a command that ran and did not succeed —
// timed out, exited non-zero, or exited 0 without being seen to its end — with
// its stderr scrubbed of what it was handed on stdin and what it printed
// ([scrubAuxiliary]).
func auxiliaryFailure(command string, res *rawResult, stdin string) error {
	stderr := scrubAuxiliary(res.stderr, stdin, res.stdout)
	switch {
	case res.timedOut:
		return &auxiliaryTimeoutError{command: command, stderr: stderr}
	case res.exitCode != 0:
		return &auxiliaryExitError{command: command, code: res.exitCode, stderr: stderr}
	default:
		return &auxiliaryUnfinishedError{command: command, why: res.unfinished(), stderr: stderr}
	}
}

// auxiliaryTimeoutError is an auxiliary command that ran past
// [auxiliaryTimeout] and was stopped.
type auxiliaryTimeoutError struct {
	command string
	// stderr is already scrubbed.
	stderr string
}

func (e *auxiliaryTimeoutError) Error() string {
	return fmt.Sprintf("%s did not finish within %s%s", e.command, auxiliaryTimeout, stderrDetail(e.stderr))
}

// Unwrap makes the timeout a [context.DeadlineExceeded], the one fact every
// classifier above this package already reads as a timeout ([llm.KindOf]).
func (e *auxiliaryTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// auxiliaryExitError is an auxiliary command that ran and exited non-zero —
// a harness that answered, and refused.
type auxiliaryExitError struct {
	command string
	code    int
	// stderr is already scrubbed.
	stderr string
}

func (e *auxiliaryExitError) Error() string {
	return fmt.Sprintf("%s exited %d%s", e.command, e.code, stderrDetail(e.stderr))
}

// auxiliaryUnfinishedError is an auxiliary command that exited 0 and still
// cannot be taken as done: something it left running held its stdout past the
// grace, so what it printed was cut there, or waiting for it failed.
//
// NOT an [auxiliaryExitError], which an import reads as the harness refusing the
// login ([loginRefusedError]): this harness refused nothing, and the next call
// imports the same login again. To [acquireFailureKind] it is the SERVER
// failure an exit is, a local harness that did not answer usably.
type auxiliaryUnfinishedError struct {
	command string
	// why is [rawResult.unfinished], which repeats nothing the command was
	// handed or printed.
	why string
	// stderr is already scrubbed.
	stderr string
}

func (e *auxiliaryUnfinishedError) Error() string {
	return fmt.Sprintf("%s exited 0, but %s%s", e.command, e.why, stderrDetail(e.stderr))
}

// scrubMarker replaces what [scrubAuxiliary] takes out, in [redact]'s marker
// form, so a second redaction pass recognises its work and a reader can tell
// a removal from the harness's own words.
const scrubMarker = redact.Marker + "credential]"

// minScrubbedValue is the shortest JSON string value [scrubAuxiliary] takes
// out: eight characters, below which a value in a login document is an id, a
// type or a flag — `oauth`, `key`, `cred_1` — rather than a credential, which
// no vendor issues that short. Taking those out too would leave a harness's own
// account of what it rejected unreadable for nothing.
const minScrubbedValue = 8

// minTokenRun is the shortest [tokenRun]: sixteen characters, below which a run
// is a word or a path segment rather than a key — and every credential a shipped
// harness keeps is far longer. It is also the shortest piece of one that
// [scrubAuxiliary] takes out where a harness quoted only part of it.
const minTokenRun = 16

// tokenRun is a run of the characters a credential is spelled in — letters,
// digits and the URL-safe and base64 punctuation a JWT, an OAuth token or an
// API key carries — at least [minTokenRun] long.
var tokenRun = regexp.MustCompile(fmt.Sprintf(`[A-Za-z0-9_\-.+/=~]{%d,}`, minTokenRun))

// scrubAuxiliary takes out of stderr everything the command was handed and
// printed — handed being its stdin and its stdout — and then runs
// [redact.Secrets] over what is left.
//
// THE MATERIAL THE ENGINE ALREADY KNOWS IS SECRET, rather than shapes it hopes
// to recognise: an import is handed every credential the shared login holds,
// and an export prints every one the database does, so what a harness quoting
// either could repeat is exactly what those two texts contain. From each:
//
//   - the whole of it, so a harness echoing a document entire leaves one marker;
//   - where it is JSON, every string value at least [minScrubbedValue] long,
//     at any depth, and the [tokenRun]s inside each one (a harness may quote a
//     token out of a longer value, `Bearer …`);
//   - where it is not — a truncated document, a timed-out export's partial
//     output — the [tokenRun]s in it.
//
// The whole texts and the values are taken out where stderr repeats them
// exactly, longest first, so a value never leaves part of a longer one behind.
// A token run is taken out wherever stderr shares [minTokenRun] or more
// consecutive characters with it ([maskShared]), not only where it repeats the
// run whole: a parser's error quotes a WINDOW of its input — the bytes either
// side of where it stopped — and a window cuts a credential wherever it falls,
// so matched whole, the forty characters a window kept of a seventy-character
// token went into the error intact. [redact.Secrets] stays as the second layer
// for anything this did not know.
func scrubAuxiliary(stderr string, handed ...string) string {
	if strings.TrimSpace(stderr) == "" {
		return stderr
	}
	var exact, runs []string
	for _, text := range handed {
		e, r := secretsIn(text)
		exact = append(exact, e...)
		runs = append(runs, r...)
	}
	slices.SortFunc(exact, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	exact = slices.Compact(exact)
	pairs := make([]string, 0, 2*len(exact))
	for _, s := range exact {
		pairs = append(pairs, s, scrubMarker)
	}
	if len(pairs) > 0 {
		stderr = strings.NewReplacer(pairs...).Replace(stderr)
	}
	return redact.Secrets(maskShared(stderr, runs))
}

// secretsIn is what [scrubAuxiliary] takes out of a text it was handed: what
// is taken out where stderr repeats it exactly, and the token runs taken out
// wherever stderr shares a piece of one.
func secretsIn(text string) (exact, runs []string) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < minScrubbedValue {
		return nil, nil
	}
	exact = []string{trimmed}
	var doc any
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return exact, tokenRun.FindAllString(trimmed, -1)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if len(v) >= minScrubbedValue {
				exact = append(exact, v)
				runs = append(runs, tokenRun.FindAllString(v, -1)...)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(doc)
	return exact, runs
}

// maskShared replaces every stretch of stderr that shares [minTokenRun] or more
// consecutive characters with one of runs by one [scrubMarker].
//
// The pieces are indexed from the RUNS' side — every minTokenRun-long window of
// every run — and stderr's own token runs are scanned against them, so the
// index is as large as the credential material handed in, the same order as
// the exact replacement's own, and a stretch of stderr no credential shares is
// read once and kept.
func maskShared(stderr string, runs []string) string {
	pieces := map[string]struct{}{}
	for _, run := range runs {
		for i := 0; i+minTokenRun <= len(run); i++ {
			pieces[run[i:i+minTokenRun]] = struct{}{}
		}
	}
	if len(pieces) == 0 {
		return stderr
	}
	var covered []bool
	for _, loc := range tokenRun.FindAllStringIndex(stderr, -1) {
		for i := loc[0]; i+minTokenRun <= loc[1]; i++ {
			if _, shared := pieces[stderr[i:i+minTokenRun]]; !shared {
				continue
			}
			if covered == nil {
				covered = make([]bool, len(stderr))
			}
			for k := i; k < i+minTokenRun; k++ {
				covered[k] = true
			}
		}
	}
	if covered == nil {
		return stderr
	}
	var b strings.Builder
	for i := 0; i < len(stderr); {
		if !covered[i] {
			b.WriteByte(stderr[i])
			i++
			continue
		}
		for i < len(stderr) && covered[i] {
			i++
		}
		b.WriteString(scrubMarker)
	}
	return b.String()
}
