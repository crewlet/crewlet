package derive

import (
	"cmp"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/integration"
)

// Records is what the engine's own records say about the entries, as closed
// projections: each field is one the atlas shows, and none can carry a
// person's email address, a credential or a vendor's free text. A nil list is
// a record nobody read; gather says which.
type Records struct {
	Integrations []IntegrationStatus
	Holders      []AccountHolder
	Listings     []ToolServerListing
	ToolHealth   []ToolServerHealth
	SignIns      []ToolServerSignIn
	Models       []ModelHealth
	ServerTests  []ServerTest
	Commands     []HostUse
	ToolCalls    []ToolUse
	ModelCalls   []ModelUse
	Pushes       []RepoPush
}

// IntegrationStatus is one integration's standing as its reconcile pass left
// it: the base address its registration at the vendor points at, and whether
// it waits on a person or failed.
type IntegrationStatus struct {
	Kind     integration.Kind
	Endpoint string
	Blocked  bool
	Failing  bool
	// Since is the pass that left it so.
	Since time.Time
}

// AccountHolder is one agent's account on one lane of an integration, as the
// last sync observed it at the vendor.
type AccountHolder struct {
	Kind       integration.Kind
	Seat       string
	Lane       string
	Level      string
	Exists     bool
	Enabled    bool
	ObservedAt time.Time
}

// ToolServerListing is what one node's instances of a tool server listed: the
// server's own name and version, and how many tools. Its instructions and its
// tools' descriptions are a vendor's text, and are never read.
type ToolServerListing struct {
	Server    string
	Name      string
	Version   string
	ToolCount int
	At        time.Time
}

// ToolServerHealth is one node's instances of a tool server at its last beat.
type ToolServerHealth struct {
	Server    string
	Failed    int
	Waiting   int
	DownSince time.Time
	At        time.Time
}

// ToolServerSignIn is the public half of a tool server's sign-in: who issued
// it and when it lapses, and whether its refresh failed. Never who signed in.
type ToolServerSignIn struct {
	Server   string
	Issuer   string
	Expiry   time.Time
	FailedAt time.Time
}

// ModelHealth is one node's reading of one model entry at its last beat. The
// plan's sign-in account is a person's email address, and is never read.
type ModelHealth struct {
	Key             string
	SittingOutSince time.Time
	LastFailure     time.Time
	LastAnswer      time.Time
}

// ServerTest is a server's last connection test against its current settings.
type ServerTest struct {
	Host string
	OK   bool
	At   time.Time
}

// HostUse is one seat's commands on one server.
type HostUse struct {
	Host, Seat  string
	Week, Month int64
	Last        time.Time
}

// ToolUse is one seat's calls to one tool server.
type ToolUse struct {
	Server, Seat string
	Week, Month  int64
	Last         time.Time
}

// ModelUse is one seat's model calls through one entry.
type ModelUse struct {
	Key, Seat   string
	Week, Month int64
	Last        time.Time
}

// RepoPush is a Gitlet repository's last push.
type RepoPush struct {
	Repo string
	Last time.Time
}

// Records derives what the engine's records say: facts as claims (an
// integration's inbound address, a tool server's own name, version, tool
// count and sign-in), and beside them, joined rather than claimed, the agents
// holding each account, each entry's use and its open signals.
func (s Settings) Records(r Records, at time.Time) (Derived, atlas.Joined) {
	accounts := s.Accounts(at)
	j := atlas.Joined{Use: map[atlas.Ref][]atlas.Use{}, Holders: map[atlas.Ref][]atlas.Holder{},
		Signals: map[atlas.Ref][]atlas.Signal{}, Counted: map[atlas.UseChannel]bool{}}
	signal := func(ref atlas.Ref, k atlas.SignalKind, sev atlas.Severity, aud atlas.Audience, since time.Time) {
		j.Signals[ref] = append(j.Signals[ref], atlas.Signal{Kind: k, Severity: sev, Audience: aud, Since: since})
	}

	integrations := newBuilder(atlas.RecordSource(atlas.RecordIntegrations), at)
	for _, st := range r.Integrations {
		account, ok := accounts[st.Kind]
		if !ok {
			continue
		}
		integrations.says("integration:"+string(st.Kind)+".endpoint", account, atlas.FactInboundURL, st.Endpoint)
		switch {
		case st.Blocked:
			signal(account, atlas.SignalIntegrationBlocked, atlas.SeverityWarning, atlas.AudienceAdmins, st.Since)
		case st.Failing:
			signal(account, atlas.SignalIntegrationFailing, atlas.SeverityDanger, atlas.AudienceAdmins, st.Since)
		}
	}
	for _, h := range r.Holders {
		if account, ok := accounts[h.Kind]; ok && h.Exists {
			j.Holders[account] = append(j.Holders[account], atlas.Holder{Seat: h.Seat, Lane: h.Lane, Level: h.Level,
				Enabled: h.Enabled, ObservedAt: h.ObservedAt})
		}
	}

	tools := newBuilder(atlas.RecordSource(atlas.RecordToolServers), at)
	toolServer := func(name string) atlas.Ref {
		r, _ := atlas.NewRef(atlas.KindToolServer, atlas.ToolServerPrefix+"/"+name)
		return r
	}
	for _, l := range r.Listings {
		ref, where := toolServer(l.Server), "tool_server:"+l.Server
		if ref.Key == "" {
			continue
		}
		tools.at = observed(l.At, at)
		tools.says(where+".name", ref, atlas.FactServerName, l.Name)
		tools.says(where+".version", ref, atlas.FactServerVersion, l.Version)
		tools.says(where+".tools", ref, atlas.FactToolCount, strconv.Itoa(l.ToolCount))
	}
	for _, h := range r.ToolHealth {
		ref := toolServer(h.Server)
		switch {
		case ref.Key == "":
		case h.Failed > 0:
			signal(ref, atlas.SignalToolServerDown, atlas.SeverityDanger, atlas.AudienceHolders, cmp.Or(h.DownSince, h.At))
		case h.Waiting > 0:
			signal(ref, atlas.SignalToolServerWaiting, atlas.SeverityWarning, atlas.AudienceHolders, h.At)
		}
	}
	tools.at = at
	for _, g := range r.SignIns {
		ref, where := toolServer(g.Server), "tool_server:"+g.Server+".sign_in"
		if ref.Key == "" {
			continue
		}
		tools.says(where+".issuer", ref, atlas.FactOAuthIssuer, g.Issuer)
		if !g.Expiry.IsZero() {
			tools.says(where+".expiry", ref, atlas.FactSignInExpires, atlas.Instant(g.Expiry).Text)
		}
		switch {
		case !g.FailedAt.IsZero():
			signal(ref, atlas.SignalSignInFailed, atlas.SeverityDanger, atlas.AudienceHolders, g.FailedAt)
		case !g.Expiry.IsZero() && !g.Expiry.After(at):
			signal(ref, atlas.SignalSignInExpired, atlas.SeverityDanger, atlas.AudienceHolders, g.Expiry)
		case !g.Expiry.IsZero() && g.Expiry.Sub(at) <= atlas.SignInWarning:
			signal(ref, atlas.SignalSignInExpiring, atlas.SeverityWarning, atlas.AudienceHolders, g.Expiry)
		}
	}

	model := func(key string) atlas.Ref {
		r, _ := atlas.NewRef(atlas.KindModel, atlas.ModelLLMPrefix+"/"+key)
		return r
	}
	for _, m := range r.Models {
		ref := model(m.Key)
		switch {
		case ref.Key == "":
		case !m.SittingOutSince.IsZero():
			signal(ref, atlas.SignalModelSittingOut, atlas.SeverityWarning, atlas.AudienceHolders, m.SittingOutSince)
		case m.LastFailure.After(m.LastAnswer):
			signal(ref, atlas.SignalModelFailing, atlas.SeverityDanger, atlas.AudienceHolders, m.LastFailure)
		}
	}
	host := func(id string) atlas.Ref {
		r, _ := atlas.NewRef(atlas.KindHost, atlas.HostSSHPrefix+"/"+id)
		return r
	}
	for _, t := range r.ServerTests {
		if ref := host(t.Host); ref.Key != "" && !t.OK {
			signal(ref, atlas.SignalServerTestFailed, atlas.SeverityDanger, atlas.AudienceAdmins, t.At)
		}
	}

	use := func(ref atlas.Ref, u atlas.Use) {
		if ref.Key != "" {
			j.Use[ref] = append(j.Use[ref], u)
		}
	}
	if r.Commands != nil {
		j.Counted[atlas.UseCommand] = true
		for _, u := range r.Commands {
			use(host(u.Host), atlas.Use{Channel: atlas.UseCommand, Counted: true, Week: u.Week, Month: u.Month,
				Last: u.Last, Seats: []string{u.Seat}})
		}
	}
	if r.ToolCalls != nil {
		j.Counted[atlas.UseToolCall] = true
		for _, u := range r.ToolCalls {
			use(toolServer(u.Server), atlas.Use{Channel: atlas.UseToolCall, Counted: true, Week: u.Week,
				Month: u.Month, Last: u.Last, Seats: []string{u.Seat}})
		}
	}
	if r.ModelCalls != nil {
		j.Counted[atlas.UseModelCall] = true
		for _, u := range r.ModelCalls {
			use(model(u.Key), atlas.Use{Channel: atlas.UseModelCall, Counted: true, Week: u.Week, Month: u.Month,
				Last: u.Last, Seats: []string{u.Seat}})
		}
	}
	if r.Pushes != nil {
		j.Counted[atlas.UsePush] = true
		for _, p := range r.Pushes {
			ref, _ := atlas.NewRef(atlas.KindRepository, atlas.GitletPrefix+"/"+p.Repo)
			if !p.Last.IsZero() {
				use(ref, atlas.Use{Channel: atlas.UsePush, Last: p.Last})
			}
		}
	}

	out := integrations.out
	out.Claims = append(out.Claims, tools.out.Claims...)
	out.Withheld = append(out.Withheld, tools.out.Withheld...)
	return out, j
}

// observed is when a record says it was read, or at where it says nothing.
func observed(t, at time.Time) time.Time {
	if t.IsZero() {
		return at
	}
	return t
}
