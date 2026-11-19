package evals

import (
	"fmt"
	"strings"
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

// companyDoc is a company document with Ask Crewlet on, on an ordinary Claude
// Code entry (a helper's run turns its web tools off whatever the entry says),
// and one human seat for each handle the caller names.
const companyDoc = `name: Nimbus
providers:
  llm:
    assistant:
      harness: claude-code
      model: sonnet
assistant:
  model: assistant
roles:
%s`

// assistantOn is the line that assigns Ask Crewlet its model in every document here.
const assistantOn = "assistant:\n  model: assistant\n"

// Company is the company a case's people work for: Ask Crewlet on when on
// is set, and each of seats a human seat of that handle.
func Company(on bool, seats ...string) (assistant.Company, error) {
	return company(companyDoc, on, seats)
}

// ResearchCompany is [Company] with web research on, from the hosts named,
// and searched through a Tavily server where search is set.
func ResearchCompany(hosts []string, search bool, seats ...string) (assistant.Company, error) {
	research := "  research:\n    hosts: [" + strings.Join(hosts, ", ") + "]\n"
	doc := companyDoc
	if search {
		research += "    search: {mcp_server: search}\n"
		doc = strings.Replace(doc, assistantOn, searchServer+assistantOn, 1)
	}
	doc = strings.Replace(doc, assistantOn, assistantOn+research, 1)
	return company(doc, true, seats)
}

// searchServer is the curated Tavily entry a research search runs.
const searchServer = `mcp_servers:
  - name: search
    command: npm
    args: [exec, --yes, --, tavily-mcp@0.2.22]
    env: {TAVILY_API_KEY: '${TAVILY_API_KEY}'}
`

func company(doc string, on bool, seats []string) (assistant.Company, error) {
	var roles string
	for _, seat := range seats {
		roles += fmt.Sprintf("  - name: %s\n    handle: %s\n    kind: human\n", seat, seat)
	}
	if !on {
		doc = strings.Replace(doc, "  model: assistant\n", "", 1)
	}
	return parsedCompany(fmt.Appendf(nil, doc, roles))
}

// parsedCompany is the company doc describes, with its org.
func parsedCompany(doc []byte) (assistant.Company, error) {
	c, err := config.ParseCompany(doc)
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
