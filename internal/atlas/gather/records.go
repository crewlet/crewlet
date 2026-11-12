package gather

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// IntegrationStates is each integration's standing as its reconcile pass left
// it (integration.CoordStore).
type IntegrationStates interface {
	LoadIntegrations(ctx context.Context) ([]integration.State, error)
}

// SeatAccess is the ledger of the accounts the engine made for agents at each
// integration (seataccess.CoordLedger).
type SeatAccess interface {
	Records(ctx context.Context, kind integration.Kind) (map[string]seataccess.Record, error)
}

// ToolServers is what the coordination store keeps about tool servers: what
// each node's instances listed, and the public half of each sign-in.
type ToolServers interface {
	ListMCPListings(ctx context.Context) ([]coord.MCPListing, error)
	ListMCPGrants(ctx context.Context) ([]coord.MCPGrant, error)
}

// Nodes is the live nodes' leases, each carrying the node's last beat.
type Nodes interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// ServerTests is the connection tests people ran against the servers.
type ServerTests interface {
	ListSSHTests(ctx context.Context) ([]coord.SSHTest, error)
}

// AccessLog counts each server's use from the commands the engine ran there
// (accessdomain.Reader).
type AccessLog interface {
	HostUse(ctx context.Context, week, month time.Time, fresh statelog.Freshness) ([]accessdomain.HostUse, error)
}

// Usage is the replicated usage history (usage.Reader): model calls per entry
// and tool calls per server.
type Usage interface {
	Spend(ctx context.Context, q usage.SpendQuery) ([]usage.SpendRow, error)
	ServerToolCalls(ctx context.Context, q usage.ServerToolCallsQuery) ([]usage.ServerToolCallsRow, error)
}

// Alarms is this node's latest alarm evaluation (the engine's), and whether
// it has evaluated any yet.
type Alarms interface {
	LatestAlarms() []statelog.Alarm
	AlarmsEvaluated() bool
}

// errAlarmsUnevaluated is a node that has not evaluated its alarms yet: no
// alarm is unknown, not none.
var errAlarmsUnevaluated = errors.New("this node has not evaluated its alarms yet")

// UseWeek is the short window a use is counted over beside the month.
const UseWeek = 7 * 24 * time.Hour

// window is the company days and the instants a read counts use over: the
// last week and the last [atlas.NotUsedFor], each ending today.
type window struct {
	week, month            time.Time
	today, weekDay, month0 string
	loc                    *time.Location
}

func windowAt(now time.Time, loc *time.Location) window {
	today := period.At(period.Day, now, loc)
	return window{
		week: now.Add(-UseWeek), month: now.Add(-atlas.NotUsedFor),
		today: today.Label, weekDay: today.Shift(-6).Label, month0: today.Shift(-29).Label, loc: loc,
	}
}

// readRecords reads every record source this node serves, each into the
// closed projections derive takes, and says how each read went.
func readRecords(ctx context.Context, src Sources, s derive.Settings, req Request, pushes []derive.RepoPush,
	pushesRead bool) (derive.Records, []Coverage) {
	var r derive.Records
	var cov []Coverage
	note := func(name SourceName, err error) {
		c := Coverage{Source: name, Complete: err == nil}
		if err != nil {
			c.Reason = err.Error()
		}
		cov = append(cov, c)
	}
	w := windowAt(req.Now, s.Location())

	if src.Integrations == nil {
		note(SourceIntegrations, ErrNoSource)
	} else {
		states, err := src.Integrations.LoadIntegrations(ctx)
		for _, st := range states {
			r.Integrations = append(r.Integrations, derive.IntegrationStatus{Kind: st.Kind, Endpoint: st.Endpoint,
				Blocked: st.Outcome == integration.OutcomeBlocked, Failing: st.LastError != "",
				Since: cmp.Or(st.LastAttemptAt, st.SettledAt)})
		}
		note(SourceIntegrations, err)
	}
	if src.SeatAccess == nil {
		note(SourceSeatAccess, ErrNoSource)
	} else {
		r.Holders = []derive.AccountHolder{}
		var failed []error
		for _, kind := range integration.Kinds {
			records, err := src.SeatAccess.Records(ctx, kind)
			if err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", kind, err))
				continue
			}
			for _, rec := range records {
				for lane, l := range rec.Lanes {
					r.Holders = append(r.Holders, derive.AccountHolder{Kind: kind, Seat: rec.Handle, Lane: string(lane),
						Level: l.Observed.Level, Exists: l.Observed.Exists, Enabled: l.Observed.Enabled,
						ObservedAt: l.ObservedAt})
				}
			}
		}
		note(SourceSeatAccess, errors.Join(failed...))
	}
	var leases []coord.Lease
	var leaseErr error
	if src.Nodes == nil {
		leaseErr = ErrNoSource
	} else {
		leases, leaseErr = src.Nodes.ListLive(ctx, coord.ClassNode)
	}
	switch {
	case src.ToolServers == nil:
		note(SourceToolServers, ErrNoSource)
	case leaseErr != nil:
		note(SourceToolServers, fmt.Errorf("a listing counts only while its node is live, and the nodes are unread: %w",
			leaseErr))
	default:
		listings, listErr := src.ToolServers.ListMCPListings(ctx)
		for _, l := range coord.LiveMCPListings(listings, leases) {
			p := derive.ToolServerListing{Server: l.Server, ToolCount: len(l.Tools), At: l.At}
			if l.Info != nil {
				p.Name, p.Version = l.Info.Name, l.Info.Version
			}
			r.Listings = append(r.Listings, p)
		}
		grants, grantErr := src.ToolServers.ListMCPGrants(ctx)
		for _, g := range grants {
			p := derive.ToolServerSignIn{Server: g.Server, Issuer: g.Issuer}
			if g.Expiry != nil {
				p.Expiry = *g.Expiry
			}
			if g.FailedAt != nil {
				p.FailedAt = *g.FailedAt
			}
			r.SignIns = append(r.SignIns, p)
		}
		note(SourceToolServers, errors.Join(listErr, grantErr))
	}
	if leaseErr != nil {
		note(SourceNodes, leaseErr)
	} else {
		for _, lease := range leases {
			status, ok := coord.StatusFromMeta(lease.Meta)
			if !ok {
				continue
			}
			for _, m := range status.MCP {
				r.ToolHealth = append(r.ToolHealth, derive.ToolServerHealth{Server: m.Server, Failed: m.Failed,
					Waiting: m.Waiting, DownSince: m.DownSince, At: req.Now})
			}
			for _, m := range status.Models {
				p := derive.ModelHealth{Key: m.Key, LastAnswer: m.LastAnswer}
				if m.SittingOut != nil {
					p.SittingOutSince = cmp.Or(m.SittingOut.Since, req.Now)
				}
				if m.LastFailure != nil {
					p.LastFailure = m.LastFailure.At
				}
				r.Models = append(r.Models, p)
			}
		}
		note(SourceNodes, nil)
	}
	if src.ServerTests == nil {
		note(SourceServerTests, ErrNoSource)
	} else {
		tests, err := src.ServerTests.ListSSHTests(ctx)
		r.ServerTests = currentTests(tests, s)
		note(SourceServerTests, err)
	}
	if src.AccessLog == nil {
		note(SourceAccessLog, ErrNoSource)
	} else {
		use, err := src.AccessLog.HostUse(ctx, w.week, w.month, req.Fresh)
		if err == nil {
			r.Commands = make([]derive.HostUse, 0, len(use))
			for _, u := range use {
				r.Commands = append(r.Commands, derive.HostUse{Host: u.Host, Seat: u.Handle, Week: u.Week,
					Month: u.Month, Last: u.Last})
			}
		}
		note(SourceAccessLog, err)
	}
	if src.Usage == nil {
		note(SourceUsage, ErrNoSource)
	} else {
		models, modelErr := modelCalls(ctx, src.Usage, w)
		tools, toolErr := toolCalls(ctx, src.Usage, s.ToolServers(), w)
		if modelErr == nil {
			r.ModelCalls = models
		}
		if toolErr == nil {
			r.ToolCalls = tools
		}
		note(SourceUsage, errors.Join(modelErr, toolErr))
	}
	switch {
	case src.Alarms == nil:
		note(SourceAlarms, ErrNoSource)
	case !src.Alarms.AlarmsEvaluated():
		note(SourceAlarms, errAlarmsUnevaluated)
	default:
		r.Alarms = standingAlarms(src.Alarms.LatestAlarms())
		note(SourceAlarms, nil)
	}
	if pushesRead {
		r.Pushes = pushes
	}
	return r, cov
}

// standingAlarms is every standing alarm about one model entry or one tool
// server, by its kind and the entry it names. An alarm's detail is the
// engine's sentence about it, and is never read: the kind says what it is.
func standingAlarms(alarms []statelog.Alarm) []derive.Alarm {
	models, servers := statelog.PerModelKinds(), statelog.PerServerKinds()
	out := []derive.Alarm{}
	for _, a := range alarms {
		var about atlas.Kind
		switch {
		case slices.Contains(models, a.Kind):
			about = atlas.KindModel
		case slices.Contains(servers, a.Kind):
			about = atlas.KindToolServer
		default:
			continue
		}
		for _, e := range a.Entries {
			out = append(out, derive.Alarm{Kind: string(a.Kind), About: about, Key: e.Key, Since: e.Since})
		}
	}
	return out
}

// currentTests is each server's newest test against its settings as they are
// now: a test of an address or a key since changed says nothing about it.
func currentTests(tests []coord.SSHTest, s derive.Settings) []derive.ServerTest {
	newest := map[string]coord.SSHTest{}
	for _, t := range tests {
		host, ok := s.SSHHost(t.Host)
		if !ok || t.HostDigest != ssh.HostDigest(host) {
			continue
		}
		if prior, ok := newest[t.Host]; !ok || t.At.After(prior.At) {
			newest[t.Host] = t
		}
	}
	out := make([]derive.ServerTest, 0, len(newest))
	for _, t := range newest {
		out = append(out, derive.ServerTest{Host: t.Host, OK: t.OK, At: t.At})
	}
	return out
}

// modelCalls is each seat's model calls per entry over the month, the week's
// counted apart, read from the usage history's day rows.
func modelCalls(ctx context.Context, u Usage, w window) ([]derive.ModelUse, error) {
	rows, err := u.Spend(ctx, usage.SpendQuery{From: w.month0, To: w.today})
	if err != nil {
		return nil, err
	}
	type key struct{ entry, seat string }
	sums := map[key]*derive.ModelUse{}
	for _, row := range rows {
		if row.ProviderKey == "" || row.Calls == 0 {
			continue
		}
		k := key{row.ProviderKey, row.Handle}
		m, ok := sums[k]
		if !ok {
			m = &derive.ModelUse{Key: row.ProviderKey, Seat: row.Handle}
			sums[k] = m
		}
		m.Month += row.Calls
		if row.Day >= w.weekDay {
			m.Week += row.Calls
		}
		if day, err := period.Parse(period.Day, row.Day, w.loc); err == nil && day.Start.After(m.Last) {
			m.Last = day.Start
		}
	}
	out := make([]derive.ModelUse, 0, len(sums))
	for _, m := range sums {
		out = append(out, *m)
	}
	return out, nil
}

// toolCalls is each seat's calls to each tool server over the month and the
// week, two reads a server through the usage history's server index.
func toolCalls(ctx context.Context, u Usage, servers []string, w window) ([]derive.ToolUse, error) {
	var out []derive.ToolUse
	for _, server := range servers {
		month, err := u.ServerToolCalls(ctx, usage.ServerToolCallsQuery{From: w.month0, To: w.today, Server: server})
		if err != nil {
			return nil, err
		}
		week, err := u.ServerToolCalls(ctx, usage.ServerToolCallsQuery{From: w.weekDay, To: w.today, Server: server})
		if err != nil {
			return nil, err
		}
		bySeat := map[string]*derive.ToolUse{}
		seat := func(handle string) *derive.ToolUse {
			t, ok := bySeat[handle]
			if !ok {
				t = &derive.ToolUse{Server: server, Seat: handle}
				bySeat[handle] = t
			}
			return t
		}
		for _, row := range month {
			t := seat(row.Handle)
			t.Month += row.Calls
			if row.LastAt.After(t.Last) {
				t.Last = row.LastAt
			}
		}
		for _, row := range week {
			seat(row.Handle).Week += row.Calls
		}
		for _, t := range bySeat {
			out = append(out, *t)
		}
	}
	if out == nil {
		out = []derive.ToolUse{}
	}
	return out, nil
}
