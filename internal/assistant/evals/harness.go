package evals

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

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

// Clock is a rig's clock: fixed at [Epoch] until it is moved.
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
// assistant may run on (Claude Code, its local tools denied by its profile and
// its web tools off), and one human seat for each handle the caller names.
const companyDoc = `name: Nimbus
providers:
  llm:
    assistant:
      harness: claude-code
      model: sonnet
      overrides: {web_tools: denied}
assistant:
  enabled: %t
  llm: [assistant]
roles:
%s`

// deepDoc is [companyDoc] with deep work on, on a Claude Code entry of its own.
const deepDoc = `name: Nimbus
providers:
  llm:
    assistant:
      harness: claude-code
      model: sonnet
      overrides: {web_tools: denied}
    deep:
      harness: claude-code
      model: sonnet
assistant:
  enabled: %t
  llm: [assistant]
  deep_work:
    llm: deep
    max_steps: 20
    max_tokens: 100000
roles:
%s`

// Company is the company a case's people work for: the assistant on when on
// is set, and each of seats a human seat of that handle.
func Company(on bool, seats ...string) (assistant.Company, error) {
	return company(companyDoc, on, seats)
}

// DeepCompany is [Company] with deep work configured: twenty Crewlet calls and
// a hundred thousand tokens a task.
func DeepCompany(seats ...string) (assistant.Company, error) {
	return company(deepDoc, true, seats)
}

func company(doc string, on bool, seats []string) (assistant.Company, error) {
	var roles string
	for _, seat := range seats {
		roles += fmt.Sprintf("  - name: %s\n    handle: %s\n    kind: human\n", seat, seat)
	}
	c, err := config.ParseCompany(fmt.Appendf(nil, doc, on, roles))
	if err != nil {
		return nil, fmt.Errorf("evals: the company does not parse: %w", err)
	}
	o, err := c.Organization()
	if err != nil {
		return nil, fmt.Errorf("evals: the org does not build: %w", err)
	}
	return func() (*config.Company, *org.Organization) { return c, o }, nil
}

// namespace is the UUIDv5 namespace fixture ids are derived in.
var namespace = uuid.MustParse("6f9b2c4e-5a1d-5e3f-9c8b-1a2b3c4d5e6f")

// ID is a stable id for a name, so a golden file or a case never moves
// because an id was minted at random.
func ID(name string) uuid.UUID { return uuid.NewSHA1(namespace, []byte(name)) }

// Epoch is the instant every clock here starts at.
var Epoch = time.Date(2026, 10, 24, 9, 41, 0, 0, time.UTC)

// Key is a stable operation key for one question, in the shape
// internal/api/opkey scopes one to a person.
func Key(name string) string { return "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12.k" + name }
