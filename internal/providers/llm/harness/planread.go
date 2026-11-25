package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
)

// A SIGN-IN'S STATE COMES FROM ITS PLAN READ, NOT FROM TRAFFIC. The harness's
// own read of its plan ([harnessprofile.PlanRead]) costs no model call, so the
// engine reads it on a schedule of its own and every reading, from a read or a
// run alike, decides the plan-wide window ([Plan.reckon]): a session or weekly
// window used up sits the sign-in out until it turns over, and a reading below
// the headroom line ([llm.LimitWarnAt]) ends a session or weekly sit-out opened
// before it. A read that fails decides nothing.

// ErrNoPlanRead is an entry whose plan its harness cannot read without a call:
// its profile has no plan read, or it signs in with a token or a key.
var ErrNoPlanRead = errors.New("harness: this entry's plan cannot be read without a model call")

// ErrPlanReadRanModel is a plan read that reported a model turn: its harness
// no longer reads the plan this way, and the reading is refused.
var ErrPlanReadRanModel = errors.New("harness: the plan read ran a model turn")

// planReadTimeout bounds one plan read: measured at 1.7-2.7 s on a cold start
// and 9 s in a busy home, so thirty seconds is headroom on a loaded host.
const planReadTimeout = 30 * time.Second

// ReadsPlan reports whether [Provider.ReadPlan] can read this entry's plan.
func (p *Provider) ReadsPlan() bool {
	return p.profile.PlanRead != nil && p.SignsInWithLogin()
}

// ReadPlan asks the harness where its plan's windows stand, with no model
// call, in the entry's login home, and keeps what it says on the sign-in's
// [Plan]. [ErrNoPlanRead] where it cannot; any other error is a read that
// failed, which changes nothing.
func (p *Provider) ReadPlan(ctx context.Context) ([]llm.LimitReading, error) {
	if !p.ReadsPlan() {
		return nil, ErrNoPlanRead
	}
	read := p.profile.PlanRead
	captures, err := p.ws.capturesDir()
	if err != nil {
		return nil, err
	}
	var res *rawResult
	_, err = p.inCredentialHome(ctx, syncRefresh, io.Discard,
		func(ctx context.Context, home string, env []string) error {
			r, runErr := run(ctx, invocation{
				binary: p.profile.Binary, args: read.Args,
				dir: home, env: env, timeout: planReadTimeout, grace: p.termGrace,
				outDir: captures, setting: p.stateSetting(), hooks: p.hooks, janitor: &p.janitor,
			})
			res = r
			return runErr
		}, read.Fence)
	if err != nil {
		return nil, err
	}
	at := p.now()
	readings, err := p.planReadings(res, at)
	if err != nil {
		return nil, err
	}
	p.Plan().Observe(readings, at)
	return readings, nil
}

// planReadings reads a plan read's result as of at.
func (p *Provider) planReadings(res *rawResult, at time.Time) ([]llm.LimitReading, error) {
	read := p.profile.PlanRead
	switch {
	case res.timedOut:
		return nil, fmt.Errorf("harness: the plan read did not answer within %s", planReadTimeout)
	case res.unfinished() != "":
		return nil, fmt.Errorf("harness: the plan read %s", res.unfinished())
	case res.exitCode != 0:
		said := firstLine(ansiEscape.ReplaceAllString(res.stderr, ""))
		return nil, fmt.Errorf("harness: the plan read exited %d: %s", res.exitCode,
			clipBytes(redact.Secrets(said), maxStatusDetail))
	}
	doc, ok := decodeObject(strings.TrimSpace(res.stdout))
	if !ok {
		return nil, errors.New("harness: the plan read printed no JSON object")
	}
	turns, found := lookup(doc, read.Turns)
	if n, number := numberOf(turns); !found || !number || n != 0 {
		return nil, fmt.Errorf("%w (%s is %v): it is not read again on this build", ErrPlanReadRanModel,
			read.String(), turns)
	}
	text, _ := lookup(doc, read.Text)
	screen, _ := text.(string)
	var readings []llm.LimitReading
	for line := range strings.SplitSeq(screen, "\n") {
		line = strings.TrimSpace(line)
		w, ok := read.PlanLineOf(line)
		if !ok {
			continue
		}
		used, ok := usedPercent(line, read.UsedSuffix)
		if !ok {
			continue
		}
		r := llm.LimitReading{Window: w.Window, Hours: w.Hours, Model: w.Model, State: llm.LimitOK,
			Used: &used, ResetsAt: resetAt(line, read.Reset, at, p.zone)}
		if used >= 1 {
			r.State = llm.LimitReached
		}
		readings = append(readings, r)
	}
	if len(readings) == 0 {
		return nil, errors.New("harness: the plan read named no window: the vendor did not answer it")
	}
	return readings, nil
}

// usedPercent is the whole percent before suffix in line, as a fraction.
func usedPercent(line, suffix string) (float64, bool) {
	end := strings.Index(line, suffix)
	if end < 0 {
		return 0, false
	}
	start := end
	for start > 0 && line[start-1] >= '0' && line[start-1] <= '9' {
		start--
	}
	n, err := strconv.Atoi(line[start:end])
	if err != nil {
		return 0, false
	}
	return float64(n) / 100, true
}

// reckon decides the plan-wide window from readings heard at `at`; under
// pl.mu. See the file's doc for the rule.
func (pl *Plan) reckon(readings []llm.LimitReading, at time.Time) {
	var spent *llm.LimitReading
	for i, r := range readings {
		if (r.Window != llm.LimitSession && r.Window != llm.LimitWeekly) || !r.ResetsAt.After(at) {
			continue
		}
		if r.State == llm.LimitReached || (r.Used != nil && *r.Used >= 1) {
			if spent == nil || r.ResetsAt.After(spent.ResetsAt) {
				spent = &readings[i]
			}
		}
	}
	if spent != nil {
		pl.wide.record(limitReport{reset: spent.ResetsAt, at: at, window: spent.Window,
			said: fmt.Sprintf("The plan's %s window is used up", spent.Window)})
		return
	}
	b := &pl.wide
	if !at.Before(b.until) || !b.since.Before(at) ||
		(b.window != llm.LimitSession && b.window != llm.LimitWeekly) {
		return
	}
	room := false
	for _, r := range readings {
		if r.Window != b.window {
			continue
		}
		if r.Used == nil || *r.Used >= llm.LimitWarnAt || r.State == llm.LimitReached {
			return
		}
		room = true
	}
	if room {
		*b = bench{}
	}
}

// Heard keeps limits another node published for this sign-in, each at the
// instant it was seen, as if read here: a vendor's word is the account's.
func (pl *Plan) Heard(limits []Limit) {
	for _, l := range limits {
		pl.Observe([]llm.LimitReading{l.LimitReading}, l.Seen)
	}
}

// ReadWanted is when a call on the sign-in last opened a window: a plan read
// after it says where the window really stands. Zero for never.
func (pl *Plan) ReadWanted() time.Time {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.readWanted
}

// LastRead is the newest reading of any window the sign-in holds, zero for none.
func (pl *Plan) LastRead() time.Time {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	var last time.Time
	for _, l := range pl.limits {
		if l.Seen.After(last) {
			last = l.Seen
		}
	}
	return last
}
