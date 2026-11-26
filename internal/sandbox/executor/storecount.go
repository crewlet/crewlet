package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// storeCounted is a harness whose run's model requests are counted off its own
// session store, asked through the box, live ([Runner.Count]) and at its
// collection alike ([Runner.Collect]): a store the box's files cannot be read
// off a byte at a time.
type storeCounted interface {
	// storeRequests is the requests the store holds for the run; ok is false
	// while the run has not recorded where it began in the store.
	storeRequests(ctx context.Context, box sandbox.Sandbox, paths Paths) (requests int, ok bool, err error)
}

// storeCount is one live count of a job off its harness's store
// ([storeCounted]): each read asks the store again, and the count never goes
// down.
type storeCount struct {
	harness  storeCounted
	inFlight int
	most     sandbox.RequestCount
}

// Read implements [sandbox.Counter].
func (c *storeCount) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	n, ok, err := c.harness.storeRequests(ctx, box, PathsFor(box))
	if err != nil || !ok {
		return c.most, err
	}
	c.most = sandbox.RequestCount{Requests: max(c.most.Requests, n), InFlight: c.inFlight}
	return c.most, nil
}

// storeRequests implements [storeCounted]: the model responses Hermes's store
// holds for the run's session past where the run began ([Paths.HermesRunStart],
// hermes_carry.py's requests): its assistant rows but the notes Hermes writes as
// one, an interrupt's or a failed turn's. Measured on 0.23.3 against a stand-in,
// as many as the requests answered in every run, the summary a run its round cap
// stopped asks for included, and written within 0.35 s of each. ONE EXEC OF
// HERMES'S OWN PYTHON A READ, so a capped Hermes run costs its box one a tick.
// NOT COUNTED: a summary its answer came back empty for, asked again, which
// writes one row for two requests (Hermes's own rule, unmeasured).
func (Hermes) storeRequests(ctx context.Context, box sandbox.Sandbox, paths Paths) (int, bool, error) {
	raw, err := box.ReadFile(ctx, paths.HermesRunStart())
	if err != nil {
		return 0, false, fmt.Errorf("executor: reading where the run began in its Hermes session: %w", err)
	}
	session, from, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	after, atoiErr := strconv.ParseInt(from, 10, 64)
	if !ok || atoiErr != nil || after < 0 || !sandbox.ValidSessionID(session) {
		return 0, false, nil
	}
	q := shellword.Quote
	res, err := box.Exec(ctx, hermesCarryRun(paths)+fmt.Sprintf("hcarry requests %s %s %d",
		q(paths.HermesStateDB()), q(session), after), sandbox.ExecOptions{})
	if err != nil {
		return 0, false, fmt.Errorf("executor: counting the run's requests in its Hermes session: %w", err)
	}
	count, said := strings.CutPrefix(strings.TrimSpace(res.Stdout), "crewlet-carry ok ")
	n, err := strconv.Atoi(count)
	if !said || err != nil || n < 0 {
		return 0, false, fmt.Errorf("executor: counting the run's requests in its Hermes session answered %q: %s",
			firstLine(res.Stdout, transcriptDetailLimit), firstLine(res.Stderr, transcriptDetailLimit))
	}
	return n, true, nil
}
