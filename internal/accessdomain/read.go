package accessdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// Reader answers questions about what seats were told, about the SSH audit and
// about the verdicts on delegated requests from this node's own applied rows,
// through the domain's read authority, at the level each caller asks: a turn's
// dispatch reads its seat's baseline at `linearizable`, since the seat's
// previous turn may have been told on another node; an audit page reads at the
// caller's level.
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority, required: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader
}

// NewReader builds the access read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	switch {
	case opts.DB.IsZero():
		return nil, errors.New("accessdomain: a reader needs the replicated estate")
	case opts.Log == nil:
		return nil, errors.New("accessdomain: a reader needs its domain's read authority")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// DispatchRead is the level a turn's dispatch reads its seat's baseline at.
func DispatchRead() statelog.Freshness {
	return statelog.Freshness{Level: statelog.ReadLinearizable}
}

// stale is the level the engine's own duties read at: a nomination decided
// again in the write's own snapshot, and a census that is a count.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// rowScanner is one row, from a query or a query row.
type rowScanner interface{ Scan(dest ...any) error }

// ---- told --------------------------------------------------------------- //

// ToldRow is what one turn of a seat was told.
type ToldRow struct {
	Handle string
	Turn   string

	Digest   string
	Snapshot json.RawMessage
	Changes  json.RawMessage
	Baseline bool

	RevisionFrom string
	RevisionTo   string
	Authors      []string

	// Latest is the seat's newest told record, its baseline for the next diff.
	Latest bool

	// At is the broker's instant for the record, Position its place on the log.
	At       time.Time
	Position statelog.Position
}

// toldColumns is every column a [ToldRow] is read from, in [scanTold]'s order.
const toldColumns = `agent_handle, turn_id, digest, snapshot, changes, baseline,
	revision_from, revision_to, authors, latest, at, position`

// toldChangesColumns is [toldColumns] with the snapshot left out.
const toldChangesColumns = `agent_handle, turn_id, digest, '' AS snapshot, changes, baseline,
	revision_from, revision_to, authors, latest, at, position`

// scanTold reads one told row.
func scanTold(row rowScanner) (ToldRow, error) {
	var (
		t                         ToldRow
		snapshot, changes, author string
		baseline, latest          int
		at, position              int64
	)
	if err := row.Scan(&t.Handle, &t.Turn, &t.Digest, &snapshot, &changes, &baseline,
		&t.RevisionFrom, &t.RevisionTo, &author, &latest, &at, &position); err != nil {
		return ToldRow{}, err
	}
	if snapshot != "" {
		t.Snapshot = json.RawMessage(snapshot)
	}
	if changes != "" {
		t.Changes = json.RawMessage(changes)
	}
	if author != "" {
		if err := json.Unmarshal([]byte(author), &t.Authors); err != nil {
			return ToldRow{}, fmt.Errorf("accessdomain: %s's turn %s names its authors as %w",
				t.Handle, t.Turn, err)
		}
	}
	t.Baseline, t.Latest = baseline == 1, latest == 1
	t.At, t.Position = store.DecodeTime(at), statelog.Unpack(Domain{}.Stream().Name, position)
	return t, nil
}

// Recent is a seat's baseline and its told records since an instant.
type Recent struct {
	// Baseline is the seat's newest told record, nil where it has none: its
	// next turn is then a baseline turn.
	Baseline *ToldRow

	// Since is every told record at or after the instant asked, newest first,
	// at most [MaxRecentTold]: the changes a seat's older notes may predate,
	// read without their snapshots, which a dispatch does not need.
	Since []ToldRow

	// Position is where the answer is current through, and Complete false
	// where this node holds a record of the seat it could not decode.
	Position statelog.Position
	Complete bool
}

// Recent reads seat handle's baseline and its told records since `since`, in
// one read: what a turn's dispatch diffs against and its memory is checked by.
func (r *Reader) Recent(ctx context.Context, handle string, since time.Time,
	fresh statelog.Freshness) (Recent, error) {

	if err := checkSeat(handle); err != nil {
		return Recent{}, err
	}
	var out Recent
	answer, err := r.log.Read(ctx, fresh.Query(SeatScope(handle), true), func(tx *sql.Tx) error {
		out = Recent{}
		latest, err := scanTold(tx.QueryRowContext(ctx, `SELECT `+toldColumns+`
			FROM access_told INDEXED BY access_told_latest_idx
			WHERE agent_handle = ? AND latest = 1`, handle))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			out.Baseline = &latest
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+toldChangesColumns+`
			FROM access_told INDEXED BY access_told_seat_idx
			WHERE agent_handle = ? ORDER BY position DESC LIMIT ?`, handle, MaxRecentTold)
		if err != nil {
			return err
		}
		for rows.Next() {
			t, scanErr := scanTold(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			if t.At.Before(since) {
				break
			}
			out.Since = append(out.Since, t)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return Recent{}, fmt.Errorf("accessdomain: read what %s was told: %w", handle, err)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ToldPage is one page of a seat's told records, newest first.
type ToldPage struct {
	Rows []ToldRow

	// Next is the cursor of the page after this one, empty on the last.
	Next string

	Position statelog.Position
	Complete bool
}

// Told pages seat handle's told records, newest first, from cursor.
func (r *Reader) Told(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (ToldPage, error) {

	if err := checkSeat(handle); err != nil {
		return ToldPage{}, err
	}
	before, limit, err := page(cursor, limit)
	if err != nil {
		return ToldPage{}, err
	}
	var out ToldPage
	answer, err := r.log.Read(ctx, fresh.Query(SeatScope(handle), true), func(tx *sql.Tx) error {
		out = ToldPage{}
		rows, queryErr := tx.QueryContext(ctx, `SELECT `+toldColumns+`
			FROM access_told INDEXED BY access_told_seat_idx
			WHERE agent_handle = ? AND position < ? ORDER BY position DESC LIMIT ?`,
			handle, before, limit+1)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			t, scanErr := scanTold(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Rows = append(out.Rows, t)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return ToldPage{}, fmt.Errorf("accessdomain: page what %s was told: %w", handle, err)
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		out.Next = cursorOf(out.Rows[limit-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ---- the SSH audit ------------------------------------------------------ //

// AuditQuery narrows an audit page. Every field is optional.
type AuditQuery struct {
	// Handle is one seat, Host one registered host.
	Handle string
	Host   string

	// Since and Until bound the record's broker instant, Until exclusive.
	Since time.Time
	Until time.Time

	Cursor string
	Limit  int
}

// validate refuses a query naming what no record could hold.
func (q AuditQuery) validate() error {
	if q.Handle != "" {
		if err := checkHolder(q.Handle); err != nil {
			return err
		}
	}
	if q.Host != "" && !hostPattern().MatchString(q.Host) {
		return invalid("%q is not a host id", q.Host)
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Until.After(q.Since) {
		return invalid("until is after since")
	}
	return nil
}

// window is the query's instants as stored, the widest where unset.
func (q AuditQuery) window() (since, until int64) { return window(q.Since, q.Until) }

// window is a page's bounds on the broker instant as stored, the widest where
// unset.
func window(from, to time.Time) (since, until int64) {
	since, until = 0, int64(1<<63-1)
	if !from.IsZero() {
		since = store.EncodeTime(from)
	}
	if !to.IsZero() {
		until = store.EncodeTime(to)
	}
	return since, until
}

// CertificateRow is one certificate's record.
type CertificateRow struct {
	Handle     string
	Serial     string
	KeyID      string
	Turn       string
	HeldCall   string
	Purpose    CertificatePurpose
	HostIDs    []string
	Principals []string

	ValidAfter  time.Time
	ValidBefore time.Time

	PublicKeySHA256 string
	CASHA256        string
	Node            string

	At       time.Time
	Position statelog.Position
}

// CertificatePage is one page of certificates, newest first.
type CertificatePage struct {
	Rows     []CertificateRow
	Next     string
	Position statelog.Position
	Complete bool
}

// certificateColumns is every column a [CertificateRow] is read from.
const certificateColumns = `agent_handle, serial, key_id, turn_id, held_call, purpose,
	host_ids, principals, valid_after, valid_before, public_key_sha256, ca_sha256, node,
	at, position`

// scanCertificate reads one certificate row.
func scanCertificate(row rowScanner) (CertificateRow, error) {
	var (
		c                       CertificateRow
		purpose, hosts, princ   string
		after, before, at, posn int64
	)
	if err := row.Scan(&c.Handle, &c.Serial, &c.KeyID, &c.Turn, &c.HeldCall, &purpose,
		&hosts, &princ, &after, &before, &c.PublicKeySHA256, &c.CASHA256, &c.Node,
		&at, &posn); err != nil {
		return CertificateRow{}, err
	}
	if err := json.Unmarshal([]byte(hosts), &c.HostIDs); err != nil {
		return CertificateRow{}, fmt.Errorf("accessdomain: certificate %s names its hosts as %w", c.Serial, err)
	}
	if err := json.Unmarshal([]byte(princ), &c.Principals); err != nil {
		return CertificateRow{}, fmt.Errorf("accessdomain: certificate %s names its principals as %w",
			c.Serial, err)
	}
	c.Purpose = CertificatePurpose(purpose)
	c.ValidAfter, c.ValidBefore = store.DecodeTime(after), store.DecodeTime(before)
	c.At, c.Position = store.DecodeTime(at), statelog.Unpack(Domain{}.Stream().Name, posn)
	return c, nil
}

// Certificates pages the certificates the engine signed, newest first. A host
// matches a certificate naming it among its hosts.
func (r *Reader) Certificates(ctx context.Context, q AuditQuery,
	fresh statelog.Freshness) (CertificatePage, error) {

	if err := q.validate(); err != nil {
		return CertificatePage{}, err
	}
	before, limit, err := page(q.Cursor, q.Limit)
	if err != nil {
		return CertificatePage{}, err
	}
	since, until := q.window()
	index, scope := "access_certificates_position_idx", DomainScope()
	if q.Handle != "" {
		index, scope = "access_certificates_seat_idx", SeatScope(q.Handle)
	}
	var out CertificatePage
	answer, err := r.log.Read(ctx, fresh.Query(scope, true), func(tx *sql.Tx) error {
		out = CertificatePage{}
		rows, queryErr := tx.QueryContext(ctx, `SELECT `+certificateColumns+`
			FROM access_certificates INDEXED BY `+index+`
			WHERE (? = '' OR agent_handle = ?) AND position < ? AND at >= ? AND at < ?
			  AND (? = '' OR instr(host_ids, '"' || ? || '"') > 0)
			ORDER BY position DESC LIMIT ?`,
			q.Handle, q.Handle, before, since, until, q.Host, q.Host, limit+1)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			c, scanErr := scanCertificate(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Rows = append(out.Rows, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return CertificatePage{}, fmt.Errorf("accessdomain: page the certificates: %w", err)
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		out.Next = cursorOf(out.Rows[limit-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// CommandRow is one command's record. Its text stays sealed until [CommandRow.Open].
type CommandRow struct {
	Handle   string
	ID       string
	Turn     string
	HeldCall string
	Host     string
	User     string
	Level    CommandLevel

	// CommandSealed is the command's text, sealed: opened by [CommandRow.Open]
	// for an administrator's audit read, and never shown as it is.
	CommandSealed string
	ContentKey    string

	StdinBytes   int64
	StdinSHA256  string
	Ran          bool
	ExitCode     int
	Duration     time.Duration
	StdoutBytes  int64
	StderrBytes  int64
	OutputSHA256 string
	Truncated    bool

	Approver          string
	Decision          CommandDecision
	CertificateSerial string
	Failure           CommandFailure

	At       time.Time
	Position statelog.Position
}

// Open is the command's text, opened under the keyring the row was sealed for:
// for an administrator's audit read alone.
func (c CommandRow) Open(sealer *Sealer) (string, error) {
	if sealer == nil {
		return "", errors.New("accessdomain: opening a command needs the fleet keyring")
	}
	return sealer.Open(c.CommandSealed, AAD(c.Handle, c.ID))
}

// CommandPage is one page of commands, newest first.
type CommandPage struct {
	Rows     []CommandRow
	Next     string
	Position statelog.Position
	Complete bool
}

// commandColumns is every column a [CommandRow] is read from.
const commandColumns = `agent_handle, command_id, turn_id, held_call, host, login_user, level,
	command_sealed, content_key, stdin_bytes, stdin_sha256, ran, exit_code, duration_ms,
	stdout_bytes, stderr_bytes, output_sha256, truncated, approver, decision,
	certificate_serial, failure, at, position`

// scanCommand reads one command row.
func scanCommand(row rowScanner) (CommandRow, error) {
	var (
		c                        CommandRow
		level, decision, failure string
		ran, truncated           int
		duration, at, posn       int64
	)
	if err := row.Scan(&c.Handle, &c.ID, &c.Turn, &c.HeldCall, &c.Host, &c.User, &level,
		&c.CommandSealed, &c.ContentKey, &c.StdinBytes, &c.StdinSHA256, &ran, &c.ExitCode,
		&duration, &c.StdoutBytes, &c.StderrBytes, &c.OutputSHA256, &truncated, &c.Approver,
		&decision, &c.CertificateSerial, &failure, &at, &posn); err != nil {
		return CommandRow{}, err
	}
	c.Level, c.Decision, c.Failure = CommandLevel(level), CommandDecision(decision), CommandFailure(failure)
	c.Ran, c.Truncated = ran == 1, truncated == 1
	c.Duration = time.Duration(duration) * time.Millisecond
	c.At, c.Position = store.DecodeTime(at), statelog.Unpack(Domain{}.Stream().Name, posn)
	return c, nil
}

// Commands pages the commands the engine ran or a person denied, newest first.
func (r *Reader) Commands(ctx context.Context, q AuditQuery,
	fresh statelog.Freshness) (CommandPage, error) {

	if err := q.validate(); err != nil {
		return CommandPage{}, err
	}
	before, limit, err := page(q.Cursor, q.Limit)
	if err != nil {
		return CommandPage{}, err
	}
	since, until := q.window()
	index, scope := "access_commands_position_idx", DomainScope()
	switch {
	case q.Handle != "":
		index, scope = "access_commands_seat_idx", SeatScope(q.Handle)
	case q.Host != "":
		index = "access_commands_host_idx"
	}
	var out CommandPage
	answer, err := r.log.Read(ctx, fresh.Query(scope, true), func(tx *sql.Tx) error {
		out = CommandPage{}
		rows, queryErr := tx.QueryContext(ctx, `SELECT `+commandColumns+`
			FROM access_commands INDEXED BY `+index+`
			WHERE (? = '' OR agent_handle = ?) AND (? = '' OR host = ?)
			  AND position < ? AND at >= ? AND at < ?
			ORDER BY position DESC LIMIT ?`,
			q.Handle, q.Handle, q.Host, q.Host, before, since, until, limit+1)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			c, scanErr := scanCommand(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Rows = append(out.Rows, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return CommandPage{}, fmt.Errorf("accessdomain: page the commands: %w", err)
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		out.Next = cursorOf(out.Rows[limit-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ---- verdicts ----------------------------------------------------------- //

// VerdictQuery narrows a page of verdicts. Every field is optional.
type VerdictQuery struct {
	// Handle is the seat whose requests they are, By the seat that gave
	// them, Request one request's, Host one registered host's.
	Handle  string
	By      string
	Request string
	Host    string

	// Kind, Verdict and ByKind narrow to one act, one verdict that took
	// effect and one kind of giver.
	Kind    delegated.Kind
	Verdict delegated.Verdict
	ByKind  VerdictBy

	// Final and Sampled narrow to the records that did or did not decide
	// their request, or were or were not chosen for a spot-check.
	Final   *bool
	Sampled *bool

	// Since and Until bound the record's broker instant, Until exclusive.
	Since time.Time
	Until time.Time

	Cursor string
	Limit  int
}

// validate refuses a query naming what no record could hold.
func (q VerdictQuery) validate() error {
	for _, seat := range []string{q.Handle, q.By} {
		if seat != "" {
			if err := checkSeat(seat); err != nil {
				return err
			}
		}
	}
	if err := checkRef("the request", q.Request, false); err != nil {
		return err
	}
	switch {
	case q.Host != "" && !hostPattern().MatchString(q.Host):
		return invalid("%q is not a host id", q.Host)
	case q.Kind != "" && !q.Kind.Delegable():
		return invalid("kind %q is not one of %s", q.Kind, words.Or(delegated.DelegableKinds()))
	case q.Verdict != "" && !q.Verdict.Valid():
		return invalid("verdict %q is not one of %s", q.Verdict, words.Or(delegated.Verdicts()))
	case q.ByKind != "" && !q.ByKind.Valid():
		return invalid("by_kind %q is not one of %s", q.ByKind, words.Or(VerdictGivers))
	case q.ByKind == VerdictByEngine && q.By != "":
		return invalid("the engine's verdicts name no seat")
	case !q.Since.IsZero() && !q.Until.IsZero() && !q.Until.After(q.Since):
		return invalid("until is after since")
	}
	return nil
}

// VerdictRow is one verdict's record. What it decided and its giver's words
// stay sealed until [VerdictRow.Open].
type VerdictRow struct {
	Handle  string
	ID      string
	Request string
	Kind    delegated.Kind
	Rule    int
	Turn    string

	By      string
	ByKind  VerdictBy
	Through VerdictThrough

	Asked      delegated.Verdict
	Confidence delegated.Confidence
	Verdict    delegated.Verdict
	Final      bool
	Overruled  []string
	Sampled    bool

	Digest      string
	Host        string
	User        string
	StdinBytes  int64
	StdinSHA256 string

	Decision string
	Item     string

	// SubjectSealed and WordsSealed are what was decided and the giver's
	// reason and findings, sealed under ContentKey: opened by
	// [VerdictRow.Open] for an administrator's audit read, never shown as
	// they are.
	SubjectSealed string
	WordsSealed   string
	ContentKey    string

	At       time.Time
	Position statelog.Position
}

// Open is what the verdict decided and its giver's reason and findings,
// opened under the keyring the row was sealed for: for an administrator's
// audit read and the engine's own digest alone.
func (v VerdictRow) Open(sealer *Sealer) (subject, reason string, findings []string, err error) {
	if sealer == nil {
		return "", "", nil, errors.New("accessdomain: opening a verdict needs the fleet keyring")
	}
	return sealer.openVerdict(v.Handle, v.ID, v.SubjectSealed, v.WordsSealed)
}

// VerdictPage is one page of verdicts, newest first.
type VerdictPage struct {
	Rows     []VerdictRow
	Next     string
	Position statelog.Position
	Complete bool
}

// verdictColumns is every column a [VerdictRow] is read from.
const verdictColumns = `agent_handle, verdict_id, request_id, request_kind, rule_number, turn_id,
	by_handle, by_kind, through, asked, confidence, verdict, final, overruled, sampled, digest,
	host, login_user, stdin_bytes, stdin_sha256, decision_id, item, subject_sealed, words_sealed,
	content_key, at, position`

// scanVerdict reads one verdict row.
func scanVerdict(row rowScanner) (VerdictRow, error) {
	var (
		v                                                            VerdictRow
		kind, byKind, through, asked, confidence, verdict, overruled string
		final, sampled                                               int
		at, posn                                                     int64
	)
	if err := row.Scan(&v.Handle, &v.ID, &v.Request, &kind, &v.Rule, &v.Turn, &v.By, &byKind,
		&through, &asked, &confidence, &verdict, &final, &overruled, &sampled, &v.Digest, &v.Host,
		&v.User, &v.StdinBytes, &v.StdinSHA256, &v.Decision, &v.Item, &v.SubjectSealed,
		&v.WordsSealed, &v.ContentKey, &at, &posn); err != nil {
		return VerdictRow{}, err
	}
	if err := json.Unmarshal([]byte(overruled), &v.Overruled); err != nil {
		return VerdictRow{}, fmt.Errorf("accessdomain: verdict %s names its overrulings as %w", v.ID, err)
	}
	v.Kind, v.ByKind, v.Through = delegated.Kind(kind), VerdictBy(byKind), VerdictThrough(through)
	v.Asked, v.Confidence = delegated.Verdict(asked), delegated.Confidence(confidence)
	v.Verdict = delegated.Verdict(verdict)
	v.Final, v.Sampled = final == 1, sampled == 1
	v.At, v.Position = store.DecodeTime(at), statelog.Unpack(Domain{}.Stream().Name, posn)
	return v, nil
}

// verdictStatement is the page query for q below a position: the narrowest
// index q names, each filter set spelt as an equality so the index seeks it.
// A spot-check page reads the partial index its literal predicate admits.
func verdictStatement(q VerdictQuery, before, since, until int64, limit int) (string, []any) {
	index := "access_verdicts_position_idx"
	switch {
	case q.Request != "":
		index = "access_verdicts_request_idx"
	case q.Handle != "":
		index = "access_verdicts_seat_idx"
	case q.By != "":
		index = "access_verdicts_by_idx"
	case q.Sampled != nil && *q.Sampled:
		index = "access_verdicts_sampled_idx"
	}
	var (
		where []string
		args  []any
	)
	for _, eq := range []struct{ column, value string }{
		{"request_id", q.Request}, {"agent_handle", q.Handle}, {"by_handle", q.By}, {"host", q.Host},
		{"request_kind", string(q.Kind)}, {"verdict", string(q.Verdict)}, {"by_kind", string(q.ByKind)},
	} {
		if eq.value != "" {
			where, args = append(where, eq.column+" = ?"), append(args, eq.value)
		}
	}
	for _, flag := range []struct {
		column string
		value  *bool
	}{{"final", q.Final}, {"sampled", q.Sampled}} {
		switch {
		case flag.value == nil:
		case *flag.value:
			where = append(where, flag.column+" = 1")
		default:
			where = append(where, flag.column+" = 0")
		}
	}
	where = append(where, "position < ?", "at >= ?", "at < ?")
	args = append(args, before, since, until, limit+1)
	return `SELECT ` + verdictColumns + ` FROM access_verdicts INDEXED BY ` + index + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY position DESC LIMIT ?`, args
}

// Verdicts pages the verdicts on delegated requests, newest first.
func (r *Reader) Verdicts(ctx context.Context, q VerdictQuery,
	fresh statelog.Freshness) (VerdictPage, error) {

	if err := q.validate(); err != nil {
		return VerdictPage{}, err
	}
	before, limit, err := page(q.Cursor, q.Limit)
	if err != nil {
		return VerdictPage{}, err
	}
	since, until := window(q.Since, q.Until)
	scope := DomainScope()
	if q.Handle != "" {
		scope = SeatScope(q.Handle)
	}
	statement, args := verdictStatement(q, before, since, until, limit)
	var out VerdictPage
	answer, err := r.log.Read(ctx, fresh.Query(scope, true), func(tx *sql.Tx) error {
		out = VerdictPage{}
		rows, queryErr := tx.QueryContext(ctx, statement, args...)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			v, scanErr := scanVerdict(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Rows = append(out.Rows, v)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return VerdictPage{}, fmt.Errorf("accessdomain: page the verdicts: %w", err)
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		out.Next = cursorOf(out.Rows[limit-1].Position)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// ---- the engine's duties ------------------------------------------------ //

// minAtStatements find the oldest instant at or after a bound and before
// another that an expiry would take, one indexed probe per table.
var minAtStatements = []string{
	`SELECT MIN(at) FROM access_told INDEXED BY access_told_expiry_idx
		WHERE latest = 0 AND at >= ? AND at < ?`,
	`SELECT MIN(at) FROM access_seat_removals INDEXED BY access_seat_removals_at_idx
		WHERE at >= ? AND at < ?`,
	`SELECT MIN(at) FROM access_certificates INDEXED BY access_certificates_expiry_idx
		WHERE at >= ? AND at < ?`,
	`SELECT MIN(at) FROM access_commands INDEXED BY access_commands_expiry_idx
		WHERE at >= ? AND at < ?`,
	`SELECT MIN(at) FROM access_verdicts INDEXED BY access_verdicts_expiry_idx
		WHERE at >= ? AND at < ?`,
}

// ExpirableHours lists, oldest first and at most limit of them, the hours that
// hold a record past cutoff an expiry would take: what the retention duty
// nominates. A stale read: each expiry is decided again in its own snapshot.
func (r *Reader) ExpirableHours(ctx context.Context, cutoff time.Time, limit int) ([]time.Time, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []time.Time
	end := store.EncodeTime(cutoff)
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		from := int64(0)
		for len(out) < limit {
			oldest := sql.NullInt64{}
			for _, statement := range minAtStatements {
				var at sql.NullInt64
				if err := tx.QueryRowContext(ctx, statement, from, end).Scan(&at); err != nil {
					return err
				}
				if at.Valid && (!oldest.Valid || at.Int64 < oldest.Int64) {
					oldest = at
				}
			}
			if !oldest.Valid {
				return nil
			}
			hour := store.DecodeTime(oldest.Int64).Truncate(ExpiryBucket)
			out = append(out, hour)
			from = store.EncodeTime(hour.Add(ExpiryBucket))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("accessdomain: list the hours past the retention: %w", err)
	}
	return out, nil
}

// Handles is every seat holding a told record: what the retention duty holds
// against the company's seats, to forget a removed seat's. A stale read: each
// removal is decided again before it is published.
func (r *Reader) Handles(ctx context.Context) ([]string, error) {
	var out []string
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT agent_handle FROM access_told INDEXED BY access_told_latest_idx
			WHERE latest = 1 ORDER BY agent_handle`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var handle string
			if scanErr := rows.Scan(&handle); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out = append(out, handle)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("accessdomain: list the seats told anything: %w", err)
	}
	return out, nil
}

// HostUse is one seat's use of one server over a month: how many commands the
// engine ran there for it in the last week and in the month, and the last.
type HostUse struct {
	Host, Handle string
	Week, Month  int64
	Last         time.Time
}

// HostUse is every seat's use of every server since month, the commands since
// week counted apart: what the atlas shows as a server's use. Counted in the
// store over the retention index, one row per server and seat, so a busy month
// costs one range scan and never a page of commands per row. Only commands
// that ran count: a denied one used nothing.
func (r *Reader) HostUse(ctx context.Context, week, month time.Time, fresh statelog.Freshness) ([]HostUse, error) {
	if week.Before(month) {
		return nil, invalid("the week a server's use counts starts inside the month")
	}
	var out []HostUse
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT host, agent_handle, SUM(CASE WHEN at >= ? THEN 1 ELSE 0 END), COUNT(*), MAX(at)
			  FROM access_commands INDEXED BY access_commands_expiry_idx
			 WHERE at >= ? AND ran = 1
			 GROUP BY host, agent_handle
			 ORDER BY host, agent_handle`, store.EncodeTime(week), store.EncodeTime(month))
		if err != nil {
			return err
		}
		for rows.Next() {
			var u HostUse
			var last int64
			if scanErr := rows.Scan(&u.Host, &u.Handle, &u.Week, &u.Month, &last); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			u.Last = store.DecodeTime(last)
			out = append(out, u)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("accessdomain: count each server's use: %w", err)
	}
	return out, nil
}

// SealedCount is how many recorded commands and verdicts are sealed under each
// keyring key.
type SealedCount struct {
	Values secrets.KeyCounts `json:"values,omitempty"`
}

// Outside is how many of them are sealed under a key other than active.
func (c SealedCount) Outside(active string) int { return c.Values.Outside(active) }

// sealedCountStatements count one sealed table's rows by key, one indexed walk
// each.
var sealedCountStatements = []string{
	`SELECT content_key, COUNT(*) FROM access_commands INDEXED BY access_commands_key_idx
		GROUP BY content_key`,
	`SELECT content_key, COUNT(*) FROM access_verdicts INDEXED BY access_verdicts_key_idx
		GROUP BY content_key`,
}

// SealedKeys counts the commands and the verdicts by the keyring key that
// sealed their texts: what a keyring rotation waits out before an old key may
// leave the ring.
func (r *Reader) SealedKeys(ctx context.Context) (SealedCount, error) {
	out := secrets.KeyCounts{}
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		clear(out)
		for _, statement := range sealedCountStatements {
			rows, err := tx.QueryContext(ctx, statement)
			if err != nil {
				return err
			}
			for rows.Next() {
				var (
					key string
					n   int
				)
				if scanErr := rows.Scan(&key, &n); scanErr != nil {
					_ = rows.Close()
					return scanErr
				}
				out[key] += n
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return SealedCount{}, fmt.Errorf("accessdomain: count the sealed values by key: %w", err)
	}
	return SealedCount{Values: out}, nil
}

// ---- paging ------------------------------------------------------------- //

// page reads a cursor and a limit: before is the packed position a page starts
// below, the largest for none.
func page(cursor string, limit int) (before int64, size int, err error) {
	switch {
	case limit < 0 || limit > MaxPageSize:
		return 0, 0, invalid("a page holds 1 to %d rows", MaxPageSize)
	case limit == 0:
		limit = DefaultPageSize
	}
	if cursor == "" {
		return int64(1<<63 - 1), limit, nil
	}
	before, err = strconv.ParseInt(cursor, 10, 64)
	if err != nil || before <= 0 {
		return 0, 0, invalid("the cursor %q is not one a page answered", cursor)
	}
	return before, limit, nil
}

// cursorOf is the cursor of the page below position p.
func cursorOf(p statelog.Position) string { return strconv.FormatInt(p.Packed(), 10) }
