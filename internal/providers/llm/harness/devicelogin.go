package harness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	// ExpiresAt is when the code lapses, in UTC: the lifetime the harness
	// printed, else [deviceLoginTimingDefault]'s, counted from when the code
	// was read.
	ExpiresAt time.Time
}

// ErrDeviceCodeExpired is a device sign-in whose code lapsed before anybody
// approved it, so the harness was stopped and nothing was signed in.
var ErrDeviceCodeExpired = errors.New("harness: the device sign-in's code expired before it was approved")

// deviceTiming is how long a device sign-in waits for each of its steps.
type deviceTiming struct {
	// codeWait is how long the harness has to print its address and code.
	codeWait time.Duration
	// lifetime is a code's lifetime when the harness prints none.
	lifetime time.Duration
	// minute is the unit the harness prints a lifetime in.
	minute time.Duration
	// grace is how long past the code's lifetime the harness is left to finish:
	// an approval made in the code's last second still has to reach the harness
	// on its next poll and be written.
	grace time.Duration
	// after, where set, is the suite's clock for the sign-in's deadlines: a
	// channel that fires at the instant given. Nil is the wall clock's.
	after func(at time.Time) <-chan time.Time
}

// deviceLoginTimingDefault is every device sign-in's timing.
//
//   - codeWait, a minute: printing a code is one request to the vendor's
//     device-authorization endpoint, which answers in well under a second; a
//     minute covers a slow proxy and a cold runtime, and a harness silent by
//     then is not going to print, better said than shown as "starting".
//   - lifetime, fifteen minutes: OpenAI's documented device-code lifetime, for
//     a harness that does not print one (Codex does).
//   - grace, thirty seconds: the harness polls every few seconds (OAuth's
//     device grant defaults to five), so a last-moment approval reaches it
//     within a few polls; thirty seconds is that several times over.
var deviceLoginTimingDefault = deviceTiming{
	codeWait: time.Minute,
	lifetime: 15 * time.Minute,
	minute:   time.Minute,
	grace:    30 * time.Second,
}

// deviceTailLines is how many of the last lines a device sign-in printed are
// kept for the message that says why it failed.
const deviceTailLines = 5

// DeviceLogin signs p's harness in by its vendor's DEVICE-CODE flow: it runs the
// profile's device_login_args in the entry's login home, relays the address and
// the one-time code the harness prints to out, ONCE, and waits for approval.
//
// NOTHING IS PASTED BACK THROUGH CREWLET: the person approves at the vendor, the
// harness learns of it by polling and writes its own login file, and the engine
// only reads what it printed and carries that file into the credential
// directory: why this flow starts a sign-in from the dashboard.
//
// APPROVED is the harness exiting 0 having printed a code, with a login where
// the entry reads one, carried over under the shared login's lock with a new
// install record ([LoginInstall]); exit 0 with no login is refused.
func DeviceLogin(ctx context.Context, p *Provider, out func(DeviceCode)) error {
	return deviceLogin(ctx, p, out, deviceLoginTimingDefault)
}

// deviceLogin is [DeviceLogin] on a timing of the caller's, which the suite
// shortens to watch a code lapse.
//
// REFUSED UP FRONT for a profile with no device sign-in ([ErrNoLoginCommand])
// and for an entry whose calls sign in without a login (api-key, env, or
// inherit-env with the key in the engine's environment): such a login would be
// read by none of its calls.
//
// THE DEADLINE is the code's lifetime as printed, else fifteen minutes, plus
// thirty seconds ([deviceLoginTimingDefault]); a harness that prints no code
// within a minute is stopped. A lapsed code answers [ErrDeviceCodeExpired], a
// cancelled ctx the context's error.
func deviceLogin(ctx context.Context, p *Provider, out func(DeviceCode), timing deviceTiming) error {
	if !p.profile.HasDeviceLogin() {
		return fmt.Errorf("%w: the %q harness has no device sign-in, so it cannot be signed in "+
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

// deviceReading is what a device sign-in has read of its harness's output so far.
type deviceReading struct {
	rx       devicePatterns
	timing   deviceTiming
	url      string
	code     string
	lifetime time.Duration
	// sent is whether the code has been read to be relayed.
	sent bool
	// expires is when that code lapses: the lifetime from when it was read.
	expires time.Time
	// tail is the last lines printed, for a failure's message.
	tail []string
}

// read takes one line the harness printed, and answers the code to relay the
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
	d.expires = time.Now().Add(life)
	return DeviceCode{URL: d.url, Code: d.code, ExpiresAt: d.expires.UTC()}, true
}

// said is the last lines the harness printed, scrubbed, for a failure's message.
func (d *deviceReading) said() string {
	if len(d.tail) == 0 {
		return "it printed nothing"
	}
	return "it printed: " + redact.Secrets(strings.Join(d.tail, " / "))
}

// runDeviceLogin runs the device sign-in's harness in home and reads what it
// prints until it exits: see [DeviceLogin]. NO TERMINAL: ANSI colour is
// stripped ([ansiEscape]) before the profile's device_code_patterns read a
// line, and the harness runs in a process group of its own ([procgroup.Set]),
// so a stopped sign-in takes everything it started with it.
//
// ITS TWO STREAMS ARE ONE CAPTURE, in the workspace's captures directory, read
// to its end and kept nowhere ([childSpec.merged]): each line is handed to the
// reading as the reader reads it, the last one too, whether or not it ended in
// a line break. Its end is a call's: the harness exited, its group gone and
// nothing holding the capture, within the grace ([childRun.end]).
func (p *Provider) runDeviceLogin(
	ctx context.Context, home string, env []string, rx devicePatterns, timing deviceTiming, out func(DeviceCode),
) error {
	command := string(p.profile.Binary) + " " + strings.Join(p.profile.DeviceLoginArgs, " ")
	captures, err := p.ws.capturesDir()
	if err != nil {
		return err
	}
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	// THE READING IS THE READER'S, guarded for the one question the loop
	// below asks of it: whether the code is out, which moves the deadline.
	var mu sync.Mutex
	reading := &deviceReading{rx: rx, timing: timing}
	// relayed carries the instant the relayed code's approval is waited
	// until: its lapse, and the grace.
	relayed := make(chan time.Time, 1)
	read := func(line string) {
		mu.Lock()
		code, ok := reading.read(line)
		by := reading.expires.Add(timing.grace)
		mu.Unlock()
		if !ok {
			return
		}
		out(code)
		relayed <- by
	}
	ch, err := startChild(runCtx, childSpec{
		binary: p.profile.Binary, args: p.profile.DeviceLoginArgs, dir: home, env: env,
		outDir: captures, setting: p.stateSetting(), merged: true,
		// No stdin: nobody types into a device sign-in.
		out:   streamSpec{kind: readToEnd, onLine: read, last: lastLineHanded},
		grace: p.termGrace, hooks: p.hooks, janitor: &p.janitor,
	})
	if err != nil {
		return fmt.Errorf("harness: starting %s: %w", command, err)
	}

	after := timing.after
	if after == nil {
		after = func(at time.Time) <-chan time.Time { return time.After(time.Until(at)) }
	}
	deadline := after(time.Now().Add(timing.codeWait))
	cancelled := runCtx.Done()
	var waitErr error
	waited := false
	for !waited {
		select {
		case by := <-relayed:
			// The code is out: the harness now has its lifetime, and the
			// grace, to be approved.
			deadline = after(by)
		case <-deadline:
			mu.Lock()
			wasSent, by, said := reading.sent, reading.expires.Add(timing.grace), reading.said()
			mu.Unlock()
			switch {
			case !wasSent:
				deadline = nil
				stop(fmt.Errorf("harness: %s printed no device code within %s, so it was stopped; %s",
					command, timing.codeWait, said))
			case time.Now().Before(by):
				// READ AS THE WAIT FOR IT RAN OUT, and still being relayed
				// (in the engine, a write to the coordination store): the
				// code is fresh, and its own lifetime decides.
				deadline = after(by)
			default:
				deadline = nil
				stop(ErrDeviceCodeExpired)
			}
		case <-cancelled:
			cancelled = nil
		case waitErr = <-ch.exited:
			waited = true
		}
	}
	letGoBy := time.Now().Add(p.termGrace)
	if runCtx.Err() != nil {
		// Stopped: Cancel signalled the group and WaitDelay killed the harness
		// if it would not go; whatever it started that survived goes now, so
		// nothing holding the capture outlives the sign-in. After the harness
		// was reaped, so only while the group is still the one recorded
		// ([harnessGroup.signal]).
		ch.signal("device sign-in stopped", "SIGKILL", procgroup.Kill)
	}
	// What the harness printed before it exited is read to the end the end
	// measured, its last line included.
	ch.collect(ch.end(letGoBy))
	ch.close()
	mu.Lock()
	defer mu.Unlock()

	switch cause, cut := context.Cause(runCtx), ch.cut.Load(); {
	case ctx.Err() != nil:
		return fmt.Errorf("harness: the device sign-in was stopped before it was approved: %w", ctx.Err())
	case cut != nil:
		// THE ENGINE ENDED IT OVER ITS OUTPUT, so the kill its wait reports
		// is the engine's own, and the cut says why: a state_dir whose
		// volume refused the room ahead, or a reading that fell behind.
		return fmt.Errorf("harness: %s: %w; %s", command, cut, reading.said())
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
