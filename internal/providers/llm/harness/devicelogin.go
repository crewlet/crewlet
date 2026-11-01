package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/redact"
)

// DeviceCode is what a device sign-in relays to the person approving it: the
// address to open, the one-time code to enter there, and when the code stops
// being accepted.
type DeviceCode struct {
	URL  string
	Code string
	// ExpiresAt is when the code lapses, in UTC: the lifetime the CLI
	// printed, else [deviceLoginTimingDefault]'s, counted from when the code
	// was read.
	ExpiresAt time.Time
}

// ErrDeviceCodeExpired is a device sign-in whose code lapsed before anybody
// approved it, so the CLI was stopped and nothing was signed in.
var ErrDeviceCodeExpired = errors.New("harness: the device sign-in's code expired before it was approved")

// deviceTiming is how long a device sign-in waits for each of its steps.
type deviceTiming struct {
	// codeWait is how long the CLI has to print its address and code.
	codeWait time.Duration
	// lifetime is a code's lifetime when the CLI prints none.
	lifetime time.Duration
	// minute is the unit the CLI prints a lifetime in.
	minute time.Duration
	// grace is how long past the code's lifetime the CLI is left to finish:
	// an approval made in the code's last second still has to reach the CLI
	// on its next poll and be written.
	grace time.Duration
}

// deviceLoginTimingDefault is every device sign-in's timing.
//
//   - codeWait, a minute: printing a code is the CLI starting and making ONE
//     request to the vendor's device-authorization endpoint, which answers in
//     well under a second; a minute covers a slow proxy and a cold runtime,
//     and a CLI that has printed nothing by then is not going to, which is
//     better said than shown as "starting" for the code's whole lifetime.
//   - lifetime, fifteen minutes: OpenAI's documented device-code lifetime,
//     for a CLI that does not print one (Codex does, "expires in 15 minutes").
//   - grace, thirty seconds: a device sign-in polls the vendor every few
//     seconds (OAuth's device grant defaults the interval to five), so an
//     approval made at the last moment reaches the CLI within a few polls,
//     and thirty seconds is that several times over.
var deviceLoginTimingDefault = deviceTiming{
	codeWait: time.Minute,
	lifetime: 15 * time.Minute,
	minute:   time.Minute,
	grace:    30 * time.Second,
}

// deviceTailLines is how many of the last lines a device sign-in printed are
// kept for the message that says why it failed.
const deviceTailLines = 5

// DeviceLogin signs p's CLI in by its vendor's DEVICE-CODE flow: it runs the
// profile's device_login_args in the entry's login home, relays the address
// and the one-time code the CLI prints to out, ONCE, and waits for the person
// who opens the address to approve the code.
//
// NOTHING IS PASTED BACK THROUGH CREWLET. The person approves at the vendor,
// the CLI learns of it by polling the vendor, and it writes its own login file;
// the engine only reads what the CLI printed and carries the file it wrote into
// the credential directory. That is the whole reason this flow, and no
// browser or terminal relay, is how a sign-in starts from the dashboard.
//
// NO TERMINAL: stdout and stderr are ONE pipe the engine reads, in the order
// the CLI wrote them, with ANSI colour stripped ([ansiEscape]) before the
// profile's device_code_patterns read the address, the code and the lifetime
// out of each line. So the CLI runs in a process group of its own
// ([procgroup.Set]), unlike the interactive login, which needs the operator's
// terminal as its foreground group, and a sign-in stopped by its caller, or by
// its code lapsing, takes everything the CLI started with it.
//
// THE DEADLINE is the code's lifetime as the CLI printed it, else fifteen
// minutes, plus thirty seconds ([deviceLoginTimingDefault]); a CLI that prints
// no code within a minute is stopped as one that will not. A lapsed code
// answers [ErrDeviceCodeExpired], and a cancelled ctx the context's error.
//
// APPROVED is the CLI exiting 0 having printed a code, with a login where the
// entry reads one: the login it wrote is carried into the credential directory
// under the shared login's lock, as every login command's is, with a new
// install record ([LoginInstall]). A CLI that exited 0 and left no login is
// refused as a login that left nothing ([Provider.BrokerLogin] does the same).
//
// REFUSED UP FRONT for a profile with no device sign-in ([ErrNoLoginCommand]),
// and for an entry whose calls sign the CLI in without a login (api-key, env,
// or inherit-env with the key in the engine's environment): a login written
// for such an entry would be read by none of its calls.
func DeviceLogin(ctx context.Context, p *Provider, out func(DeviceCode)) error {
	return deviceLogin(ctx, p, out, deviceLoginTimingDefault)
}

// deviceLogin is [DeviceLogin] on a timing of the caller's, which the suite
// shortens to watch a code lapse.
func deviceLogin(ctx context.Context, p *Provider, out func(DeviceCode), timing deviceTiming) error {
	if !p.profile.HasDeviceLogin() {
		return fmt.Errorf("%w: the %q CLI has no device sign-in, so it cannot be signed in "+
			"without a terminal on the engine host: %s", ErrNoLoginCommand, p.harness, p.loginRemedy(nil))
	}
	if p.signsInWithKey() {
		sign, _ := p.ModeSignIn()
		return fmt.Errorf("harness: %q is not signed in by a login: its auth.mode is %s, which %s, "+
			"so a device sign-in would store a login none of its calls reads; set auth.mode: "+
			"subscription to sign it in with one", p.key, sign.Mode, sign.Reads)
	}
	if out == nil {
		return errors.New("harness: a device sign-in was started with nowhere to relay its code")
	}
	rx, err := p.devicePatterns()
	if err != nil {
		return err
	}
	holds, err := p.inCredentialHome(ctx, syncLogin, nil,
		func(ctx context.Context, home string, env []string) error {
			return p.runDeviceLogin(ctx, home, env, rx, timing, out)
		})
	if err != nil {
		return err
	}
	if !holds {
		return p.loginLeftNothing()
	}
	return nil
}

// devicePatterns are the profile's device_code_patterns, compiled.
type devicePatterns struct {
	url, code, lifetime *regexp.Regexp
}

// devicePatterns compiles the profile's device_code_patterns, which the
// profile's validation has already held to a first capture group each.
func (p *Provider) devicePatterns() (devicePatterns, error) {
	d := p.profile.DeviceCodePatterns
	if d == nil || d.URL == "" || d.Code == "" {
		return devicePatterns{}, fmt.Errorf("harness: the %q profile runs a device sign-in but "+
			"names no device_code_patterns.url and .code to read its address and code with; "+
			"declare them under providers.llm.%s.overrides", p.harness, p.key)
	}
	var out devicePatterns
	for _, pattern := range []struct {
		field, expr string
		into        **regexp.Regexp
	}{{"url", d.URL, &out.url}, {"code", d.Code, &out.code}, {"lifetime", d.Lifetime, &out.lifetime}} {
		if pattern.expr == "" {
			continue
		}
		re, err := regexp.Compile(pattern.expr)
		if err != nil || re.NumSubexp() < 1 {
			return devicePatterns{}, fmt.Errorf("harness: device_code_patterns.%s %q is not a "+
				"regular expression with a capture group: %v", pattern.field, pattern.expr, err)
		}
		*pattern.into = re
	}
	return out, nil
}

// deviceReading is what a device sign-in has read of its CLI's output so far.
type deviceReading struct {
	rx       devicePatterns
	timing   deviceTiming
	url      string
	code     string
	lifetime time.Duration
	// sent is whether the code has been relayed.
	sent bool
	// tail is the last lines printed, for a failure's message.
	tail []string
}

// read takes one line the CLI printed, and answers the code to relay the
// first time both the address and the code have been read.
func (d *deviceReading) read(line string) (DeviceCode, bool) {
	// A redraw is a carriage return inside a line: each frame is read.
	for frame := range strings.SplitSeq(ansiEscape.ReplaceAllString(line, ""), "\r") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		d.tail = append(d.tail, frame)
		if len(d.tail) > deviceTailLines {
			d.tail = d.tail[len(d.tail)-deviceTailLines:]
		}
		if d.url == "" {
			if m := d.rx.url.FindStringSubmatch(frame); m != nil {
				d.url = m[1]
			}
		}
		if d.code == "" {
			if m := d.rx.code.FindStringSubmatch(frame); m != nil {
				d.code = m[1]
			}
		}
		if d.lifetime == 0 && d.rx.lifetime != nil {
			if m := d.rx.lifetime.FindStringSubmatch(frame); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
					d.lifetime = time.Duration(n) * d.timing.minute
				}
			}
		}
	}
	if d.sent || d.url == "" || d.code == "" {
		return DeviceCode{}, false
	}
	d.sent = true
	life := d.lifetime
	if life == 0 {
		life = d.timing.lifetime
	}
	return DeviceCode{URL: d.url, Code: d.code, ExpiresAt: time.Now().Add(life).UTC()}, true
}

// said is the last lines the CLI printed, scrubbed, for a failure's message.
func (d *deviceReading) said() string {
	if len(d.tail) == 0 {
		return "it printed nothing"
	}
	return "it printed: " + redact.Secrets(strings.Join(d.tail, " / "))
}

// runDeviceLogin runs the device sign-in's CLI in home and reads what it
// prints until it exits: see [DeviceLogin].
func (p *Provider) runDeviceLogin(
	ctx context.Context, home string, env []string, rx devicePatterns, timing deviceTiming, out func(DeviceCode),
) error {
	command := p.profile.Binary + " " + strings.Join(p.profile.DeviceLoginArgs, " ")
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("harness: starting %s: %w", command, err)
	}
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	cmd := exec.CommandContext(runCtx, p.profile.Binary, p.profile.DeviceLoginArgs...) //nolint:gosec // args come from a validated profile
	cmd.Dir = home
	cmd.Env = env
	// ONE PIPE FOR BOTH STREAMS, the same file for each, so the CLI's
	// lines arrive in the order it wrote them whichever stream it chose for
	// the code; no stdin, since nobody types into a device sign-in.
	cmd.Stdout, cmd.Stderr = w, w
	procgroup.Set(cmd)
	cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = p.termGrace
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("harness: starting %s: %w", command, err)
	}
	// The child holds its own copy; this one would keep the pipe open past
	// the child, and the read below would never end.
	_ = w.Close()
	pgid := cmd.Process.Pid

	lines := make(chan string)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(r)
		scan.Buffer(make([]byte, 0, 4096), maxOutput)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	reading := &deviceReading{rx: rx, timing: timing}
	read := func(line string) {
		if code, ok := reading.read(line); ok {
			out(code)
		}
	}
	deadline := time.NewTimer(timing.codeWait)
	defer deadline.Stop()
	cancelled := runCtx.Done()
	var waitErr error
	for running := true; running; {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			sent := reading.sent
			read(line)
			if !sent && reading.sent {
				// The code is out: the CLI now has its lifetime, and the
				// grace, to be approved.
				life := reading.lifetime
				if life == 0 {
					life = timing.lifetime
				}
				deadline.Reset(life + timing.grace)
			}
		case <-deadline.C:
			if reading.sent {
				stop(ErrDeviceCodeExpired)
			} else {
				stop(fmt.Errorf("harness: %s printed no device code within %s, so it was stopped; %s",
					command, timing.codeWait, reading.said()))
			}
		case <-cancelled:
			cancelled = nil
		case waitErr = <-exited:
			running = false
		}
	}
	if runCtx.Err() != nil {
		// Stopped: Cancel signalled the group and WaitDelay killed the CLI
		// if it would not go; whatever it started that survived goes now, so
		// nothing holding the pipe outlives the sign-in.
		if err := procgroup.Kill(pgid); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			log.WarnContext(ctx, "cli_agent_group_kill_failed", "pgid", pgid,
				"reason", "device sign-in stopped", "error", err)
		}
	}
	// What the CLI printed before it exited is still in the pipe: read to its
	// end, bounded by the grace in case something it left behind holds it.
	drain := time.NewTimer(p.termGrace)
	defer drain.Stop()
	for lines != nil {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			read(line)
		case <-drain.C:
			_ = r.Close()
		}
	}
	_ = r.Close()

	switch cause := context.Cause(runCtx); {
	case ctx.Err() != nil:
		return fmt.Errorf("harness: the device sign-in was stopped before it was approved: %w", ctx.Err())
	case errors.Is(cause, ErrDeviceCodeExpired):
		return fmt.Errorf("%w (%s, at %s); start the sign-in again for a new code",
			ErrDeviceCodeExpired, command, reading.code)
	case runCtx.Err() != nil:
		return cause
	case waitErr != nil && !reading.sent:
		return fmt.Errorf("harness: %s exited without printing a device code (%w); %s",
			command, waitErr, reading.said())
	case waitErr != nil:
		return fmt.Errorf("harness: %s did not finish the sign-in (%w); %s", command, waitErr, reading.said())
	case !reading.sent:
		// Exit 0 with no code relayed: whatever it did, nobody was shown a
		// code to approve, so it is not this flow's approval.
		return fmt.Errorf("harness: %s exited without printing a device code that "+
			"device_code_patterns read; %s", command, reading.said())
	}
	return nil
}
