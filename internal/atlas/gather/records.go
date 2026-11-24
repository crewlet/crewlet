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
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/seat/placement"
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

// Listings is what each node's MCP server instances listed, as this node
// watches it (the engine's): a read on a turn waits for no listing to cross
// a link, each up to 896 KiB. An error until the watch has answered.
type Listings interface {
	MCPListings(ctx context.Context) ([]coord.MCPListing, error)
}

// SignIns is the public half of each tool server's sign-in, as the
// coordination store keeps it.
type SignIns interface {
	ListOAuthGrants(ctx context.Context, f coord.GrantFamily) ([]coord.OAuthGrant, error)
}

// Nodes is the live nodes' leases, each carrying the node's last beat.
type Nodes interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// NodeStatuses is the nodes' status records ([coord.NodeStatusRecords]): each
// node's MCP and model rows and what it runs on, counted against its presence.
type NodeStatuses interface {
	NodeStatusRecords(ctx context.Context, read coord.NodeStatusRead) ([]coord.NodeStatusRecord, error)
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
	case src.Listings == nil || src.SignIns == nil:
		note(SourceToolServers, ErrNoSource)
	case leaseErr != nil:
		note(SourceToolServers, fmt.Errorf("a listing counts only while its node is live, and the nodes are unread: %w",
			leaseErr))
	default:
		listings, listErr := src.Listings.MCPListings(ctx)
		for _, l := range coord.LiveMCPListings(listings, leases) {
			p := derive.ToolServerListing{Server: l.Server, ToolCount: len(l.Tools), At: l.At}
			if l.Info != nil {
				p.Name, p.Version = l.Info.Name, l.Info.Version
			}
			r.Listings = append(r.Listings, p)
		}
		grants, grantErr := src.SignIns.ListOAuthGrants(ctx, coord.MCPGrantFamily)
		for _, g := range grants {
			p := derive.ToolServerSignIn{Server: g.Name, Issuer: g.Issuer}
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
		// WHAT EACH NODE SAYS OF ITSELF rides its status record, counted
		// against its presence. A store that cannot answer leaves every node
		// drawn from its presence alone — its id and its broker — and says
		// the source was not read whole.
		var statuses map[string]coord.NodeStatusRecord
		statusErr := ErrNoSource
		if src.NodeStatuses != nil {
			var records []coord.NodeStatusRecord
			if records, statusErr = src.NodeStatuses.NodeStatusRecords(ctx,
				coord.NodeStatusRead{MCP: true, Models: true}); statusErr == nil {
				statuses = coord.LiveNodeStatuses(records, leases)
			}
		}
		for _, lease := range leases {
			id, _ := coord.ClassNode.Name(lease.Resource)
			status, ok := statuses[id]
			r.Fleet = append(r.Fleet, fleetNode(lease, status.Infra))
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
		if statusErr != nil {
			statusErr = fmt.Errorf("each node's status record, which says what it runs on: %w", statusErr)
		}
		note(SourceNodes, statusErr)
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

// fleetNode is one live node as the atlas draws it: its id and how its broker
// takes part off its profile, and what its status record says of what it runs
// on. A node whose record says nothing, or is not counted yet, is still drawn,
// with nothing beyond those.
func fleetNode(lease coord.Lease, infra coord.NodeInfra) derive.FleetNode {
	id, _ := coord.ClassNode.Name(lease.Resource)
	profile := placement.FromMeta(id, lease.Meta)
	n := derive.FleetNode{ID: id, BrokerMember: profile.Broker == placement.BrokerMember,
		ExternalBroker: profile.Broker == placement.BrokerClient}
	n.Address = infra.Address
	n.BrokerServers = infra.BrokerServers
	n.ObjectsKnown = infra.ObjectsBackend != ""
	n.ObjectsS3 = infra.ObjectsBackend == string(config.ObjectBackendS3) && infra.Bucket != ""
	n.Bucket, n.Region = infra.Bucket, infra.Region
	return n
}
