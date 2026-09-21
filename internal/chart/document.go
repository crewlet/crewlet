package chart

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DocumentVersion is the shape version every stored object here carries, on the
// same terms as the tracker's and the knowledge base's: a row is read, modified
// and written back by whichever node applied the record, so an older build's
// apply would strip a newer build's field out of the object. Unknown fields
// round-trip; an unknown VERSION is refused rather than downgraded.
const DocumentVersion = 1

// Unit is one unit as the chart holds it: what was AUTHORED, plus where the
// structure put it.
//
// THE STRUCTURE FIELDS ARE ON THE ROW AND NOT IN THE CONTENT RECORD, which is
// the shape the two subject kinds imply: [OpUpsert] writes the content and
// [OpPlace] writes ParentKey and Lead, in two records that arbitrate on two
// different subjects and meet here.
type Unit struct {
	V int `json:"v"`

	// Key is the unit's ADDRESS, in [NormalizeKey]'s folded form.
	//
	// THE FOLDED FORM AND NOT THE AUTHORED ONE, because the organisation
	// model already compares unit keys folded — "a name is prose and a
	// reader who cannot tell two teams apart files one team's work under
	// the other" — so the two spellings are one address and storing the
	// authored one would make every comparison fold it again, in each of
	// the places that compare. The spelling a founder typed lives in the
	// company document, which is where they edit it; Name is what a screen
	// renders.
	Key string `json:"key"`

	// FormerKeys are the addresses this unit used to answer to, newest
	// first, capped at [MaxFormerKeys].
	//
	// ON THE ROW AS A LIST rather than in a key directory of its own, and
	// the reason is the size of the corpus: a company has tens or hundreds
	// of units, not the hundreds of thousands of work items
	// `tracker_task_keys` was built for. A former-key lookup here is a scan
	// of a table a page of memory holds, so a child table and its index
	// would be machinery bought for a read that is already free — and a
	// list read WHOLE beside the row it belongs to is one fewer thing an
	// apply has to keep in step.
	FormerKeys []string `json:"former_keys,omitempty"`

	// OriginKey is the address this unit was CREATED under, and it is the
	// unit's identity where [Unit.Key] is only its address.
	//
	// EMPTY MEANS NEVER RENAMED, which is a meaningful zero rather than a
	// missing value: a unit that has never been rekeyed still answers to the
	// key it was created under, so [Unit.Origin] reads Key. It is written
	// exactly once — by the FIRST rekey, which is the last moment the create
	// address is still known — and never touched again.
	//
	// DERIVED BY THE APPLY FROM THE RECORD'S OWN FORMER KEY rather than
	// stated by a writer, for the reason a writer may not state history: the
	// origin is a fact about what already happened to this row, so a record
	// carrying one would be a writer asserting what it read in another
	// transaction. Every node applies the same record to the same row and
	// reaches the same origin.
	//
	// IN THE DOCUMENT AND NOT A COLUMN, deliberately. Nothing queries by it
	// and nothing has to hold it unique — a chart address is unclaimable for
	// ever once used, because a create arbitrates on the object's own subject
	// and a rekey on the address's, so no second object can ever reach an
	// origin another one holds. A column would be an index bought for a read
	// nobody makes and a uniqueness rule something else already keeps.
	OriginKey string `json:"origin_key,omitempty"`

	Name    string   `json:"name,omitempty"`
	Type    string   `json:"type,omitempty"`
	Purpose string   `json:"purpose,omitempty"`
	Goals   []string `json:"goals,omitempty"`

	Channel string `json:"channel,omitempty"`
	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`

	KnowledgeRefs []string `json:"knowledge_refs,omitempty"`

	// ParentKey is the unit this one sits under, empty at the org root, and
	// written only by a structural record.
	ParentKey string `json:"parent_key,omitempty"`

	// Lead is the AUTHORED lead's handle, empty where the unit inherits
	// one. The effective lead is a walk up ParentKey and is deliberately
	// not stored: a derived value written down is a second answer that goes
	// stale the moment an ancestor's lead moves.
	Lead string `json:"lead,omitempty"`

	// Runtime is everything about this unit that only the ENGINE reads:
	// the credentials its direct members inherit, its token budget, its
	// learning toggle, its scheduled work. Opaque here for the reason
	// [Seat.Runtime] gives, and carrying no structure for the same one.
	Runtime json.RawMessage `json:"runtime,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// LastChange is the complete record of what most recently happened to
	// this unit, so a surface can render its card from the row alone.
	LastChange *Change `json:"last_change,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Seat is one seat as the chart holds it.
type Seat struct {
	V int `json:"v"`

	// Handle is the seat's address — the identity every other subsystem
	// already uses, from the mailbox to the derived agent id.
	Handle string `json:"handle"`

	// FormerHandles are the handles this seat used to answer to. See
	// [Unit.FormerKeys] for why they are a list on the row.
	//
	// WHAT THEY KEEP WORKING IS THE REFERENCES — a `manages:` entry, a
	// `lead:`, a vendor mapping somebody already wrote down — for as long as
	// the list holds them. The seat's own durable state does not need them
	// at all: it is keyed on the id [Seat.OriginHandle] anchors, which no
	// rename and no cap can move.
	FormerHandles []string `json:"former_handles,omitempty"`

	// OriginHandle is the handle this seat was CREATED under — the seat's
	// IDENTITY, where [Seat.Handle] is only its address. See [Unit.OriginKey]
	// for the zero-value rule, who writes it and why it is not a column.
	//
	// THE AGENT ID IS DERIVED FROM IT, which is what makes a rename keep the
	// seat's mailbox, its lease, its diary and its schedule ledger: those are
	// all keyed on the id, and the id is a UUIDv5 over (company name, origin
	// handle). A handle is prose somebody types, so it could never be the
	// anchor of anything durable.
	OriginHandle string `json:"origin_handle,omitempty"`

	Kind SeatKind `json:"kind"`

	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`

	Backstory string `json:"backstory,omitempty"`
	Goal      string `json:"goal,omitempty"`

	Responsibilities     []string `json:"responsibilities,omitempty"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty"`

	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`

	// UnitKey is the unit this seat sits in, empty at the org root, and
	// written only by a structural record.
	UnitKey string `json:"unit_key,omitempty"`

	// Runtime is everything about this seat that only the ENGINE reads:
	// its model chain, its tool credentials, its sandbox cell, its worker
	// grants, its schedules, its per-seat vendor identities.
	//
	// # Why it is opaque here
	//
	// This domain owns the chart — who exists, where they sit, who reports
	// to whom — and it can say what every one of those means. It cannot say
	// what an `mcp_env` key is for, and a chart that grew a field per
	// runtime setting would be the company document again with a log under
	// it. So the content travels as bytes the organisation model owns both
	// ends of: [org.Role] carries the shape and the JSON tags, the writer
	// marshals one and the view unmarshals it.
	//
	// IT IS NOT STRUCTURE. Nothing in here may name a unit, a parent or a
	// `manages:` entry — those are the row's own columns and the edge
	// tables, arbitrated on the tree's subject — and a content record
	// carrying them would be a second writer of the shape of the company,
	// contending with nobody.
	Runtime json.RawMessage `json:"runtime,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	LastChange *Change `json:"last_change,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Change is one entry in the record a wake is derived from, and the row an
// operator reads a reorganisation out of. Create-only and never rewritten.
type Change struct {
	V int `json:"v"`

	ID     string     `json:"id"`
	Object ObjectRef  `json:"object"`
	Kind   ChangeKind `json:"kind"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	// Revision is the company configuration revision this change came
	// from, when it came from one. Its absence is the answer to "which edit
	// did this": nobody's — somebody did it by hand.
	Revision string `json:"revision,omitempty"`

	// Fields is what moved, as text. A move states the unit it left and the
	// unit it joined, which is the pair a person actually wants and the one
	// a diff of two documents cannot give them.
	Fields map[string]Delta `json:"fields,omitempty"`

	// Summary is at most [MaxSummary] bytes of what a card should show.
	Summary string `json:"summary,omitempty"`

	TurnID string `json:"turn_id,omitempty"`

	Quiet bool `json:"quiet,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Origin is the address this unit was created under: [Unit.OriginKey] where a
// rekey has recorded one, and [Unit.Key] where none has, because a unit that
// was never rekeyed still answers to the key it was created under.
//
// ONE READING, so nothing above this package has to remember the zero-value
// rule or gets it half right.
func (u Unit) Origin() string {
	if u.OriginKey != "" {
		return u.OriginKey
	}
	return u.Key
}

// Origin is [Unit.Origin] for a seat: the handle it was created under, which
// is what its agent id is derived from.
func (s Seat) Origin() string {
	if s.OriginHandle != "" {
		return s.OriginHandle
	}
	return s.Handle
}

// Delta is one field's before and after, as text.
type Delta struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("chart: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// Validate refuses a unit a writer could not have meant.
//
// THE CAPS ARE CHECKED WHERE A RECORD IS WRITTEN and never where one is
// applied, which is the asymmetry every value rule in this tree follows: the
// applier deliberately SALVAGES, because refusing a record there would stop
// that object's every later change on every node.
func (u Unit) Validate() error {
	if err := checkKey("key", u.Key); err != nil {
		return err
	}
	if len(u.FormerKeys) > MaxFormerKeys {
		return invalid("former_keys", "%d retired keys and the cap is %d — the "+
			"oldest is dropped rather than the newest, because the reference "+
			"somebody typed last week is the one still being followed",
			len(u.FormerKeys), MaxFormerKeys)
	}
	for _, key := range u.FormerKeys {
		if err := checkKey("former_keys", key); err != nil {
			return err
		}
	}
	if u.OriginKey != "" {
		if err := checkKey("origin_key", u.OriginKey); err != nil {
			return err
		}
	}
	if len(u.Name) > MaxName {
		return invalid("name", "%d bytes and the cap is %d", len(u.Name), MaxName)
	}
	if len(u.Purpose) > MaxProse {
		return invalid("purpose", "%d bytes and the cap is %d — a purpose past "+
			"that is a knowledge-base page, and the unit can point at one",
			len(u.Purpose), MaxProse)
	}
	if err := checkList("goals", u.Goals); err != nil {
		return err
	}
	return checkList("knowledge_refs", u.KnowledgeRefs)
}

// Validate refuses a seat a writer could not have meant.
func (s Seat) Validate() error {
	if err := checkKey("handle", s.Handle); err != nil {
		return err
	}
	if len(s.FormerHandles) > MaxFormerKeys {
		return invalid("former_handles", "%d retired handles and the cap is %d",
			len(s.FormerHandles), MaxFormerKeys)
	}
	for _, handle := range s.FormerHandles {
		if err := checkKey("former_handles", handle); err != nil {
			return err
		}
	}
	if s.OriginHandle != "" {
		if err := checkKey("origin_handle", s.OriginHandle); err != nil {
			return err
		}
	}
	if !s.Kind.Valid() {
		return invalid("kind", "%q is not a seat kind this build serves (want "+
			"%s or %s) — an agent seat has an inbox, a turn loop and a model "+
			"chain, and a human seat has none of them", s.Kind, SeatAgent, SeatHuman)
	}
	if len(s.Name) > MaxName {
		return invalid("name", "%d bytes and the cap is %d", len(s.Name), MaxName)
	}
	if len(s.Email) > MaxEmail {
		return invalid("email", "%d bytes and the cap is %d, which is the "+
			"longest address RFC 5321 permits", len(s.Email), MaxEmail)
	}
	if len(s.Backstory) > MaxProse {
		return invalid("backstory", "%d bytes and the cap is %d — a backstory is "+
			"rendered into every system prompt this seat's turns build, so the "+
			"cap is a token budget before it is a storage one",
			len(s.Backstory), MaxProse)
	}
	if len(s.Goal) > MaxProse {
		return invalid("goal", "%d bytes and the cap is %d", len(s.Goal), MaxProse)
	}
	if err := checkList("responsibilities", s.Responsibilities); err != nil {
		return err
	}
	return checkList("behavioral_guidelines", s.BehavioralGuidelines)
}

// checkKey refuses a key that could not address an object.
//
// IT REFUSES THE SCOPE SEPARATOR AND THE BROKER'S WILDCARDS, because a key is
// both a subject token on [KindRekey] and a segment of every scope path the
// object's records are filed under — so a key carrying one of them publishes
// somewhere nobody consumes, or files a deferral under a path no probe reaches.
// [Subject.Validate] states the same rule from the other end, and both have to,
// because a key reaches a subject and a scope path by two different routes.
func checkKey(field, key string) error {
	switch {
	case key == "":
		return invalid(field, "is empty — a unit's key and a seat's handle are "+
			"what every `lead:` and every `manages:` entry resolve, so an "+
			"object without one is addressable by nothing")
	case len(key) > MaxKey:
		return invalid(field, "%q is %d bytes and the cap is %d", key, len(key), MaxKey)
	case key != NormalizeKey(key):
		return invalid(field, "%q is not in its folded form (%q) — unit keys are "+
			"compared folded, so two spellings of one key would read as two "+
			"objects on every screen while each reference reached only one",
			key, NormalizeKey(key))
	case strings.ContainsAny(key, scopeSeparator+" \t\n*>"):
		return invalid(field, "%q carries whitespace, a wildcard or the scope "+
			"path separator — a key is a broker subject token AND a scope path "+
			"segment, and either one would send this object's records where "+
			"nothing looks for them", key)
	}
	return nil
}

// checkList refuses a repeated prose field past its caps.
func checkList(field string, values []string) error {
	if len(values) > MaxList {
		return invalid(field, "%d entries and the cap is %d — every one of them "+
			"is rendered into a prompt on every turn, and a list past that is a "+
			"document", len(values), MaxList)
	}
	for _, v := range values {
		if len(v) > MaxProse {
			return invalid(field, "an entry is %d bytes and the cap is %d",
				len(v), MaxProse)
		}
	}
	return nil
}

// ---- encoding --------------------------------------------------------- //

// The known field names per record. Explicit rather than reflective, for the
// reason the tracker gives: a name missing here is decoded into the struct AND
// carried as unknown, so the next encode writes the stale carried copy back
// over what the caller set. A test asserts every declared name is covered.
var (
	unitFields = fieldSet(Unit{}, "former_keys", "name", "type", "purpose",
		"goals", "channel", "project", "space", "knowledge_refs", "parent_key",
		"lead", "runtime", "last_change")
	seatFields = fieldSet(Seat{}, "former_handles", "name", "email", "backstory",
		"goal", "responsibilities", "behavioral_guidelines", "project", "space",
		"unit_key", "runtime", "last_change")
	changeFields = fieldSet(Change{}, "actor", "actor_kind", "operator_id",
		"revision", "fields", "summary", "turn_id", "quiet")
)

// EncodeUnit renders a unit.
func EncodeUnit(u Unit) ([]byte, error) { return encode(u, u.Extra) }

// DecodeUnit reads a unit.
func DecodeUnit(data []byte) (Unit, error) {
	var u Unit
	extra, err := decodeInto(data, &u, unitFields)
	if err != nil {
		return Unit{}, fmt.Errorf("chart: decode unit: %w", err)
	}
	if err := checkVersion(u.V); err != nil {
		return Unit{}, err
	}
	u.Extra = extra
	return u, nil
}

// EncodeSeat renders a seat.
func EncodeSeat(s Seat) ([]byte, error) { return encode(s, s.Extra) }

// DecodeSeat reads a seat.
func DecodeSeat(data []byte) (Seat, error) {
	var s Seat
	extra, err := decodeInto(data, &s, seatFields)
	if err != nil {
		return Seat{}, fmt.Errorf("chart: decode seat: %w", err)
	}
	if err := checkVersion(s.V); err != nil {
		return Seat{}, err
	}
	s.Extra = extra
	return s, nil
}

// EncodeChange renders a change.
func EncodeChange(c Change) ([]byte, error) { return encode(c, c.Extra) }

// DecodeChange reads a change.
func DecodeChange(data []byte) (Change, error) {
	var c Change
	extra, err := decodeInto(data, &c, changeFields)
	if err != nil {
		return Change{}, fmt.Errorf("chart: decode change: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Change{}, err
	}
	c.Extra = extra
	return c, nil
}

func checkVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}

// encode marshals a record and folds unknown fields back in. A carried field
// LOSES to a known one, so a stale carried copy can never undo the write that
// set it.
func encode(record any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("chart: encode: %w", err)
	}
	if len(extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("chart: encode: %w", err)
	}
	for name, value := range extra {
		if _, known := merged[name]; !known {
			merged[name] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("chart: encode: %w", err)
	}
	return out, nil
}

// decodeInto unmarshals into out and returns the fields out has no home for.
func decodeInto(data []byte, out any, known map[string]bool) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for name, value := range all {
		if known[name] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[name] = value
	}
	return extra, nil
}

// fieldSet is the JSON names a struct defines: the ones a zero value marshals,
// plus the omitempty names given explicitly.
func fieldSet(v any, omitted ...string) map[string]bool {
	data, err := json.Marshal(v)
	if err != nil {
		panic("chart: a record type does not marshal: " + err.Error())
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		panic("chart: a record type does not marshal to an object: " + err.Error())
	}
	out := make(map[string]bool, len(named)+len(omitted))
	for name := range named {
		out[name] = true
	}
	for _, name := range omitted {
		out[name] = true
	}
	return out
}
