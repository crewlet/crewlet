package accessdomain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE GROWTH RECORD (ADR-0171): one record per step of a growth act, on the
// subject of the act's seat and the step's own id (package doc, "Every record
// is created once"). It carries closed words, ids, digests and bounded values,
// never a token, a credential, a person's personal data or published text.

// GrowthAnswerer is who answered a held growth call.
type GrowthAnswerer string

// The answerers.
const (
	// GrowthByPerson is an administrator, in person at a step-up.
	GrowthByPerson GrowthAnswerer = "person"
	// GrowthByEngine is the engine's lapse of an ask nobody answered.
	GrowthByEngine GrowthAnswerer = "engine"
)

// growthSpace is the UUIDv5 namespace a growth record's id is derived in. FIXED
// for the life of the format: a new one would give a step retried across an
// upgrade a second record.
var growthSpace = uuid.MustParse("2b8f6d41-73c5-5e09-9a1d-c4e7f0b3a268")

// growthPhase is the one record of an act a step takes: a held call's ask, its
// answer (allowed, denied or lapsed, the first written) and its outcome; a call
// made at once has its outcome alone.
func growthPhase(step growth.Step) string {
	switch step {
	case growth.StepAsked:
		return "ask"
	case growth.StepAllowed, growth.StepDenied, growth.StepLapsed:
		return "answer"
	case growth.StepMade, growth.StepApplied, growth.StepVoid, growth.StepFailed:
		return "outcome"
	}
	return string(step)
}

// GrowthID is the id of the record of step of the act named act (a held call's
// id, or a call made at once's): derived from the act and the step's phase, so
// an act holds one answer and one outcome however many nodes write them.
func GrowthID(act string, step growth.Step) string {
	return uuid.NewSHA1(growthSpace, []byte(act+"/"+growthPhase(step))).String()
}

// GrowthOpID is the operation the record of step of act is published under. It
// names the step, not its phase: a lapse and an allow minted at one instant are
// two operations, so the second answers [ErrRecorded] rather than the first's
// result.
func GrowthOpID(minted time.Time, handle, act string, step growth.Step) string {
	return statelog.DeriveOpID(minted, "access-growth", "crewlet.access", handle, act, string(step))
}

// GrowthSubject names one growth record by its seat and its own id.
func GrowthSubject(handle, id string) Subject {
	return Subject{Kind: KindGrowth, ID: handle + "." + id}
}

// GrowthChange is one field a growth act changes. A published text's values
// are kept as their SHA-256 and length alone ([PublishedText]).
type GrowthChange struct {
	Field  string `json:"field"`
	Label  string `json:"label,omitempty"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Money  bool   `json:"money,omitempty"`
	Text   bool   `json:"text,omitempty"`

	BeforeSHA256 string `json:"before_sha256,omitempty"`
	BeforeBytes  int    `json:"before_bytes,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	AfterBytes   int    `json:"after_bytes,omitempty"`
}

// PublishedText is a change of words an act publishes, as the log keeps it:
// each value's SHA-256 and length, "" and zero for a value that is not there.
func PublishedText(field, label, before, after string) GrowthChange {
	c := GrowthChange{Field: field, Label: label, Text: true}
	c.BeforeSHA256, c.BeforeBytes = textDigest(before)
	c.AfterSHA256, c.AfterBytes = textDigest(after)
	return c
}

// textDigest is a text's SHA-256 in lowercase hex and its length in bytes.
func textDigest(text string) (string, int) {
	if text == "" {
		return "", 0
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), len(text)
}

// GrowthMoney is a spend's figures in micros of Currency: the change, the
// day's spend before and after it, and the caps it was held to.
type GrowthMoney struct {
	Currency           string `json:"currency"`
	DeltaMicros        int64  `json:"delta_micros,omitempty"`
	DailyMicros        int64  `json:"daily_micros,omitempty"`
	DailyAfterMicros   int64  `json:"daily_after_micros,omitempty"`
	PerChangeCapMicros int64  `json:"per_change_cap_micros,omitempty"`
	DailyCapMicros     int64  `json:"daily_cap_micros,omitempty"`
}

// Growth is one step of a growth act.
type Growth struct {
	V int `json:"v"`

	// ID is the record's own id, [GrowthID] of its act and step, and Handle
	// the agent seat that called: the record's key.
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Turn   string `json:"turn,omitempty"`

	// Call names a call made at once, HeldCall a held call: exactly one.
	Call     string `json:"call,omitempty"`
	HeldCall string `json:"held_call,omitempty"`

	Product    string       `json:"product"`
	Connection string       `json:"connection"`
	Account    string       `json:"account"`
	Target     string       `json:"target,omitempty"`
	Tool       string       `json:"tool"`
	Class      growth.Class `json:"class"`
	Step       growth.Step  `json:"step"`

	// AnsweredBy and Answerer are who answered a held call: a person and
	// their human seat, or the engine's lapse with no seat.
	AnsweredBy GrowthAnswerer `json:"answered_by,omitempty"`
	Answerer   string         `json:"answerer,omitempty"`

	// Via is the vendor role of the credential the call went through, for a
	// product holding several (growth.Record.Via).
	Via string `json:"via,omitempty"`

	// Signer is whose identity signed the call (growth.Record.Signer).
	Signer growth.Signer `json:"signer,omitempty"`

	// Operation and Before are SHA-256 digests in lowercase hex of the exact
	// vendor operation and the target's before-state.
	Operation string `json:"operation"`
	Before    string `json:"before,omitempty"`

	Changes []GrowthChange `json:"changes,omitempty"`
	Money   *GrowthMoney   `json:"money,omitempty"`
	Failure growth.Failure `json:"failure,omitempty"`
}

// act is the id that names the record's act.
func (g Growth) act() string {
	if g.HeldCall != "" {
		return g.HeldCall
	}
	return g.Call
}

// ---- writing ------------------------------------------------------------ //

// NewGrowth is one step of a growth act, as the writer records it. A published
// text arrives as its digests ([PublishedText]), never in the clear.
type NewGrowth struct {
	Handle, Turn   string
	Call, HeldCall string

	Product, Connection, Account, Target, Tool string

	Class growth.Class
	Step  growth.Step

	// Answerer is the human seat that allowed or denied a held call, "" for
	// every other step.
	Answerer string
	// Via is the vendor role of the credential the call went through.
	Via string
	// Signer is whose identity signed the call.
	Signer growth.Signer

	Operation, Before string

	Changes []GrowthChange
	Money   *GrowthMoney
	Failure growth.Failure
}

// The growth record's patterns, compiled at first use for the reason the
// package's others are.
var (
	growthProductPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	})
	growthToolPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	})
	// growthConnectionPattern is internal/config's connection id.
	growthConnectionPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	})
	// growthObjectPattern is a vendor's id for an account or an object:
	// `act_1234`, `123-456-7890`, a UUID, `accounts/pub-1`.
	growthObjectPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
	})
	growthFieldPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\[\]-]{0,127}$`)
	})
	emailPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`[^\s@<>()"',;]+@[^\s@<>()"',;]+\.[A-Za-z]{2,}`)
	})
)

// personalWords and credentialWords are what a changed field's name may not
// hold, as words of its snake or camel case: a person's own data, which the log
// counts and never keeps, and a credential, which it never sees.
var (
	personalWords = []string{"email", "emails", "phone", "telephone", "mobile", "birthday", "birthdate",
		"reviewer", "reviewers", "tester", "testers", "first_name", "last_name", "full_name", "given_name",
		"family_name", "street", "postal_code", "zip_code", "ip_address"}
	credentialWords = []string{"token", "tokens", "secret", "secrets", "credential", "credentials",
		"api_key", "access_key", "private_key", "signing_key", "client_secret", "authorization", "cookie"}
)

// nameWords is a field name as its lowercase words: `adsetDailyBudget` and
// `adset.daily_budget` both read adset, daily, budget.
func nameWords(name string) []string {
	var b strings.Builder
	prev := rune(0)
	for _, r := range name {
		switch {
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			b.WriteRune('_')
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune('_')
		}
		prev = r
	}
	return strings.FieldsFunc(b.String(), func(r rune) bool { return r == '_' })
}

// namesAny reports whether name's words hold one of list's, each of which may
// be two words joined by an underscore.
func namesAny(name string, list []string) (string, bool) {
	ws := nameWords(name)
	for i, w := range ws {
		for _, word := range list {
			if w == word || (i+1 < len(ws) && w+"_"+ws[i+1] == word) {
				return word, true
			}
		}
	}
	return "", false
}

// checkGrowthWords refuses a value a growth record may not keep: a credential's
// shape (internal/redact's one list of them) or a person's email address.
func checkGrowthWords(field, value string) error {
	switch {
	case redact.Contains(value):
		return invalid("%s holds what reads as a credential: a growth record never carries one", field)
	case emailPattern().MatchString(value):
		return invalid("%s holds an email address: a growth record keeps no person's personal data, "+
			"so count the people instead", field)
	}
	return nil
}

// checkGrowthObject refuses a vendor id outside its shape or holding a credential.
func checkGrowthObject(field, value string, required bool) error {
	switch {
	case value == "" && !required:
		return nil
	case !growthObjectPattern().MatchString(value):
		// NOT ECHOED: a value refused here may be the credential the record must not carry.
		return invalid("%s is not a vendor's id: letters, digits, and _ . : / -, at most 128", field)
	}
	return checkGrowthWords(field, value)
}

// GrowthOf is a product's record of step as the log takes it, each published
// text as its SHA-256 and length: the words never reach the log.
func GrowthOf(step growth.Step, r growth.Record) NewGrowth {
	n := NewGrowth{
		Handle: r.Handle, Turn: r.Turn, Call: r.Call, HeldCall: r.HeldCall, Product: r.Product,
		Connection: r.Connection, Account: r.Account, Target: r.Target, Tool: r.Tool, Class: r.Class,
		Step: step, Answerer: r.Answerer, Via: r.Via, Signer: r.Signer, Operation: r.Operation, Before: r.Before, Failure: r.Failure,
	}
	for _, c := range r.Changes {
		if c.Text {
			n.Changes = append(n.Changes, PublishedText(c.Field, c.Label, c.Before, c.After))
			continue
		}
		n.Changes = append(n.Changes, GrowthChange{Field: c.Field, Label: c.Label, Before: c.Before, After: c.After,
			Money: c.Money})
	}
	if m := r.Money; m != nil {
		n.Money = &GrowthMoney{Currency: m.Currency, DeltaMicros: m.DeltaMicros, DailyMicros: m.DailyMicros,
			DailyAfterMicros: m.DailyAfterMicros, PerChangeCapMicros: m.PerChangeCapMicros,
			DailyCapMicros: m.DailyCapMicros}
	}
	return n
}

// GrowthUnrecordedField is the change that counts the fields a record leaves
// out ([RecordableGrowthChanges]).
const GrowthUnrecordedField = "unrecordedFields"

// RecordableGrowthChanges is changes as the log takes them: a field it may not
// name (a person's data, a credential), or one past [MaxGrowthChanges], is
// left out and counted in one last change, so a product's record of an act
// lands whatever the vendor's fields are called.
func RecordableGrowthChanges(changes []growth.RecordChange) []growth.RecordChange {
	out := make([]growth.RecordChange, 0, len(changes))
	left := 0
	for _, c := range changes {
		if !GrowthFieldRecordable(c.Field) || len(out) == MaxGrowthChanges-1 {
			left++
			continue
		}
		out = append(out, c)
	}
	if left > 0 {
		out = append(out, growth.RecordChange{Field: GrowthUnrecordedField, Label: "Fields left out",
			After: strconv.Itoa(left)})
	}
	return out
}

// GrowthFieldRecordable reports whether a growth record may name field: a
// vendor field's path that names neither a person's data nor a credential.
func GrowthFieldRecordable(field string) bool { return checkGrowthField(field) == nil }

// GrowthValueRecordable reports whether a growth record may keep value in the
// clear: within its bound, holding neither a credential nor an email address.
func GrowthValueRecordable(value string) bool { return checkGrowthValue("a value", value) == nil }

// GrowthIDRecordable reports whether a growth record may name value as an
// account or a target.
func GrowthIDRecordable(value string) bool { return checkGrowthObject("an id", value, true) == nil }

// Validate refuses a growth record the log could not hold or must not keep:
// what [Writer.Growth] refuses before it writes.
func (n NewGrowth) Validate() error {
	if err := checkSeat(n.Handle); err != nil {
		return err
	}
	for _, ref := range []struct{ field, value string }{
		{"the turn", n.Turn}, {"the call", n.Call}, {"the held call", n.HeldCall}} {
		if err := checkRef(ref.field, ref.value, false); err != nil {
			return err
		}
	}
	held := n.HeldCall != ""
	switch {
	case held == (n.Call != ""):
		return invalid("a growth record names its act once: the call made at once, or the held call")
	case !growthProductPattern().MatchString(n.Product):
		return invalid("product %q is not an integration kind", n.Product)
	case !growthConnectionPattern().MatchString(n.Connection):
		return invalid("connection %q is not a connection id", n.Connection)
	case !growthToolPattern().MatchString(n.Tool):
		return invalid("tool %q is not a tool's name", n.Tool)
	case n.Via != "" && !growthProductPattern().MatchString(n.Via):
		// NOT ECHOED: a value refused here may be the credential itself.
		return invalid("via is a vendor role's word: lowercase letters, digits and underscores, never a credential")
	case !n.Signer.Valid():
		return invalid("signer %q is not own or company", n.Signer)
	case !n.Class.Callable():
		return invalid("class %q is not one a call is made in", n.Class)
	case !n.Step.Valid():
		return invalid("step %q is not one of %s", n.Step, words.Or(growth.Steps()))
	case !shaPattern().MatchString(n.Operation):
		return invalid("the operation's digest is SHA-256 in lowercase hex")
	case n.Before != "" && !shaPattern().MatchString(n.Before):
		return invalid("the before-state's digest is SHA-256 in lowercase hex")
	case len(n.Changes) > MaxGrowthChanges:
		return invalid("a growth record names %d changes and the maximum is %d", len(n.Changes), MaxGrowthChanges)
	}
	if err := n.checkStep(held); err != nil {
		return err
	}
	if err := checkGrowthObject("the account", n.Account, true); err != nil {
		return err
	}
	if err := checkGrowthObject("the target", n.Target, false); err != nil {
		return err
	}
	for _, c := range n.Changes {
		if err := c.validate(); err != nil {
			return err
		}
	}
	return n.Money.validate()
}

// checkStep holds the step to its act, its class, its answerer and its failure.
func (n NewGrowth) checkStep(held bool) error {
	switch n.Step {
	case growth.StepMade:
		if held || n.Class.Held() {
			return invalid("made is a read or operate call made at once; a held call is applied")
		}
	case growth.StepFailed:
		if held != n.Class.Held() {
			return invalid("a failed %s call names its act as the class makes it: a held call for public, "+
				"spend and admin, the call made at once for read and operate", n.Class)
		}
	default:
		if !held || !n.Class.Held() {
			return invalid("%s is a step of a held public, spend or admin call", n.Step)
		}
	}
	switch {
	case (n.Step == growth.StepFailed) != (n.Failure != ""):
		return invalid("a failed step, and no other, names its failure")
	case n.Failure != "" && !n.Failure.Valid():
		return invalid("a failure is one of %s: a closed word, never the vendor's text",
			words.Or(growth.Failures()))
	case n.Step == growth.StepAllowed || n.Step == growth.StepDenied:
		if !iam.ValidSeatHandle(n.Answerer) {
			return invalid("an administrator's answer names their human seat's handle, never their login")
		}
	case n.Answerer != "":
		return invalid("only an administrator's allow or deny names who answered; a lapse is the engine's")
	}
	return nil
}

// answeredBy is who answered the step, "" where nobody did.
func (n NewGrowth) answeredBy() GrowthAnswerer {
	switch n.Step {
	case growth.StepAllowed, growth.StepDenied:
		return GrowthByPerson
	case growth.StepLapsed:
		return GrowthByEngine
	}
	return ""
}

// validate refuses a change the log could not hold or must not keep.
func (c GrowthChange) validate() error {
	if err := checkGrowthField(c.Field); err != nil {
		return err
	}
	if err := checkText("a change's label", c.Label, GrowthLabelMaxBytes); err != nil {
		return err
	}
	if strings.ContainsAny(c.Label, "\r\n") {
		return invalid("a change's label is one line")
	}
	if err := checkGrowthWords("field "+c.Field+"'s label", c.Label); err != nil {
		return err
	}
	if c.Text {
		return c.validateText()
	}
	if c.BeforeSHA256 != "" || c.AfterSHA256 != "" || c.BeforeBytes != 0 || c.AfterBytes != 0 {
		return invalid("field %s carries a text's digests and is not marked as text", c.Field)
	}
	for _, v := range []struct{ side, value string }{{"before", c.Before}, {"after", c.After}} {
		if err := checkGrowthValue("field "+c.Field+"'s value "+v.side, v.value); err != nil {
			return err
		}
		if _, err := strconv.ParseInt(v.value, 10, 64); c.Money && v.value != "" && err != nil {
			return invalid("field %s is money, and %q is not integer micros", c.Field, v.value)
		}
	}
	return nil
}

// checkGrowthField refuses a changed field's name the log may not keep.
func checkGrowthField(field string) error {
	if !growthFieldPattern().MatchString(field) {
		return invalid("a changed field is a vendor field's path: letters, digits, and _ . [ ] -, at most 128")
	}
	if word, ok := namesAny(field, personalWords); ok {
		return invalid("field %s names a person's %s: a growth record keeps no personal data, so count "+
			"the people instead", field, word)
	}
	if word, ok := namesAny(field, credentialWords); ok || redact.NamesPassword(field) {
		return invalid("field %s names a credential (%s): a growth record never carries one", field, word)
	}
	return nil
}

// checkGrowthValue refuses a value the log may not keep in the clear.
func checkGrowthValue(name, value string) error {
	if err := checkText(name, value, GrowthValueMaxBytes); err != nil {
		return err
	}
	return checkGrowthWords(name, value)
}

// validateText holds a published text to its digests: its words never reach
// the log.
func (c GrowthChange) validateText() error {
	switch {
	case c.Before != "" || c.After != "":
		return invalid("field %s is published text, which a growth record keeps as its SHA-256 and "+
			"length, never in the clear", c.Field)
	case c.Money:
		return invalid("field %s is text and money at once", c.Field)
	case c.BeforeSHA256 == "" && c.AfterSHA256 == "":
		return invalid("published text %s names the digest of what it was or what it is", c.Field)
	}
	for _, d := range []struct {
		side, digest string
		bytes        int
	}{{"before", c.BeforeSHA256, c.BeforeBytes}, {"after", c.AfterSHA256, c.AfterBytes}} {
		switch {
		case d.digest != "" && !shaPattern().MatchString(d.digest):
			return invalid("published text %s's digest %s is SHA-256 in lowercase hex", c.Field, d.side)
		case d.bytes < 0 || (d.digest == "") != (d.bytes == 0):
			return invalid("published text %s's length %s goes with its digest", c.Field, d.side)
		}
	}
	return nil
}

// validate refuses money the log could not read back exactly; nil is none.
func (m *GrowthMoney) validate() error {
	switch {
	case m == nil:
		return nil
	case !growth.ValidCurrency(m.Currency):
		return invalid("currency %q is not an ISO 4217 code", m.Currency)
	case m.DailyMicros < 0 || m.DailyAfterMicros < 0 || m.PerChangeCapMicros < 0 || m.DailyCapMicros < 0:
		return invalid("a day's spend and a cap are never negative")
	}
	return nil
}

// Growth records one step of a growth act, once: a second record of the act's
// same phase (its answer or its outcome, from any node) answers [ErrRecorded]
// and changes nothing, so a lapse racing an administrator's allow keeps the
// first; a retry of the same operation is answered as the first's.
func (w *Writer) Growth(ctx context.Context, c Call, n NewGrowth) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := n.Validate(); err != nil {
		return statelog.Result{}, err
	}
	act := n.HeldCall
	if act == "" {
		act = n.Call
	}
	var money *GrowthMoney
	if n.Money != nil {
		m := *n.Money
		money = &m
	}
	p := Growth{
		V: baseRecordVersion, ID: GrowthID(act, n.Step), Handle: n.Handle, Turn: n.Turn, Call: n.Call,
		HeldCall: n.HeldCall, Product: n.Product, Connection: n.Connection, Account: n.Account,
		Target: n.Target, Tool: n.Tool, Class: n.Class, Step: n.Step, AnsweredBy: n.answeredBy(),
		Answerer: n.Answerer, Via: n.Via, Signer: n.Signer, Operation: n.Operation, Before: n.Before,
		Changes: append([]GrowthChange(nil), n.Changes...), Money: money, Failure: n.Failure,
	}
	subject, scope := GrowthSubject(n.Handle, p.ID), ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpGrowth, scope, c.OpID, p, w.now())
		}))
}

// ---- reading ------------------------------------------------------------ //

// GrowthRow is one growth record as it was written.
type GrowthRow struct {
	Handle, ID, Turn, Call, HeldCall           string
	Product, Connection, Account, Target, Tool string

	Class      growth.Class
	Step       growth.Step
	AnsweredBy GrowthAnswerer
	Answerer   string
	Via        string
	Signer     growth.Signer

	Operation, Before string
	Changes           []GrowthChange
	// Money is nil for a step that spends nothing.
	Money   *GrowthMoney
	Failure growth.Failure

	At       time.Time
	Position statelog.Position
}

// Act is the id that names the row's act: its held call's, or its call's.
func (g GrowthRow) Act() string {
	if g.HeldCall != "" {
		return g.HeldCall
	}
	return g.Call
}

// growthColumns is every column a [GrowthRow] is read from, in [scanGrowth]'s order.
const growthColumns = `agent_handle, growth_id, turn_id, call_id, held_call, product, connection, account,
	target, tool, class, step, answered_by, answerer, via, operation_digest, before_digest, changes, currency,
	delta_micros, daily_micros, daily_after_micros, per_change_cap_micros, daily_cap_micros, failure, at,
	position, signer`

// scanGrowth reads one growth row.
func scanGrowth(row rowScanner) (GrowthRow, error) {
	var (
		g                                      GrowthRow
		class, step, answeredBy, changes, fail string
		signer                                 string
		m                                      GrowthMoney
		at, posn                               int64
	)
	if err := row.Scan(&g.Handle, &g.ID, &g.Turn, &g.Call, &g.HeldCall, &g.Product, &g.Connection, &g.Account,
		&g.Target, &g.Tool, &class, &step, &answeredBy, &g.Answerer, &g.Via, &g.Operation, &g.Before, &changes,
		&m.Currency, &m.DeltaMicros, &m.DailyMicros, &m.DailyAfterMicros, &m.PerChangeCapMicros,
		&m.DailyCapMicros, &fail, &at, &posn, &signer); err != nil {
		return GrowthRow{}, err
	}
	if err := json.Unmarshal([]byte(changes), &g.Changes); err != nil {
		return GrowthRow{}, fmt.Errorf("accessdomain: growth record %s names its changes as %w", g.ID, err)
	}
	g.Signer = growth.Signer(signer)
	if m.Currency != "" {
		g.Money = &m
	}
	g.Class, g.Step, g.AnsweredBy, g.Failure = growth.Class(class), growth.Step(step),
		GrowthAnswerer(answeredBy), growth.Failure(fail)
	g.At, g.Position = store.DecodeTime(at), statelog.Unpack(Domain{}.Stream().Name, posn)
	return g, nil
}

// GrowthQuery narrows the page of recent agent changes. Every field is optional.
type GrowthQuery struct {
	// Product is one integration kind's; empty is every product's.
	Product string

	Cursor string
	Limit  int
}

// GrowthPage is one page of growth records, newest first.
type GrowthPage struct {
	Rows     []GrowthRow
	Next     string
	Position statelog.Position
	Complete bool
}

// GrowthChanges pages the recent agent changes, newest first: every step of a
// call that was not a read (an operate call made at once, made or failed, and
// every step of a held call), for one product or all.
func (r *Reader) GrowthChanges(ctx context.Context, q GrowthQuery,
	fresh statelog.Freshness) (GrowthPage, error) {

	if q.Product != "" && !growthProductPattern().MatchString(q.Product) {
		return GrowthPage{}, invalid("product %q is not an integration kind", q.Product)
	}
	before, limit, err := page(q.Cursor, q.Limit)
	if err != nil {
		return GrowthPage{}, err
	}
	// THE PARTIAL INDEXES' OWN PREDICATE, spelt as the migration does, so the
	// store admits the index the statement names.
	statement := `SELECT ` + growthColumns + ` FROM access_growth INDEXED BY access_growth_changes_idx
		WHERE class != 'read' AND position < ? ORDER BY position DESC LIMIT ?`
	args := []any{before, limit + 1}
	if q.Product != "" {
		statement = `SELECT ` + growthColumns + ` FROM access_growth INDEXED BY access_growth_product_idx
			WHERE class != 'read' AND product = ? AND position < ? ORDER BY position DESC LIMIT ?`
		args = []any{q.Product, before, limit + 1}
	}
	var out GrowthPage
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = GrowthPage{}
		rows, queryErr := tx.QueryContext(ctx, statement, args...)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			g, scanErr := scanGrowth(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Rows = append(out.Rows, g)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return GrowthPage{}, fmt.Errorf("accessdomain: page the growth changes: %w", err)
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		out.Next = cursorOf(out.Rows[limit-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// GrowthCount names the changes a vendor's rate rule counts on one target.
type GrowthCount struct {
	// Product, Account and Target name the target, all three required.
	Product, Account, Target string

	// Handle narrows to one seat's changes; empty counts every seat's, as a
	// vendor counts them.
	Handle string

	// Since is the earliest broker instant counted, required.
	Since time.Time
}

// GrowthChanged counts the changes to one target that took effect at the
// vendor since an instant: allowed calls applied, and operate calls made at
// once. What a product holds to a vendor's rate rule (Meta's budget changes per
// ad set an hour) before it calls.
func (r *Reader) GrowthChanged(ctx context.Context, q GrowthCount, fresh statelog.Freshness) (int, error) {
	switch {
	case !growthProductPattern().MatchString(q.Product):
		return 0, invalid("product %q is not an integration kind", q.Product)
	case q.Since.IsZero():
		return 0, invalid("a count of changes says since when")
	}
	if err := checkGrowthObject("the account", q.Account, true); err != nil {
		return 0, err
	}
	if err := checkGrowthObject("the target", q.Target, true); err != nil {
		return 0, err
	}
	scope := DomainScope()
	if q.Handle != "" {
		if err := checkSeat(q.Handle); err != nil {
			return 0, err
		}
		scope = SeatScope(q.Handle)
	}
	var n int
	_, err := r.log.Read(ctx, fresh.Query(scope, true), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM access_growth INDEXED BY access_growth_target_idx
			WHERE product = ? AND account = ? AND target = ? AND at >= ?
			  AND (step = 'applied' OR (step = 'made' AND class = 'operate'))
			  AND (? = '' OR agent_handle = ?)`,
			q.Product, q.Account, q.Target, store.EncodeTime(q.Since), q.Handle, q.Handle).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("accessdomain: count the changes to %s's %s: %w", q.Product, q.Target, err)
	}
	return n, nil
}
