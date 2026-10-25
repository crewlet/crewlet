package assistanttest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// Person is a person signed in through their own browser, holding the human
// seat handle and the apps given: the one principal the assistant serves.
func Person(name, seat string, apps ...iam.Grant) iam.Principal {
	return iam.Principal{ID: ID("person/" + name), Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: name, Seat: seat, Grants: apps,
		Via: iam.SessionName(ID("session/" + name).String())}
}

// Clock is a test's clock: fixed at [Epoch] until it is moved.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock is a clock at [Epoch].
func NewClock() *Clock { return &Clock{now: Epoch} }

// Now is the clock's instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock on.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// companyDoc is a company document with the assistant on, on the entry the
// assistant may run on (Claude Code in text mode, its web tools off), and one
// human seat for each handle the caller names.
const companyDoc = `name: Nimbus
providers:
  llm:
    assistant:
      agent: claude-code
      model: sonnet
      overrides: {web_tools: denied}
assistant:
  enabled: %t
  llm: [assistant]
roles:
%s`

// Company is the company a suite's people work for: the assistant on when on
// is set, and each of seats a human seat of that handle.
func Company(t testing.TB, on bool, seats ...string) assistant.Company {
	t.Helper()
	var roles string
	for _, seat := range seats {
		roles += fmt.Sprintf("  - name: %s\n    handle: %s\n    kind: human\n", seat, seat)
	}
	c, err := config.ParseCompany(fmt.Appendf(nil, companyDoc, on, roles))
	if err != nil {
		t.Fatalf("assistanttest: the suite's company does not parse: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatalf("assistanttest: the suite's org does not build: %v", err)
	}
	return func() (*config.Company, *org.Organization) { return c, o }
}
