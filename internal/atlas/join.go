package atlas

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"
)

// NotUsedFor is how long an entry the engine records use of can go unused
// before the atlas says so: a month without a command, a call, a turn or a
// push is a fact about use worth a second look, never one about existence.
const NotUsedFor = 30 * 24 * time.Hour

// UseChannel is one kind of use the engine records.
type UseChannel string

const (
	// UseCommand is a command an agent ran on a server (the access log).
	UseCommand UseChannel = "command"
	// UseToolCall is an agent's call to a tool server's tool.
	UseToolCall UseChannel = "tool_call"
	// UseModelCall is a model call a turn made through an entry.
	UseModelCall UseChannel = "model_call"
	// UsePush is a push to a repository.
	UsePush UseChannel = "push"
)

// UseChannels is every channel.
var UseChannels = []UseChannel{UseCommand, UseToolCall, UseModelCall, UsePush}

// Valid reports whether c is a member of the closed set.
func (c UseChannel) Valid() bool { return slices.Contains(UseChannels, c) }

// usedBy is the channel each kind's use is read from; a kind with none is one
// the engine records no use of, which is never "not used".
var usedBy = map[Kind]UseChannel{
	KindHost:       UseCommand,
	KindToolServer: UseToolCall,
	KindModel:      UseModelCall,
	KindRepository: UsePush,
}

// Channel is the channel k's use is read from, "" for a kind with none.
func (k Kind) Channel() UseChannel { return usedBy[k] }

// Use is how much the company relies on an entry through one channel: the
// count in the last week and the last month, the last use, and the seats that
// used it in the month. Counted is false for a channel whose record keeps
// only the last use (a repository's pushes).
type Use struct {
	Channel UseChannel `json:"channel"`
	Last    time.Time  `json:"last,omitzero"`
	Counted bool       `json:"counted"`
	Week    int64      `json:"week"`
	Month   int64      `json:"month"`
	Seats   []string   `json:"seats,omitempty"`
}

// Holder is one agent's account at the vendor an entry is: which seat, on
// which lane of its access, at which level, and whether it is on. Never the
// account's own name, which a vendor often makes a person's email address.
type Holder struct {
	Seat       string    `json:"seat"`
	Lane       string    `json:"lane"`
	Level      string    `json:"level,omitempty"`
	Enabled    bool      `json:"enabled"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// SignalKind is something about an entry that needs attention now. A signal
// is joined from the record that holds it on every read, never copied, so it
// cannot disagree with its source.
type SignalKind string

const (
	// SignalIntegrationBlocked is an integration waiting on a person.
	SignalIntegrationBlocked SignalKind = "integration_blocked"
	// SignalIntegrationFailing is an integration whose last pass failed.
	SignalIntegrationFailing SignalKind = "integration_failing"
	// SignalToolServerDown is a tool server with an instance that failed.
	SignalToolServerDown SignalKind = "tool_server_down"
	// SignalToolServerWaiting is a tool server with an instance still
	// waiting to start.
	SignalToolServerWaiting SignalKind = "tool_server_waiting"
	// SignalSignInExpired is a tool server whose sign-in has lapsed.
	SignalSignInExpired SignalKind = "sign_in_expired"
	// SignalSignInExpiring is a tool server whose sign-in lapses within
	// [SignInWarning].
	SignalSignInExpiring SignalKind = "sign_in_expiring"
	// SignalSignInFailed is a tool server whose sign-in failed to refresh.
	SignalSignInFailed SignalKind = "sign_in_failed"
	// SignalModelSittingOut is a model entry whose plan is spent, sitting
	// out until it resets.
	SignalModelSittingOut SignalKind = "model_sitting_out"
	// SignalModelFailing is a model entry whose last call failed, with no
	// answer since.
	SignalModelFailing SignalKind = "model_failing"
	// SignalServerTestFailed is a server whose last connection test, against
	// its current settings, failed.
	SignalServerTestFailed SignalKind = "server_test_failed"
	// SignalStandingAlarm is a standing engine alarm about a tool server or a
	// model entry (an `mcp_` or `llm_` alarm), named by the alarm's kind.
	SignalStandingAlarm SignalKind = "standing_alarm"
	// SignalMonitorAlerting is a monitor in its alert state, on the monitor
	// and on every entry it watches.
	SignalMonitorAlerting SignalKind = "monitor_alerting"
	// SignalMonitorWarning is a monitor in its warning state, likewise.
	SignalMonitorWarning SignalKind = "monitor_warning"
	// SignalMonitorNoData is a monitor whose data stopped arriving, likewise.
	SignalMonitorNoData SignalKind = "monitor_no_data"
)

// SignalKinds is every signal.
var SignalKinds = []SignalKind{
	SignalIntegrationBlocked, SignalIntegrationFailing, SignalToolServerDown, SignalToolServerWaiting,
	SignalSignInExpired, SignalSignInExpiring, SignalSignInFailed, SignalModelSittingOut, SignalModelFailing,
	SignalServerTestFailed, SignalStandingAlarm,
	SignalMonitorAlerting, SignalMonitorWarning, SignalMonitorNoData,
}

// Valid reports whether k is a member of the closed set.
func (k SignalKind) Valid() bool { return slices.Contains(SignalKinds, k) }

// SignInWarning is how early a sign-in about to lapse is signalled: a week
// leaves a person a working week to sign in again.
const SignInWarning = 7 * 24 * time.Hour

// Severity is how urgent a signal is: danger where something is broken now,
// warning where something needs a person.
type Severity string

// The severities, the more urgent first.
const (
	SeverityDanger  Severity = "danger"
	SeverityWarning Severity = "warning"
)

// Severities is every severity.
var Severities = []Severity{SeverityDanger, SeverityWarning}

// Audience is who may read a signal. The atlas is never a way round another
// screen: what only administrators read elsewhere, they alone read here.
type Audience string

const (
	// AudienceHolders is everybody who may read the atlas.
	AudienceHolders Audience = "holders"
	// AudienceAdmins is administrators alone.
	AudienceAdmins Audience = "admins"
)

// Audiences is every audience.
var Audiences = []Audience{AudienceHolders, AudienceAdmins}

// Signal is one open condition on an entry, since when, and who may read it.
type Signal struct {
	Kind     SignalKind `json:"kind"`
	Severity Severity   `json:"severity"`
	Audience Audience   `json:"audience"`
	Since    time.Time  `json:"since,omitzero"`
	// Alarm is a standing alarm's kind, the engine's own word for it; "" for
	// every other signal.
	Alarm string `json:"alarm,omitempty"`
}

// Joined is what the engine's records say about entries beyond claims: their
// use, the agents holding accounts at them, and their open signals, each keyed
// by the entry it is about. Counted is each channel whose use was read in
// full, so "not used recently" is said only where it is known.
type Joined struct {
	Use     map[Ref][]Use
	Holders map[Ref][]Holder
	Signals map[Ref][]Signal
	Counted map[UseChannel]bool
}

// Join attaches what the records say to the entries of v, each through the
// entry its ref merged into, reads each monitor's state as a signal on it and
// on what it watches ([watch]), and marks every entry of a counted channel
// with no use in [NotUsedFor] as not used recently. It is pure, like the fold:
// v's own entries are left as they were.
func Join(v View, j Joined, now time.Time) View {
	v.Entries = slices.Clone(v.Entries)
	for i := range v.Entries {
		e := &v.Entries[i]
		e.Use, e.Holders, e.Signals = slices.Clip(e.Use), slices.Clip(e.Holders), slices.Clip(e.Signals)
	}
	index := v.Index()
	at := func(r Ref) *Entry {
		if i, ok := index[r.ID()]; ok {
			return &v.Entries[i]
		}
		return nil
	}
	for r, uses := range j.Use {
		if e := at(r); e != nil {
			e.Use = append(e.Use, uses...)
		}
	}
	for r, holders := range j.Holders {
		if e := at(r); e != nil {
			e.Holders = append(e.Holders, holders...)
		}
	}
	for r, signals := range j.Signals {
		if e := at(r); e != nil {
			e.Signals = append(e.Signals, signals...)
		}
	}
	watch(v)
	for i := range v.Entries {
		e := &v.Entries[i]
		e.Use = mergeUse(e.Use)
		slices.SortFunc(e.Holders, func(a, b Holder) int {
			return cmp.Or(cmp.Compare(a.Seat, b.Seat), cmp.Compare(a.Lane, b.Lane))
		})
		slices.SortFunc(e.Signals, func(a, b Signal) int {
			return cmp.Or(cmp.Compare(slices.Index(SignalKinds, a.Kind), slices.Index(SignalKinds, b.Kind)),
				a.Since.Compare(b.Since))
		})
		ch := e.Ref.Kind.Channel()
		if ch == "" || !j.Counted[ch] || e.State == StateRetired {
			continue
		}
		e.NotUsedRecently = !slices.ContainsFunc(e.Use, func(u Use) bool {
			return u.Channel == ch && !u.Last.IsZero() && now.Sub(u.Last) <= NotUsedFor
		})
	}
	return v
}

// monitorSignals is the signal each state of a monitor raises; a state with
// none (ok, unknown) raises nothing.
var monitorSignals = map[string]Signal{
	MonitorAlert:  {Kind: SignalMonitorAlerting, Severity: SeverityDanger, Audience: AudienceHolders},
	MonitorWarn:   {Kind: SignalMonitorWarning, Severity: SeverityWarning, Audience: AudienceHolders},
	MonitorNoData: {Kind: SignalMonitorNoData, Severity: SeverityWarning, Audience: AudienceHolders},
}

// watch raises each live monitor's state as a signal on the monitor and on
// every live entry a monitored_by relation joins to it. READ OFF THE CLAIMS ON
// EVERY READ, never copied onto the watched entry: a monitor that recovers, is
// retired or stops watching an entry takes its signal with it. The state is a
// claim every holder reads, so the signal is every holder's too.
func watch(v View) {
	raised := map[uuid.UUID]Signal{}
	for i := range v.Entries {
		e := &v.Entries[i]
		if e.Ref.Kind != KindMonitor || e.State == StateRetired {
			continue
		}
		var sig Signal
		var since time.Time
		for _, f := range e.Facts {
			switch f.Fact {
			case FactMonitorState:
				if s, ok := monitorSignals[f.Value.Text]; ok {
					sig = s
					sig.Since = f.ObservedAt
				}
			case FactStateSince:
				since, _ = time.Parse(time.RFC3339, f.Value.Text)
			}
		}
		if sig.Kind == "" {
			continue
		}
		// THE VENDOR'S OWN INSTANT where it gave one: the claim's is when a
		// read first saw the state, up to a refresh interval later.
		if !since.IsZero() {
			sig.Since = since
		}
		raised[e.ID] = sig
		e.Signals = append(e.Signals, sig)
	}
	if len(raised) == 0 {
		return
	}
	for i := range v.Entries {
		e := &v.Entries[i]
		if e.State == StateRetired {
			continue
		}
		for _, ed := range e.Relations {
			if sig, ok := raised[ed.To]; ok && ed.Relation == RelationMonitoredBy {
				e.Signals = append(e.Signals, sig)
			}
		}
	}
}

// mergeUse folds the uses of one entry's members into one per channel: the
// counts added, the latest use, the seats together.
func mergeUse(uses []Use) []Use {
	byChannel := map[UseChannel]*Use{}
	for _, u := range uses {
		m, ok := byChannel[u.Channel]
		if !ok {
			u.Seats = slices.Clone(u.Seats)
			byChannel[u.Channel] = &u
			continue
		}
		m.Week, m.Month, m.Counted = m.Week+u.Week, m.Month+u.Month, m.Counted && u.Counted
		if u.Last.After(m.Last) {
			m.Last = u.Last
		}
		m.Seats = append(m.Seats, u.Seats...)
	}
	out := make([]Use, 0, len(byChannel))
	for _, u := range byChannel {
		slices.Sort(u.Seats)
		u.Seats = slices.Compact(u.Seats)
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b Use) int {
		return cmp.Compare(slices.Index(UseChannels, a.Channel), slices.Index(UseChannels, b.Channel))
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
