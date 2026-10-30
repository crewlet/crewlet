package accessmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("accessmigrate")

// Document is the company document a migration reads and writes: the config
// surface's own ([configapi.Service.StoredDocument], [configapi.Service.Migrate]),
// so the revision it writes is validated, sealed, attributed and activated as
// every other.
type Document interface {
	// Stored is the fleet's revision as stored, as a tree, and its id.
	Stored(ctx context.Context) (root map[string]any, revision string, err error)

	// Rewrite activates transform's rewrite of the revision expect names as
	// one revision, answering its id, or reports that transform changed
	// nothing.
	Rewrite(ctx context.Context, expect, summary string, by iam.Actor,
		transform func(root map[string]any) (bool, error)) (revision string, changed bool,
		err error)
}

// Secrets is where a seat's credential is read under its old name and sealed
// under the engine's.
type Secrets interface {
	// Value is a variable's value through the engine's own chain, and
	// whether it resolved.
	Value(name string) (string, bool)

	// Seal stores a value under name in the fleet's store.
	Seal(ctx context.Context, name, value string, by secrets.Author, now time.Time) error
}

// Records is where the migration seeds the ledger and records the seats that
// shared a company credential.
type Records interface {
	// Ledger is the fleet's record of the accounts the engine created.
	Ledger() seataccess.Ledger
}

// Deps is what a migration runs on.
type Deps struct {
	Document Document
	Secrets  Secrets
	Records  Records

	// Open opens a vendor's session on the company the document decodes to,
	// for the accounts the engine made before the ledger and the old company
	// roles' ids ([Read]).
	Open func(ctx context.Context, kind integration.Kind, in seataccess.OpenInput) (
		seataccess.Session, error)

	// Env resolves the document's `${VAR}` references for a vendor session.
	Env *config.Resolver

	// Managed is a deployment whose company document only named systems
	// write ([config.Company.SeatCredentialMode]).
	Managed bool

	Now func() time.Time
}

// vendorDeadline bounds what one vendor is asked during a migration: a vendor
// that does not answer in time leaves its integration gated, and the next run
// asks again.
const vendorDeadline = 30 * time.Second

// Actor is the author every migration revision, sealed value and ledger row
// records: the engine, migrating the company for it.
func Actor() iam.Actor {
	return iam.Actor{Name: "seat access migration", Kind: iam.ActorSystem}
}

// Result is what one run did.
type Result struct {
	// Revision is the revision written, empty when none was needed.
	Revision string
	Moved    int
	Notices  int
	Seeded   int

	// Migrated are the integrations the revision marked, and Pending the
	// ones left gated, with why.
	Migrated []integration.Kind
	Pending  map[integration.Kind]error
}

// ErrPlanMoved reports a document that changed between the plan and the
// write in a way that moves different credentials: the run stops before
// writing, and the next one plans again.
var ErrPlanMoved = errors.New("accessmigrate: the document changed while it was being migrated")

// Run migrates the fleet's company, every integration it can, in the one order
// that is safe: read the vendors for the integrations left to migrate, plan
// from the stored document, seal every credential the plan moves under its
// engine name, record the accounts the engine made before the ledger and the
// seats that shared a company credential, and only then write the revision
// that marks each migrated integration, held to the revision planned on.
// Every step is idempotent, so a run that stops part way is finished by the
// next, and a document with nothing left to migrate returns at once.
//
// THE VALUES ARE MOVED AND THE LEDGER SEEDED BEFORE THE REVISION, never after:
// the revision is what opens an integration's gate, and a pass let through
// before the seat's credential is sealed, or before its account is recorded,
// would mint a second credential or create a second account. Done first, the
// old names still hold their values and the gate is still shut, so a run that
// stops between the two leaves the company running exactly as before.
func Run(ctx context.Context, d Deps) (Result, error) {
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	root, revision, err := d.Document.Stored(ctx)
	if err != nil {
		return Result{}, err
	}
	if !Needed(root) {
		return Result{}, nil
	}
	facts, err := Read(ctx, root, d.Open, d.Env, d.Managed)
	if err != nil {
		return Result{}, err
	}
	plan, err := Plan(root, facts)
	if err != nil {
		return Result{}, err
	}
	result := Result{Pending: plan.Pending, Migrated: plan.Migrated}
	for kind, why := range plan.Pending {
		log.WarnContext(ctx, "seat_access_migration_pending", "kind", kind,
			"error", why.Error(), "detail", "the integration's seats stay as they are and "+
				"its access pass waits; the migration is tried again")
	}
	if plan.Empty() {
		return result, nil
	}
	author := secrets.Author{Name: Actor().Name, Kind: string(Actor().Kind)}
	for _, m := range plan.Moves {
		value, ok := d.Secrets.Value(m.From)
		if !ok || strings.TrimSpace(value) == "" {
			// NOTHING TO MOVE: the seat named a variable that holds no
			// value, so its next sync mints a credential under the
			// engine's name as for a new seat.
			log.InfoContext(ctx, "seat_access_credential_unset", "seat", m.Handle,
				"kind", m.Kind, "variable", m.From)
			continue
		}
		if sealErr := d.Secrets.Seal(ctx, m.To, value, author, now()); sealErr != nil {
			return result, fmt.Errorf("accessmigrate: seal %s for %s: %w", m.To, m.Handle, sealErr)
		}
		result.Moved++
	}
	if result.Seeded, err = seed(ctx, d.Records.Ledger(), plan.Seeds, plan.Moves,
		now()); err != nil {
		return result, err
	}
	if err = recordShared(ctx, d.Records.Ledger(), plan.Notices, now()); err != nil {
		return result, err
	}
	result.Notices = len(plan.Notices)
	written, changed, err := d.Document.Rewrite(ctx, revision, migrationSummary(plan),
		Actor(), func(root map[string]any) (bool, error) {
			again, planErr := Plan(root, facts)
			if planErr != nil {
				return false, planErr
			}
			if !sameMoves(again.Moves, plan.Moves) ||
				!slices.Equal(again.Migrated, plan.Migrated) {
				return false, ErrPlanMoved
			}
			return Apply(root, again)
		})
	if err != nil {
		return result, err
	}
	if changed {
		result.Revision = written
	}
	log.InfoContext(ctx, "seat_access_migrated", "revision", result.Revision,
		"migrated", fmt.Sprint(plan.Migrated), "moved", result.Moved,
		"notices", result.Notices, "seeded", result.Seeded)
	return result, nil
}

// Read asks each vendor the migration needs for the integrations left to
// migrate, where the engine provisions them: the accounts it made before the
// ledger, found by the engine's own naming, and, through the same session, the
// old company roles' ids. An integration whose vendor cannot be opened or read
// is [Facts.Unavailable], which leaves it gated.
func Read(ctx context.Context, root map[string]any,
	open func(context.Context, integration.Kind, seataccess.OpenInput) (seataccess.Session, error),
	env *config.Resolver, managed bool) (Facts, error) {

	facts := Facts{Accounts: map[integration.Kind]seataccess.Observation{},
		Unavailable: map[integration.Kind]error{}, Managed: managed}
	company, err := companyOf(root)
	if err != nil {
		return facts, err
	}
	roster, err := company.Organization()
	if err != nil {
		return facts, fmt.Errorf("accessmigrate: the document derives no organization: %w", err)
	}
	var seats []seataccess.SeatRef
	for _, seat := range seataccess.SeatsOf(company) {
		seats = append(seats, seat.SeatRef)
	}
	sessions := map[integration.Kind]seataccess.Session{}
	for _, kind := range Unmigrated(root) {
		if company.SeatCredentialMode(string(kind), managed) != config.SeatCredentialEngine {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, vendorDeadline)
		session, err := open(readCtx, kind, seataccess.OpenInput{Config: company,
			Org: roster, Env: env})
		if err == nil {
			var seen seataccess.Observation
			if seen, err = session.Observe(readCtx, seataccess.ObserveInput{Seats: seats,
				Records: map[string]seataccess.Record{}}); err == nil {
				facts.Accounts[kind] = seen
				sessions[kind] = session
			}
		}
		cancel()
		if err != nil {
			facts.Unavailable[kind] = fmt.Errorf("accessmigrate: read the accounts at %s: %w",
				vendorName(kind), err)
		}
	}
	facts.Levels = func(lane seataccess.Lane, name string) (string, bool, error) {
		session, ok := sessions[lane.Kind()]
		if !ok {
			return "", false, fmt.Errorf("%s could not be read", vendorName(lane.Kind()))
		}
		readCtx, cancel := context.WithTimeout(ctx, vendorDeadline)
		defer cancel()
		opts, err := session.Options(readCtx, lane)
		if err != nil {
			return "", false, err
		}
		for _, level := range opts.Levels {
			if level.ID == name || strings.EqualFold(level.Name, name) {
				return level.ID, true, nil
			}
		}
		return "", false, nil
	}
	return facts, nil
}

// seed records each account the engine made before the ledger, where the
// ledger holds no record for its seat yet, with what the vendor showed for its
// lanes and the credential the migration sealed for it.
func seed(ctx context.Context, ledger seataccess.Ledger, seeds []Seed, moves []Move,
	now time.Time) (int, error) {

	n := 0
	byKind := map[integration.Kind]map[string]seataccess.Record{}
	for _, s := range seeds {
		records, ok := byKind[s.Kind]
		if !ok {
			var err error
			if records, err = ledger.Records(ctx, s.Kind); err != nil {
				return n, fmt.Errorf("accessmigrate: read the %s ledger: %w", s.Kind, err)
			}
			byKind[s.Kind] = records
		}
		if held, ok := records[s.Handle]; ok && held.Account != nil {
			continue
		}
		rec := records[s.Handle]
		rec.Kind, rec.Handle, rec.Name = s.Kind, s.Handle, s.Name
		account := s.Account
		if account.CreatedAt.IsZero() {
			account.CreatedAt = now
		}
		rec.Account = &account
		if len(s.Lanes) > 0 {
			rec.Lanes = map[seataccess.Lane]seataccess.LaneRecord{}
			for lane, seen := range s.Lanes {
				rec.Lanes[lane] = seataccess.LaneRecord{Observed: seen}
			}
		}
		for _, m := range moves {
			if m.Kind == s.Kind && m.Handle == s.Handle && rec.Credential == nil {
				rec.Credential = &seataccess.Credential{Variable: m.To, MintedAt: now,
					By: Actor().Name, Source: seataccess.CredentialMinted}
			}
		}
		if err := ledger.Put(ctx, rec); err != nil {
			return n, fmt.Errorf("accessmigrate: record %s's %s account: %w", s.Handle,
				s.Kind, err)
		}
		n++
	}
	return n, nil
}

// recordShared records, on each seat's ledger record, the company credential
// it shared, which the Agents tab's danger notice is drawn from until that
// credential is replaced ([SharedNotice]). A seat already carrying the record
// keeps the instant it was first raised.
func recordShared(ctx context.Context, ledger seataccess.Ledger, notices []Notice,
	now time.Time) error {

	for _, n := range notices {
		records, err := ledger.Records(ctx, n.Kind)
		if err != nil {
			return fmt.Errorf("accessmigrate: read the %s ledger: %w", n.Kind, err)
		}
		for _, handle := range n.Seats {
			rec, held := records[handle]
			if !held {
				rec = seataccess.Record{Kind: n.Kind, Handle: handle}
			}
			if rec.SharedCredential != nil && rec.SharedCredential.Variable == n.Variable {
				continue
			}
			rec.SharedCredential = &seataccess.SharedCredential{Variable: n.Variable,
				RaisedAt: now}
			if err := ledger.Put(ctx, rec); err != nil {
				return fmt.Errorf("accessmigrate: record that %s shared %s: %w", handle,
					n.Variable, err)
			}
		}
	}
	return nil
}

// Written is when the fleet's store last wrote a variable's value, and whether
// it holds one: what tells a shared company credential's finding that it has
// been replaced since. Never the value.
type Written func(ctx context.Context, name string) (at time.Time, held bool, err error)

// SharedFindings are kind's [integration.FindingCredentialShared] findings: one
// per company credential a seat once kept its own credential in (D12), as the
// migration recorded on the seats' ledger records, until the variable is
// rewritten after the finding was raised. names are the seats' names, by
// handle, for the sentence; a seat with none is named by its handle.
//
// The pass raises them on the integration's status, and the Agents tab draws
// the first as its danger notice ([NoticeOf]), so the two say one thing.
func SharedFindings(ctx context.Context, kind integration.Kind,
	records map[string]seataccess.Record, names map[string]string,
	written Written) ([]integration.Finding, error) {

	seats := map[string][]string{}
	raised := map[string]time.Time{}
	handles := make([]string, 0, len(records))
	for handle := range records {
		handles = append(handles, handle)
	}
	slices.Sort(handles)
	for _, handle := range handles {
		shared := records[handle].SharedCredential
		if shared == nil {
			continue
		}
		name := names[handle]
		if name == "" {
			name = handle
		}
		seats[shared.Variable] = append(seats[shared.Variable], name)
		if shared.RaisedAt.After(raised[shared.Variable]) {
			raised[shared.Variable] = shared.RaisedAt
		}
	}
	var out []integration.Finding
	for _, variable := range slices.Sorted(maps.Keys(seats)) {
		at, held, err := written(ctx, variable)
		if err != nil {
			return nil, fmt.Errorf("accessmigrate: when %s was last written: %w", variable, err)
		}
		if held && at.After(raised[variable]) {
			continue
		}
		vendor := vendorName(kind)
		out = append(out, integration.Finding{
			Kind: integration.FindingCredentialShared, Subject: variable,
			Subjects: seats[variable],
			Detail: fmt.Sprintf("%s kept its own %s credential in %s, which a company "+
				"setting also uses, so %s may hold that agent's value", strings.Join(
				seats[variable], ", "), vendor, variable, variable),
			Remedy: fmt.Sprintf("Replace %s with the company's own credential at %s. "+
				"Crewlet gives the agent a new credential of its own on its next sync",
				variable, vendor),
		})
	}
	return out, nil
}

// NoticeOf is the Agents tab's danger notice for a shared credential's
// finding: its title names what to replace, and its body is the finding's own
// sentence and remedy.
func NoticeOf(kind integration.Kind, f integration.Finding) seataccess.Notice {
	return seataccess.Notice{
		Tone:       "danger",
		Title:      "Replace the company's " + vendorName(kind) + " credential",
		Body:       f.Detail + ". " + f.Remedy + ".",
		ReplaceKey: true,
	}
}

// migrationSummary is the history line the revision records.
func migrationSummary(m Migration) string {
	seats := make([]string, 0, len(m.Seats))
	for handle := range m.Seats {
		seats = append(seats, handle)
	}
	slices.Sort(seats)
	line := "move agents' integration access onto per-seat settings"
	if len(seats) > 0 {
		line += " for " + strings.Join(seats, ", ")
	}
	return line
}

// sameMoves reports two plans moving the same credentials.
func sameMoves(a, b []Move) bool {
	if len(a) != len(b) {
		return false
	}
	for _, m := range a {
		if !slices.Contains(b, m) {
			return false
		}
	}
	return true
}

// vendorName is a kind as its vendor names itself, for a notice.
func vendorName(kind integration.Kind) string {
	switch kind {
	case integration.KindDatadog:
		return "Datadog"
	case integration.KindGitHub:
		return "GitHub"
	case integration.KindGitLab:
		return "GitLab"
	case integration.KindAtlassian:
		return "Atlassian"
	case integration.KindSlack:
		return "Slack"
	case integration.KindMattermost:
		return "Mattermost"
	}
	return string(kind)
}

// companyOf decodes a stored document tree into this build's company.
func companyOf(root map[string]any) (*config.Company, error) {
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("accessmigrate: encode the document: %w", err)
	}
	return config.DecodeCompany(raw)
}
