package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about artifacts from this node's own applied rows.
//
// EVERY READ GOES THROUGH [statelog.Reader], which turns a level from a word
// into a property of the answer: the refusal ladder first (an evicted node, one
// below the trim floor, one whose applier has stopped serves nothing), then the
// coverage probe over what the read is ABOUT, then the freshness target, and
// only then the rows, all in ONE transaction.
//
// A READ THAT NAMES NO FRESHNESS is served at `stale`: whatever this node holds,
// with its coverage on the answer. Every such question is either one a caller
// asks after a point read that did name a floor (the versions of an artifact it
// just read) or one whose answer is re-decided where it is used (the carry
// plan, which the apply checks again; the trash reaper's list, which the purge
// re-decides).
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader
}

// NewReader builds the artifacts read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB.IsZero() {
		return nil, errors.New("artifactsdomain: a reader needs the replicated estate")
	}
	if opts.Log == nil {
		return nil, errors.New("artifactsdomain: a reader needs its domain's read " +
			"authority: without it every read level is a label rather than a " +
			"guarantee, and a degradation invisible in the answer is worse than a " +
			"refusal")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// Head is one artifact as a listing and a point read show it.
type Head struct {
	ID          artifacts.ID    `json:"id"`
	Label       artifacts.Label `json:"label"`
	Kind        artifacts.Kind  `json:"kind"`
	Title       string          `json:"title" untrusted:"name"`
	Description string          `json:"description,omitempty" untrusted:"name"`
	Icon        string          `json:"icon,omitempty"`
	Owner       string          `json:"owner"`
	OwnerKind   iam.ActorKind   `json:"owner_kind"`
	SourceKey   string          `json:"source_key,omitempty"`

	// Folder is the folder the artifact is filed in, empty at the top
	// level. A folder grants nothing: who may open the artifact is still
	// its owner and its own shares. A listing names it only to a viewer who
	// sees the folder, and a surface answering one artifact does the same
	// (artifactsapi's shownFolder); the raw value is what a decision reads.
	Folder string `json:"folder,omitempty"`

	HeadNumber   int                    `json:"head_number"`
	Contract     string                 `json:"contract,omitempty"`
	Capabilities []artifacts.Capability `json:"capabilities"`
	Status       Status                 `json:"status"`
	TrashedBy    iam.Actor              `json:"trashed_by"`
	TrashedAt    time.Time              `json:"trashed_at"`

	// PurgeBegunAt is the instant the artifact's purge began, zero for one
	// no purge has begun on. An artifact being purged is in the trash and
	// is never restored: its datastore may already be gone for good.
	PurgeBegunAt time.Time `json:"purge_begun_at"`

	AccessEpoch int64             `json:"access_epoch"`
	BytesHeld   int64             `json:"bytes_held"`
	Shares      []artifacts.Share `json:"shares,omitempty"`
	CreatedBy   iam.Actor         `json:"created_by"`
	UpdatedBy   iam.Actor         `json:"updated_by"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`

	// Version is the packed position of the last record that wrote the
	// artifact's row.
	Version int64 `json:"version"`
}

// Version is one retained version, its manifest included.
type Version struct {
	VersionSummary
	Manifest artifacts.Manifest `json:"manifest"`
}

// VersionSummary is one version without its manifest.
type VersionSummary struct {
	Artifact     artifacts.ID           `json:"artifact"`
	Number       int                    `json:"number"`
	VersionID    uuid.UUID              `json:"version_id"`
	Base         int                    `json:"base"`
	RestoredFrom int                    `json:"restored_from,omitempty"`
	Forced       bool                   `json:"forced,omitempty"`
	Label        string                 `json:"label,omitempty" untrusted:"name"`
	Entry        string                 `json:"entry,omitempty"`
	Contract     string                 `json:"contract,omitempty"`
	Capabilities []artifacts.Capability `json:"capabilities"`

	// Brand is the adopted brand stylesheet a framed version was published
	// under (its SHA-256 in hex), which the artifact origin places when it
	// serves the version; empty for none.
	Brand string `json:"brand,omitempty"`

	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	Author    iam.Actor `json:"author"`
	CreatedAt time.Time `json:"created_at"`

	// Position is the packed position of the record that made the version.
	Position int64 `json:"position"`
}

// VersionPage is one page of an artifact's versions, newest first.
type VersionPage struct {
	Versions []VersionSummary `json:"versions"`

	// After is the cursor for the next page, absent on the last.
	After string `json:"after,omitempty"`
}

// Asset is one live file of an artifact's asset store.
type Asset struct {
	Artifact    artifacts.ID    `json:"artifact"`
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name" untrusted:"name"`
	ContentType string          `json:"content_type"`
	Object      objstore.Object `json:"object"`
	AddedBy     iam.Actor       `json:"added_by"`
	AddedAt     time.Time       `json:"added_at"`
	Version     int64           `json:"version"`
}

// HistoryEntry is one row of an artifact's change record.
type HistoryEntry struct {
	Position int64     `json:"position"`
	OpID     string    `json:"op_id"`
	Op       string    `json:"op"`
	Outcome  Outcome   `json:"outcome"`
	Reason   Refusal   `json:"reason,omitempty"`
	Number   int       `json:"number,omitempty"`
	Actor    iam.Actor `json:"actor"`
	Elevated bool      `json:"elevated,omitempty"`
	Origin   string    `json:"origin,omitempty"`
	WorkItem string    `json:"work_item,omitempty"`
	At       time.Time `json:"at"`

	Summary HistorySummary `json:"summary"`
}

// HistoryPage is one page of an artifact's history, newest first.
type HistoryPage struct {
	Entries []HistoryEntry `json:"entries"`
	After   string         `json:"after,omitempty"`
}

// Read is when one person last opened an artifact.
type Read struct {
	Seat     string    `json:"seat"`
	OpenedAt time.Time `json:"opened_at"`
}

// Usage is the company's counters, which the stamped quotas are enforced
// against.
type Usage struct {
	Artifacts int64 `json:"artifacts"`
	Bytes     int64 `json:"bytes"`
	Versions  int64 `json:"versions"`
	Assets    int64 `json:"assets"`

	// Position is where the counters were read, and Complete whether no
	// record this node could not decode is missing from them.
	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// Viewer is what a listing filters by; the caller resolves units and leads
// first.
type Viewer struct {
	// Record is the viewer's iam.RecordOwner.
	Record string

	// Units are the unit keys the viewer's seat sits in or below.
	Units []string

	// LedOwners are the agent seats the viewer leads (the lead arm, D12).
	LedOwners []string

	// Admin is the admin path: every artifact, whatever its shares.
	Admin bool
}

// ListScope is which artifacts a listing is about.
type ListScope string

const (
	// ScopeMine is the viewer's own artifacts.
	ScopeMine ListScope = "mine"

	// ScopeShared is artifacts someone else owns that are shared with the
	// viewer, directly, through a unit or company-wide.
	ScopeShared ListScope = "shared"

	// ScopeCompany is artifacts shared with the whole company.
	ScopeCompany ListScope = "company"

	// ScopeAgents is artifacts agents own that the viewer can see.
	ScopeAgents ListScope = "agents"

	// ScopeAll is every artifact the viewer can see.
	ScopeAll ListScope = "all"
)

// ListScopes are every scope.
var ListScopes = []ListScope{ScopeMine, ScopeShared, ScopeCompany, ScopeAgents, ScopeAll}

// Valid reports whether s is a scope this build lists.
func (s ListScope) Valid() bool { return slices.Contains(ListScopes, s) }

// ListSort is how a listing is ordered.
type ListSort string

const (
	// SortUpdated is the most recently changed first: the default.
	SortUpdated ListSort = "updated"

	// SortCreated is the most recently created first.
	SortCreated ListSort = "created"

	// SortTitle is by title, A to Z.
	SortTitle ListSort = "title"

	// SortOpened is the viewer's most recently opened first.
	SortOpened ListSort = "opened"
)

// ListSorts are every order.
var ListSorts = []ListSort{SortUpdated, SortCreated, SortTitle, SortOpened}

// Valid reports whether s is an order this build lists in.
func (s ListSort) Valid() bool { return slices.Contains(ListSorts, s) }

// DefaultListLimit and MaxListLimit bound one listing page: a gallery screen,
// and the most a client may ask for at once.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListDelimiter is the one delimiter a listing accepts, as S3's ListObjectsV2
// names it: the separator between folder names in a [ListQuery.Prefix].
const ListDelimiter = "/"

// ListQuery narrows a listing. A zero Scope is [ScopeAll], a zero Sort
// [SortUpdated], a zero Status [StatusLive], and a zero Fresh `stale`.
//
// THE FOLDER HALF SPEAKS S3's ListObjectsV2 (Prefix, Delimiter,
// ContinuationToken, MaxKeys), so anybody who knows S3 reads it at once. A
// folder is still a row (ADR-0047), never part of an artifact's key: the
// prefix is resolved to a folder id once, and the token carries the id.
type ListQuery struct {
	Scope  ListScope
	Kind   artifacts.Kind
	Status Status

	// Q matches the title, the description and the head version's search
	// text, case-insensitively.
	Q string

	// Pinned narrows to the viewer's pins.
	Pinned bool

	// WorkItem narrows to the artifacts a write was made for this tracker
	// item under ([artifacts.Authority.WorkItem]), or that were linked to it
	// ([OpLinkWorkItem]), as the applied history rows of the last
	// [artifacts.HistoryRetention] record it: what a work item's Artifacts
	// panel lists. Empty narrows nothing.
	WorkItem string

	// Prefix is the folder the listing is about: the path of folder names
	// from the top level, each followed by [ListDelimiter]
	// ("Marketing/Q4/"). Empty is the top level. It resolves among the
	// folders the viewer (and the Requester, where one is named) can see,
	// and a prefix that resolves to none is ErrFolderNotFound, never an
	// empty page. A name holds no "/" and siblings are unique, so below
	// the top level a path names at most one folder; at the top level a
	// private folder may share a company folder's name, and a prefix
	// naming both is a *FolderAmbiguous naming each.
	Prefix string

	// Folder is the folder the listing is about by its id, in place of
	// Prefix: the rename-proof address a client that keeps folder ids (the
	// dashboard's folder pages) lists by, which no two folders share. It
	// resolves among the same folders a prefix does.
	Folder string

	// Delimiter is empty or [ListDelimiter]. With it the page is ONE level:
	// the artifacts directly in the prefix's folder (at the top level: in no
	// folder, or in one the viewer cannot see) and, as
	// [ListPage.CommonPrefixes], the child folders the viewer can see.
	// Without it the listing is recursive: everything beneath the prefix,
	// which with no prefix is every artifact the viewer can see.
	Delimiter string

	// Requester is the person an agent's tool acts for. Where it is named,
	// it narrows EVERYTHING the page discloses as the viewer does: the
	// prefix resolves only among folders BOTH can see, the rows, the total
	// and the counts hold only what both can see, and a row names its
	// folder's path, and a common prefix its folder, only where both see
	// it (the requester ceiling, applied to the listing's own filter).
	Requester *Viewer

	Sort ListSort

	// ContinuationToken is the opaque token a previous page answered as
	// [ListPage.NextContinuationToken]. It carries the resolved folder's
	// id, never its path, so a rename mid-way does not move the pages.
	ContinuationToken string

	// MaxKeys is the page size: [DefaultListLimit] when zero, at most
	// [MaxListLimit]. It bounds Contents; a one-level page answers every
	// common prefix on its first page.
	MaxKeys int

	Fresh statelog.Freshness
}

// CommonPrefix is one child folder of a one-level listing, as S3 answers a
// common prefix, with what a folder tile shows.
type CommonPrefix struct {
	// Prefix is the folder's path, the listing's prefix and its name and a
	// "/" ("Marketing/Q4/Campaigns/"): what a caller lists next.
	Prefix string `json:"prefix"`

	// Folder is its id: the address a create or a move files into, and a
	// rename-proof link.
	Folder string `json:"folder"`

	Name       string           `json:"name"`
	Visibility FolderVisibility `json:"visibility"`

	// Items and Folders count the artifacts and the folders directly in it
	// that the viewer can see.
	Items   int `json:"items"`
	Folders int `json:"folders"`
}

// ListItem is one artifact as a listing shows it.
type ListItem struct {
	Head
	Pinned   bool      `json:"pinned,omitempty"`
	OpenedAt time.Time `json:"opened_at"`

	// FolderPath is the path of the folder the artifact is filed in as the
	// viewer sees it ("Marketing/Q4/"), empty at the top level or in a folder
	// the viewer cannot see, where the head's Folder is empty too.
	FolderPath string `json:"folder_path,omitempty"`
}

// ListPage is one page of a listing and what it is true as of, in S3's
// ListObjectsV2 terms.
type ListPage struct {
	// Prefix and Delimiter echo the query's.
	Prefix    string `json:"prefix"`
	Delimiter string `json:"delimiter,omitempty"`

	// Contents are the artifacts on this page.
	Contents []ListItem `json:"contents"`

	// CommonPrefixes are a one-level page's child folders the viewer can
	// see, A to Z, every one on the first page and none after it.
	CommonPrefixes []CommonPrefix `json:"common_prefixes,omitempty"`

	// KeyCount is how many entries this page answers, contents and common
	// prefixes together, as S3 counts them.
	KeyCount int `json:"key_count"`

	// IsTruncated reports a page past which there is more, and
	// NextContinuationToken is what asks for it.
	IsTruncated           bool   `json:"is_truncated"`
	NextContinuationToken string `json:"next_continuation_token,omitempty"`

	// Total is how many artifacts the query matches, counted in the same
	// transaction as the page.
	Total int `json:"total"`

	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`
	Complete bool               `json:"complete"`
}

// staleFresh is the freshness a question that names none is served at.
func staleFresh(f statelog.Freshness) statelog.Freshness {
	if f.Level == "" {
		f.Level = statelog.ReadStale
	}
	return f
}

// Artifact answers one artifact, its shares included, and the position it was
// read at.
func (r *Reader) Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
	Head, statelog.Position, error) {

	if fresh.Level == "" {
		return Head{}, statelog.Position{}, errors.New("artifactsdomain: this read " +
			"names no level: a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	var head Head
	served, err := r.log.Read(ctx, fresh.Query(ArtifactScope(id.String()), false),
		func(tx *sql.Tx) error {
			var err error
			head, err = mustHead(ctx, tx, id.String())
			return err
		})
	if err != nil {
		return Head{}, statelog.Position{}, err
	}
	return head, served.Position, nil
}

// mustHead is one artifact's head, or the refusal saying why there is none.
func mustHead(ctx context.Context, tx *sql.Tx, id string) (Head, error) {
	head, found, err := readHeadTx(ctx, tx, id)
	if err != nil || found {
		return head, err
	}
	return Head{}, absent(ctx, tx, id)
}

// absent is why artifact id has no row: purged, or never there.
func absent(ctx context.Context, tx *sql.Tx, id string) error {
	_, marked, err := deletionMarker(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case marked:
		return fmt.Errorf("%w: %s", ErrPurged, id)
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// headColumns is every column a Head is read from, in [scanHead]'s order.
const headColumns = `a.id, a.label, a.kind, a.title, a.description, a.icon, a.owner,
	a.owner_kind, a.source_key, a.head_number, a.contract, a.capabilities, a.status,
	a.trashed_by, a.trashed_by_kind, a.trashed_at, a.purge_begun_at, a.access_epoch, a.bytes_held,
	a.created_by, a.created_by_kind, a.created_at, a.updated_by, a.updated_by_kind,
	a.updated_at, a.version, a.folder_id`

// rowScanner is a row or a cursor over rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanHead reads one Head from a row of [headColumns], extra columns after.
func scanHead(row rowScanner, extra ...any) (Head, error) {
	var (
		h                                  Head
		id, label, kind, ownerKind, status string
		caps                               string
		trashedBy, trashedKind             string
		createdBy, createdKind             string
		updatedBy, updatedKind             string
		trashedAt, createdAt, updatedAt    int64
		purgeBegunAt                       int64
	)
	dest := []any{&id, &label, &kind, &h.Title, &h.Description, &h.Icon, &h.Owner,
		&ownerKind, &h.SourceKey, &h.HeadNumber, &h.Contract, &caps, &status,
		&trashedBy, &trashedKind, &trashedAt, &purgeBegunAt, &h.AccessEpoch, &h.BytesHeld,
		&createdBy, &createdKind, &createdAt, &updatedBy, &updatedKind, &updatedAt,
		&h.Version, &h.Folder}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Head{}, err
	}
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return Head{}, fmt.Errorf("artifactsdomain: the row for %q names no artifact: %w", id, err)
	}
	h.ID, h.Label, h.Kind = parsed, artifacts.Label(label), artifacts.Kind(kind)
	h.OwnerKind, h.Status = iam.ActorKind(ownerKind), Status(status)
	if h.Capabilities, err = decodeCapabilities(caps); err != nil {
		return Head{}, err
	}
	h.TrashedBy = iam.Actor{Name: trashedBy, Kind: iam.ActorKind(trashedKind)}
	if trashedAt > 0 {
		h.TrashedAt = store.DecodeTime(trashedAt)
	}
	if purgeBegunAt > 0 {
		h.PurgeBegunAt = store.DecodeTime(purgeBegunAt)
	}
	h.CreatedBy = iam.Actor{Name: createdBy, Kind: iam.ActorKind(createdKind)}
	h.UpdatedBy = iam.Actor{Name: updatedBy, Kind: iam.ActorKind(updatedKind)}
	h.CreatedAt, h.UpdatedAt = store.DecodeTime(createdAt), store.DecodeTime(updatedAt)
	return h, nil
}

// decodeCapabilities reads a stored capability array.
//
// NONE IS AN EMPTY SET, NEVER AN ABSENT ONE: a head or a version that
// declares no capability answers `"capabilities": []`, so a reader tells a
// version whose capabilities were stripped from one that never said, and
// nothing falls back to another version's set on a missing field.
func decodeCapabilities(raw string) ([]artifacts.Capability, error) {
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("artifactsdomain: decode stored capabilities: %w", err)
	}
	out := make([]artifacts.Capability, len(names))
	for i, n := range names {
		out[i] = artifacts.Capability(n)
	}
	return out, nil
}

// readHeadTx reads one artifact's head and its shares.
func readHeadTx(ctx context.Context, tx *sql.Tx, id string) (Head, bool, error) {
	head, err := scanHead(tx.QueryRowContext(ctx,
		`SELECT `+headColumns+` FROM artifacts a WHERE a.id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Head{}, false, nil
	case err != nil:
		return Head{}, false, fmt.Errorf("artifactsdomain: read artifact %s: %w", id, err)
	}
	shares, err := readShares(ctx, tx, []string{id})
	if err != nil {
		return Head{}, false, err
	}
	head.Shares = shares[id]
	return head, true, nil
}

// readShares reads the shares of every artifact named, in one statement per
// artifact batch, ordered by audience so a head reads the same on every data node.
func readShares(ctx context.Context, tx *sql.Tx, ids []string) (map[string][]artifacts.Share, error) {
	out := map[string][]artifacts.Share{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT artifact_id, audience_kind, audience_id, role, granted_by,
		       granted_by_kind, granted_at
		FROM artifact_shares WHERE artifact_id IN (`+placeholders(len(ids))+`)
		ORDER BY artifact_id, audience_kind, audience_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read shares: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, kind, audience, role, by, byKind string
			at                                   int64
		)
		if err := rows.Scan(&id, &kind, &audience, &role, &by, &byKind, &at); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read shares: %w", err)
		}
		out[id] = append(out[id], artifacts.Share{
			Audience:  artifacts.Audience{Kind: artifacts.AudienceKind(kind), ID: audience},
			Role:      artifacts.Role(role),
			GrantedBy: iam.Actor{Name: by, Kind: iam.ActorKind(byKind)},
			GrantedAt: store.DecodeTime(at),
		})
	}
	return out, rows.Err()
}

// placeholders is n comma-separated parameter markers.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// versionColumns is every column a VersionSummary is read from.
const versionColumns = `artifact_id, number, version_id, base_number, restored_from,
	forced, label, entry, contract, capabilities, files, bytes, author, author_kind,
	created_at, position, brand`

// scanVersion reads one VersionSummary, extra columns after.
func scanVersion(row rowScanner, extra ...any) (VersionSummary, error) {
	var (
		v                         VersionSummary
		id, versionID, caps, kind string
		forced                    int
		createdAt                 int64
	)
	dest := []any{&id, &v.Number, &versionID, &v.Base, &v.RestoredFrom, &forced, &v.Label,
		&v.Entry, &v.Contract, &caps, &v.Files, &v.Bytes, &v.Author.Name, &kind,
		&createdAt, &v.Position, &v.Brand}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return VersionSummary{}, err
	}
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return VersionSummary{}, fmt.Errorf("artifactsdomain: the version row names "+
			"no artifact: %w", err)
	}
	v.Artifact, v.Forced = parsed, forced != 0
	v.Author.Kind = iam.ActorKind(kind)
	v.CreatedAt = store.DecodeTime(createdAt)
	if versionID != "" {
		if v.VersionID, err = uuid.Parse(versionID); err != nil {
			return VersionSummary{}, fmt.Errorf("artifactsdomain: version %d's id: %w",
				v.Number, err)
		}
	}
	if v.Capabilities, err = decodeCapabilities(caps); err != nil {
		return VersionSummary{}, err
	}
	return v, nil
}

// readVersionTx reads one version, its manifest included.
func readVersionTx(ctx context.Context, tx *sql.Tx, id string, number int) (Version, bool, error) {
	var raw []byte
	summary, err := scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+`,
		manifest FROM artifact_versions WHERE artifact_id = ? AND number = ?`, id, number), &raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Version{}, false, nil
	case err != nil:
		return Version{}, false, fmt.Errorf("artifactsdomain: read %s version %d: %w",
			id, number, err)
	}
	m, err := manifest.Decode(raw)
	if err != nil {
		return Version{}, false, fmt.Errorf("artifactsdomain: %s version %d: %w", id, number, err)
	}
	return Version{VersionSummary: summary, Manifest: m}, true, nil
}

// Version answers one retained version of an artifact.
func (r *Reader) Version(ctx context.Context, id artifacts.ID, number int) (Version, error) {
	var out Version
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), false), func(tx *sql.Tx) error {
		v, found, err := readVersionTx(ctx, tx, id.String(), number)
		if err != nil {
			return err
		}
		if !found {
			if _, err := mustHead(ctx, tx, id.String()); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s: this artifact doesn't keep version %d", ErrNotFound, id,
				number)
		}
		out = v
		return nil
	})
	return out, err
}

// Versions answers a page of an artifact's retained versions, newest first.
func (r *Reader) Versions(ctx context.Context, id artifacts.ID, cursor string, limit int) (VersionPage, error) {
	before, err := intCursor(cursor)
	if err != nil {
		return VersionPage{}, err
	}
	limit = pageLimit(limit)
	var page VersionPage
	_, err = r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return err
		}
		query := `SELECT ` + versionColumns + ` FROM artifact_versions WHERE artifact_id = ?`
		args := []any{id.String()}
		if before > 0 {
			query += ` AND number < ?`
			args = append(args, before)
		}
		rows, err := tx.QueryContext(ctx, query+` ORDER BY number DESC LIMIT ?`, //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			append(args, limit+1)...)
		if err != nil {
			return fmt.Errorf("artifactsdomain: read %s's versions: %w", id, err)
		}
		defer func() { _ = rows.Close() }()
		page = VersionPage{Versions: []VersionSummary{}}
		for rows.Next() {
			v, err := scanVersion(rows)
			if err != nil {
				return fmt.Errorf("artifactsdomain: read %s's versions: %w", id, err)
			}
			page.Versions = append(page.Versions, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Versions) > limit {
			page.Versions = page.Versions[:limit]
			page.After = encodeIntCursor(int64(page.Versions[limit-1].Number))
		}
		return nil
	})
	return page, err
}

// Assets answers an artifact's live assets, oldest first.
func (r *Reader) Assets(ctx context.Context, id artifacts.ID) ([]Asset, error) {
	var out []Asset
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil {
			return err
		}
		var err error
		out, err = readAssets(ctx, tx, id.String(), "")
		return err
	})
	return out, err
}

// readAssets reads an artifact's live assets, or the one named.
func readAssets(ctx context.Context, tx *sql.Tx, id, only string) ([]Asset, error) {
	query := `SELECT asset_id, name, content_type, object, hash, size, added_by,
		added_by_kind, added_at, version
		FROM artifact_assets WHERE artifact_id = ? AND object IS NOT NULL`
	args := []any{id}
	if only != "" {
		query += ` AND asset_id = ?`
		args = append(args, only)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY added_at, asset_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's assets: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: %q names no artifact: %w", id, err)
	}
	out := []Asset{}
	for rows.Next() {
		var (
			a                     Asset
			assetID, object, hash string
			addedKind             string
			addedAt               int64
		)
		if err := rows.Scan(&assetID, &a.Name, &a.ContentType, &object, &hash, //nolint:govet // shadow: scoped to this block; see .golangci.yml
			&a.Object.Size, &a.AddedBy.Name, &addedKind, &addedAt, &a.Version); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read %s's assets: %w", id, err)
		}
		if a.ID, err = uuid.Parse(assetID); err != nil {
			return nil, fmt.Errorf("artifactsdomain: asset id %q: %w", assetID, err)
		}
		if a.Object.Key, err = objstore.ParseKey(object); err != nil {
			return nil, fmt.Errorf("artifactsdomain: asset %s's object: %w", assetID, err)
		}
		a.Artifact, a.Object.Hash = parsed, objstore.Hash(hash)
		a.AddedBy.Kind = iam.ActorKind(addedKind)
		a.AddedAt = store.DecodeTime(addedAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// History answers a page of an artifact's history, newest first.
func (r *Reader) History(ctx context.Context, id artifacts.ID, cursor string, limit int) (HistoryPage, error) {
	before, err := intCursor(cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	limit = pageLimit(limit)
	var page HistoryPage
	_, err = r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return err
		}
		query := `SELECT ` + historyColumns + ` FROM artifact_history WHERE artifact_id = ?`
		args := []any{id.String()}
		if before > 0 {
			query += ` AND position < ?`
			args = append(args, before)
		}
		rows, err := tx.QueryContext(ctx, query+` ORDER BY position DESC LIMIT ?`, //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			append(args, limit+1)...)
		if err != nil {
			return fmt.Errorf("artifactsdomain: read %s's history: %w", id, err)
		}
		defer func() { _ = rows.Close() }()
		page = HistoryPage{Entries: []HistoryEntry{}}
		for rows.Next() {
			e, err := scanHistory(rows)
			if err != nil {
				return fmt.Errorf("artifactsdomain: read %s's history: %w", id, err)
			}
			page.Entries = append(page.Entries, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Entries) > limit {
			page.Entries = page.Entries[:limit]
			page.After = encodeIntCursor(page.Entries[limit-1].Position)
		}
		return nil
	})
	return page, err
}

// historyColumns are the columns of one history row every history read
// selects, in the order [scanHistory] reads them.
const historyColumns = `position, op_id, op, outcome, reason, number, actor, actor_kind,
	operator_id, elevated, origin, work_item, at, summary`

// scanHistory reads one history row, after the columns lead scans into: the
// one decoding every history read shares, so the artifact's own history and
// the company trail cannot read a row two ways.
func scanHistory(rows *sql.Rows, lead ...any) (HistoryEntry, error) {
	var (
		e                     HistoryEntry
		outcome, reason, kind string
		summary               string
		elevated              int
		at                    int64
	)
	dest := append(append(make([]any, 0, len(lead)+14), lead...), &e.Position, &e.OpID, &e.Op, &outcome, &reason, &e.Number,
		&e.Actor.Name, &kind, &e.Actor.OperatorID, &elevated, &e.Origin, &e.WorkItem, &at,
		&summary)
	if err := rows.Scan(dest...); err != nil {
		return HistoryEntry{}, err
	}
	e.Outcome, e.Reason = Outcome(outcome), Refusal(reason)
	e.Actor.Kind, e.Elevated, e.At = iam.ActorKind(kind), elevated != 0, store.DecodeTime(at)
	if err := json.Unmarshal([]byte(summary), &e.Summary); err != nil {
		return HistoryEntry{}, fmt.Errorf("decode a history summary: %w", err)
	}
	return e, nil
}

// TrailQuery narrows the company trail: one op (`read_elevated` for the
// administrators' overrides alone, empty for every record), the cursor a page
// answered as `After`, and the page size.
type TrailQuery struct {
	Op     OpKind
	Cursor string
	Limit  int
}

// TrailEntry is one history row of the company trail: the artifact it is
// about, its title and owner as the head holds them now, and the row.
type TrailEntry struct {
	Artifact artifacts.ID `json:"artifact"`
	Title    string       `json:"title"`
	Owner    string       `json:"owner"`
	HistoryEntry
}

// TrailPage is one page of the company trail, newest first, with the position
// this node's copy answered at and whether that copy is complete.
type TrailPage struct {
	Entries  []TrailEntry      `json:"entries"`
	After    string            `json:"after,omitempty"`
	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// trailCursor is where one trail page ended: the last row's position and
// artifact, the trail's total order.
type trailCursor struct {
	Position int64  `json:"p"`
	Artifact string `json:"a"`
}

// Trail answers a page of THE COMPANY TRAIL: every artifact's history rows
// across the company, newest first in the log's own order (position, then
// artifact), private artifacts included. It is the administrators' record of
// what happened to every artifact, the elevated reads above all (D09): an
// override recorded only in an artifact's own history is one nobody finds when
// its owner has left or never looks. The caller decides who may ask
// (`audit.read`); this read answers whoever it is asked for.
func (r *Reader) Trail(ctx context.Context, q TrailQuery) (TrailPage, error) {
	// HELD TO THE HISTORY'S OPS, not to every op the log carries: a pin or
	// an opened mark writes no history row, so narrowing to one answers an
	// empty trail forever, and an asset's rows are filed under the asset's
	// own op ([HistoryOps]).
	if q.Op != "" && !slices.Contains(HistoryOps, q.Op) {
		return TrailPage{}, fmt.Errorf("%w: %q is not an op an artifact's history records, "+
			"which are %v", ErrInvalid, q.Op, HistoryOps)
	}
	var after trailCursor
	if q.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return TrailPage{}, fmt.Errorf("%w: %w", ErrBadCursor, err)
		}
		if err := json.Unmarshal(data, &after); err != nil || after.Position <= 0 ||
			after.Artifact == "" {
			return TrailPage{}, ErrBadCursor
		}
	}
	limit := pageLimit(q.Limit)
	var page TrailPage
	served, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(DomainScope(), true),
		func(tx *sql.Tx) error {
			query := `SELECT h.artifact_id, COALESCE(a.title, ''), COALESCE(a.owner, ''), ` +
				prefixed("h.", historyColumns) + `
				FROM artifact_history h LEFT JOIN artifacts a ON a.id = h.artifact_id`
			var where []string
			var args []any
			if q.Op != "" {
				where = append(where, `h.op = ?`)
				args = append(args, string(q.Op))
			}
			if after.Position > 0 {
				where = append(where, `(h.position < ? OR (h.position = ? AND h.artifact_id < ?))`)
				args = append(args, after.Position, after.Position, after.Artifact)
			}
			if len(where) > 0 {
				query += ` WHERE ` + strings.Join(where, ` AND `)
			}
			rows, err := tx.QueryContext(ctx,
				query+` ORDER BY h.position DESC, h.artifact_id DESC LIMIT ?`,
				append(args, limit+1)...)
			if err != nil {
				return fmt.Errorf("artifactsdomain: read the company trail: %w", err)
			}
			defer func() { _ = rows.Close() }()
			page = TrailPage{Entries: []TrailEntry{}}
			for rows.Next() {
				var id string
				var t TrailEntry
				if t.HistoryEntry, err = scanHistory(rows, &id, &t.Title, &t.Owner); err != nil {
					return fmt.Errorf("artifactsdomain: read the company trail: %w", err)
				}
				if t.Artifact, err = artifacts.ParseID(id); err != nil {
					return fmt.Errorf("artifactsdomain: a trail row names artifact %q: %w", id, err)
				}
				page.Entries = append(page.Entries, t)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(page.Entries) > limit {
				page.Entries = page.Entries[:limit]
				last := page.Entries[limit-1]
				data, _ := json.Marshal(trailCursor{Position: last.Position,
					Artifact: last.Artifact.String()})
				page.After = base64.RawURLEncoding.EncodeToString(data)
			}
			return nil
		})
	if err != nil {
		return TrailPage{}, err
	}
	page.Position, page.Complete = served.Position, served.Complete
	return page, nil
}

// prefixed is a column list with every column qualified by prefix.
func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// HeldObjects answers which of the digests asked about THIS artifact already
// names, each with the object that holds it: the carry plan's one question,
// and nothing wider (a carry never crosses an artifact). Where two of the
// artifact's objects hold one digest, the lowest key answers, on every data node.
func (r *Reader) HeldObjects(ctx context.Context, id artifacts.ID, hashes []objstore.Hash) (
	map[objstore.Hash]objstore.Object, error) {

	out := map[objstore.Hash]objstore.Object{}
	if len(hashes) == 0 {
		return out, nil
	}
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		var err error
		out, err = heldObjects(ctx, tx, id.String(), hashes)
		return err
	})
	return out, err
}

// heldObjects is [Reader.HeldObjects] inside a transaction, shared with the
// writer's own decide.
func heldObjects(ctx context.Context, tx *sql.Tx, id string, hashes []objstore.Hash) (
	map[objstore.Hash]objstore.Object, error) {

	out := map[objstore.Hash]objstore.Object{}
	asked := slices.Compact(slices.Sorted(slices.Values(hashes)))
	for start := 0; start < len(asked); start += maxInArgs {
		chunk := asked[start:min(start+maxInArgs, len(asked))]
		args := []any{id}
		for _, h := range chunk {
			args = append(args, string(h))
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT object, hash, size FROM artifact_objects
			WHERE artifact_id = ? AND refs > 0 AND hash IN (`+placeholders(len(chunk))+`)
			ORDER BY hash, object`, args...)
		if err != nil {
			return nil, fmt.Errorf("artifactsdomain: read %s's held objects: %w", id, err)
		}
		for rows.Next() {
			var key, hash string
			var size int64
			if err := rows.Scan(&key, &hash, &size); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("artifactsdomain: read %s's held objects: %w", id, err)
			}
			if _, seen := out[objstore.Hash(hash)]; seen {
				continue
			}
			parsed, err := objstore.ParseKey(key)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("artifactsdomain: %s names object %q: %w", id, key, err)
			}
			out[objstore.Hash(hash)] = objstore.Object{Key: parsed, Hash: objstore.Hash(hash), Size: size}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// maxInArgs bounds one IN list: the most digests a manifest names
// ([artifacts.MaxFilesPerVersion]), comfortably under the store's bind limit.
const maxInArgs = 512

// SeenBy answers who has opened an artifact and when, most recent first (D21).
func (r *Reader) SeenBy(ctx context.Context, id artifacts.ID) ([]Read, error) {
	var out []Read
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT seat, opened_at FROM artifact_reads WHERE artifact_id = ?
			ORDER BY opened_at DESC, seat`, id.String())
		if err != nil {
			return fmt.Errorf("artifactsdomain: read who opened %s: %w", id, err)
		}
		defer func() { _ = rows.Close() }()
		out = []Read{}
		for rows.Next() {
			var r Read
			var at int64
			if err := rows.Scan(&r.Seat, &at); err != nil {
				return fmt.Errorf("artifactsdomain: read who opened %s: %w", id, err)
			}
			r.OpenedAt = store.DecodeTime(at)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Usage answers the company's counters.
func (r *Reader) Usage(ctx context.Context) (Usage, error) {
	var out Usage
	served, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(DomainScope(), true),
		func(tx *sql.Tx) error {
			u, err := readUsage(ctx, tx)
			out = Usage{Artifacts: u.artifacts, Bytes: u.bytes, Versions: u.versions, Assets: u.assets}
			return err
		})
	if err != nil {
		return Usage{}, err
	}
	out.Position, out.Complete = served.Position, served.Complete
	return out, nil
}

// TrashedBefore answers up to limit artifacts the trash reaper purges: first
// every artifact whose purge has begun and not finished (a purge that stopped
// part way, which nothing but a purge may finish, whatever its trash term),
// the longest begun first, then those trashed before cutoff, the longest
// trashed first.
func (r *Reader) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]artifacts.ID, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: a trash listing names how many it wants", ErrInvalid)
	}
	var out []artifacts.ID
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(DomainScope(), true),
		func(tx *sql.Tx) error {
			out = []artifacts.ID{}
			seen := map[artifacts.ID]bool{}
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`SELECT id FROM artifacts WHERE purge_begun_at > 0
					ORDER BY purge_begun_at, id LIMIT ?`, nil},
				{`SELECT id FROM artifacts
					WHERE status = ? AND trashed_at > 0 AND trashed_at < ?
					ORDER BY trashed_at, id LIMIT ?`,
					[]any{string(StatusTrashed), store.EncodeTime(cutoff)}},
			} {
				if len(out) >= limit {
					break
				}
				ids, err := trashIDs(ctx, tx, q.sql, append(q.args, limit)...)
				if err != nil {
					return err
				}
				for _, id := range ids {
					if len(out) < limit && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				}
			}
			return nil
		})
	return out, err
}

// trashIDs runs one trash listing selecting artifact ids.
func trashIDs(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]artifacts.ID, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []artifacts.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read the trash: %w", err)
		}
		parsed, err := artifacts.ParseID(id)
		if err != nil {
			return nil, fmt.Errorf("artifactsdomain: the trash names %q: %w", id, err)
		}
		out = append(out, parsed)
	}
	return out, rows.Err()
}

// List answers one page of the artifacts the viewer can see, filtered by the
// query. A viewer who is no admin sees an artifact they own, one shared with
// their seat, a unit they sit in or the company, and one an agent they lead
// owns (the lead arm, D12, which reaches only an agent owner's artifacts).
// The authority table still decides each row where the answer is served; this
// is the filter that keeps the page from holding rows nobody may see.
//
// # Folders, in S3's terms
//
// The page is placed in the folder tree as the viewer (and the requester,
// where one is named) sees it: the prefix or the folder id resolves among the
// folders they can see, and an unknown or invisible one is [ErrFolderNotFound],
// never an empty page. With [ListDelimiter] the page is one level, whose child
// folders are answered as [ListPage.CommonPrefixes] on the first page; an
// artifact filed in a folder the viewer cannot see is at THEIR top level, so a
// folder never hides an artifact its reader may open. Every row names its
// folder's path only where the viewer sees that folder.
func (r *Reader) List(ctx context.Context, v Viewer, q ListQuery) (ListPage, error) {
	spec, err := listSpecFor(v, q)
	if err != nil {
		return ListPage{}, err
	}
	var page ListPage
	served, err := r.log.Read(ctx, staleFresh(q.Fresh).Query(DomainScope(), true),
		func(tx *sql.Tx) (err error) {
			page, err = spec.run(ctx, tx)
			return err
		})
	if err != nil {
		return ListPage{}, err
	}
	page.Level, page.Position, page.Complete = served.Level, served.Position, served.Complete
	return page, nil
}

// listSpec is a listing as statements.
type listSpec struct {
	where  []string
	args   []any
	sort   ListSort
	status Status
	record string
	after  *listCursor
	limit  int

	// viewers are whose relations decide every folder the page names and
	// every row it holds: the viewer, and the requester where one is named.
	viewers []Viewer

	// prefix and folder are the folder the query names, by path or by id
	// (neither for the top level), and delimited a one-level page.
	prefix, folder string
	delimited      bool
}

// listCursor is where a listing page ends, and the folder it lists.
//
// THE KEY IS TEXT for every order, an instant written as its decimal micros:
// JSON numbers decode as floats, and a float cannot carry every micro.
//
// THE FOLDER IS ITS ID, NEVER ITS PATH, so a rename between two pages does not
// move the next one, and a token is held to the delimiter it was minted under,
// so a recursive page never continues as a one-level one.
type listCursor struct {
	Sort ListSort `json:"s"`
	Key  string   `json:"k"`
	ID   string   `json:"i"`

	Folder    string `json:"f,omitempty"`
	Delimited bool   `json:"d,omitempty"`
}

// listSpecFor compiles a query for a viewer.
func listSpecFor(v Viewer, q ListQuery) (listSpec, error) {
	scope, sort, status := q.Scope, q.Sort, q.Status
	if scope == "" {
		scope = ScopeAll
	}
	if sort == "" {
		sort = SortUpdated
	}
	if status == "" {
		status = StatusLive
	}
	switch {
	case !scope.Valid():
		return listSpec{}, fmt.Errorf("%w: scope %q is not one of %v", ErrInvalid, scope, ListScopes)
	case !sort.Valid():
		return listSpec{}, fmt.Errorf("%w: sort %q is not one of %v", ErrInvalid, sort, ListSorts)
	case status != StatusLive && status != StatusTrashed:
		return listSpec{}, fmt.Errorf("%w: status %q is neither live nor trashed", ErrInvalid, status)
	case q.Kind != "" && !q.Kind.Valid():
		return listSpec{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, q.Kind, artifacts.Kinds)
	case v.Record == "" && !v.Admin:
		return listSpec{}, fmt.Errorf("%w: a listing names who is viewing it", ErrInvalid)
	case q.Delimiter != "" && q.Delimiter != ListDelimiter:
		return listSpec{}, fmt.Errorf("%w: the delimiter is %q, the one separator between "+
			"folder names, or absent", ErrInvalid, ListDelimiter)
	case q.Prefix != "" && q.Folder != "":
		return listSpec{}, fmt.Errorf("%w: name the folder by its prefix or by its id, not both",
			ErrInvalid)
	case q.Prefix != "" && !strings.HasSuffix(q.Prefix, ListDelimiter):
		return listSpec{}, fmt.Errorf("%w: the prefix %q names a folder and so ends with %q",
			ErrInvalid, q.Prefix, ListDelimiter)
	case q.Requester != nil && (q.Requester.Admin || q.Requester.Record == ""):
		return listSpec{}, fmt.Errorf("%w: a requester is a person or a seat an agent acts for, "+
			"named by their record and never the admin path", ErrInvalid)
	}
	s := listSpec{sort: sort, status: status, record: v.Record, limit: pageLimit(q.MaxKeys),
		viewers: []Viewer{v}, prefix: q.Prefix, folder: q.Folder,
		delimited: q.Delimiter == ListDelimiter}
	if q.Requester != nil {
		s.viewers = append(s.viewers, *q.Requester)
	}
	s.where = append(s.where, `a.status = ?`)
	s.args = append(s.args, string(status))
	if q.Kind != "" {
		s.where = append(s.where, `a.kind = ?`)
		s.args = append(s.args, string(q.Kind))
	}
	if text := strings.TrimSpace(q.Q); text != "" {
		s.where = append(s.where, `instr(lower(a.title || ' ' || a.description || ' ' || a.search_text), lower(?)) > 0`)
		s.args = append(s.args, text)
	}
	if q.Pinned {
		s.where = append(s.where, `EXISTS (SELECT 1 FROM artifact_pins p WHERE p.artifact_id = a.id AND p.seat = ?)`)
		s.args = append(s.args, v.Record)
	}
	if item := strings.TrimSpace(q.WorkItem); item != "" {
		// THE ARTIFACT'S OWN HISTORY NAMES THE ITEM, read through the
		// (work_item, artifact_id) index: one probe per artifact the page
		// considers, never a scan of the history.
		// AN APPLIED ROW ONLY: a write the log refused (a link to an
		// artifact trashed before it applied, a publish over a moved head)
		// made nothing for the item, and its row says so.
		s.where = append(s.where, `EXISTS (SELECT 1 FROM artifact_history h
			WHERE h.work_item = ? AND h.artifact_id = a.id AND h.outcome = ?)`)
		s.args = append(s.args, item, string(OutcomeApplied))
	}
	// THE ONE VISIBILITY PREDICATE, the total's and every folder count's
	// too ([visibleClause]), for the viewer and the requester alike.
	visible, visibleArgs := everyClause(s.viewers, visibleClause)
	s.where = append(s.where, visible)
	s.args = append(s.args, visibleArgs...)
	switch scope {
	case ScopeMine:
		s.where = append(s.where, `a.owner = ?`)
		s.args = append(s.args, v.Record)
	case ScopeShared:
		shared, sharedArgs := sharedClause(v, artifactRelation)
		s.where = append(s.where, `a.owner != ?`, shared)
		s.args = append(append(s.args, v.Record), sharedArgs...)
	case ScopeCompany:
		s.where = append(s.where, `EXISTS (SELECT 1 FROM artifact_shares c
			WHERE c.artifact_id = a.id AND c.audience_kind = ? AND c.role IN (`+
			placeholders(len(artifacts.Roles))+`))`)
		s.args = append(s.args, string(artifacts.AudienceCompany))
		for _, r := range artifacts.Roles {
			s.args = append(s.args, string(r))
		}
	case ScopeAgents:
		s.where = append(s.where, `a.owner_kind = ?`)
		s.args = append(s.args, string(iam.ActorAgent))
	case ScopeAll:
	}
	if q.ContinuationToken != "" {
		after, err := decodeListCursor(q.ContinuationToken, sort)
		if err != nil {
			return listSpec{}, err
		}
		if after.Delimited != s.delimited {
			return listSpec{}, fmt.Errorf("%w: the token continues a %s listing", ErrBadCursor,
				map[bool]string{true: "one-level", false: "recursive"}[after.Delimited])
		}
		s.after = &after
	}
	return s, nil
}

// placed is where a page sits in the viewer's folder tree: the folder it lists
// ("" for the top level) and the path the page echoes as its prefix.
type placed struct {
	folder, prefix string
}

// place resolves the folder a page lists among the folders tree holds (every
// one the page's viewers see): the token's folder on a later page, otherwise
// the query's id or prefix.
func (s listSpec) place(tree *folderTree) (placed, error) {
	ref := s.folder
	if s.after != nil {
		// A LATER PAGE LISTS THE TOKEN'S FOLDER, by id, whatever its path
		// has become; it is still one the viewer must see now. A query that
		// names ANOTHER folder beside the token is refused rather than
		// answered with the token's folder under the query's words: by id,
		// or by a path that names a folder now. A path that names none is
		// the token's folder renamed since the first page, which the token
		// governs.
		named := s.folder
		if named == "" && s.prefix != "" {
			if found, err := tree.resolvePath(s.prefix); err == nil && len(found) == 1 {
				named = found[0].id
			}
		}
		if named != "" && named != s.after.Folder {
			return placed{}, fmt.Errorf("%w: the continuation token continues another folder "+
				"than the one this query names; send the prefix it was minted under, or "+
				"drop the token to start again", ErrBadCursor)
		}
		ref = s.after.Folder
	} else if s.prefix != "" {
		found, err := tree.resolvePath(s.prefix)
		switch {
		case err != nil:
			return placed{}, err
		case len(found) == 0:
			return placed{}, fmt.Errorf("%w: %q", ErrFolderNotFound, s.prefix)
		case len(found) > 1:
			return placed{}, ambiguous(tree, s.prefix, found)
		}
		ref = found[0].id
	}
	if ref == "" {
		return placed{}, nil
	}
	if tree.live(ref) == nil {
		return placed{}, fmt.Errorf("%w: %s", ErrFolderNotFound, ref)
	}
	return placed{folder: ref, prefix: tree.path(ref)}, nil
}

// ambiguous is the refusal of a path naming several folders, every one as
// the tree shows it.
func ambiguous(tree *folderTree, path string, found []*folderRow) *FolderAmbiguous {
	out := &FolderAmbiguous{Path: path}
	for _, f := range found {
		out.Candidates = append(out.Candidates, tree.folder(f))
	}
	return out
}

// jsonIDs is ids as the one JSON array a `json_each(?)` term binds: a set of
// any size in one parameter, where an IN list of up to [artifacts.MaxFolders]
// placeholders would pass the store's bound on parameters.
func jsonIDs(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	data, _ := json.Marshal(ids)
	return string(data)
}

// visibleTree is every live folder all of viewers see, read in tx: a folder's
// visibility is its top-level folder's relations ([folderVisibleClause]).
func visibleTree(ctx context.Context, tx *sql.Tx, viewers []Viewer) (*folderTree, error) {
	visible, args := everyClause(viewers, folderVisibleClause)
	return readFolderTree(ctx, tx, `f.status = ? AND `+visible,
		append([]any{string(FolderLive)}, args...)...)
}

// order is the listing's ORDER BY, its keyset predicate past a cursor, and the
// key a row's cursor is cut from.
func (s listSpec) order() (orderBy, past string, pastArgs []any) {
	switch s.sort {
	case SortCreated:
		orderBy = `a.created_at DESC, a.id DESC`
		if s.after != nil {
			past = `(a.created_at < ? OR (a.created_at = ? AND a.id < ?))`
		}
	case SortTitle:
		orderBy = `a.title ASC, a.id ASC`
		if s.after != nil {
			past = `(a.title > ? OR (a.title = ? AND a.id > ?))`
		}
	case SortOpened:
		orderBy = `opened DESC, a.id DESC`
		if s.after != nil {
			past = `(opened < ? OR (opened = ? AND a.id < ?))`
		}
	default:
		orderBy = `a.updated_at DESC, a.id DESC`
		if s.after != nil {
			past = `(a.updated_at < ? OR (a.updated_at = ? AND a.id < ?))`
		}
	}
	if s.after != nil {
		var key any = s.after.Key
		if s.sort != SortTitle {
			// decodeListCursor has already held an instant key to an
			// integer, so this parse cannot fail.
			n, _ := strconv.ParseInt(s.after.Key, 10, 64)
			key = n
		}
		pastArgs = []any{key, key, s.after.ID}
	}
	return orderBy, past, pastArgs
}

// run reads the page, its total, its folders and its cursor in one
// transaction.
func (s listSpec) run(ctx context.Context, tx *sql.Tx) (ListPage, error) {
	tree, err := visibleTree(ctx, tx, s.viewers)
	if err != nil {
		return ListPage{}, err
	}
	at, err := s.place(tree)
	if err != nil {
		return ListPage{}, err
	}
	conds, condArgs := append([]string(nil), s.where...), append([]any(nil), s.args...)
	switch {
	case s.delimited && at.folder == "":
		// THE VIEWER'S TOP LEVEL: filed in no folder, or in one they cannot
		// see, so a folder never hides an artifact its reader may open.
		conds = append(conds, `(a.folder_id = '' OR a.folder_id NOT IN (SELECT value FROM json_each(?)))`)
		condArgs = append(condArgs, jsonIDs(tree.ids()))
	case s.delimited:
		conds = append(conds, `a.folder_id = ?`)
		condArgs = append(condArgs, at.folder)
	case at.folder != "":
		conds = append(conds, `a.folder_id IN (SELECT value FROM json_each(?))`)
		condArgs = append(condArgs, jsonIDs(tree.subtree(at.folder)))
	}
	from := ` FROM artifacts a
		LEFT JOIN artifact_reads r ON r.artifact_id = a.id AND r.seat = ?`
	where := ` WHERE ` + strings.Join(conds, ` AND `)
	baseArgs := append([]any{s.record}, condArgs...)
	var total int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*)`+from+where, baseArgs...).
		Scan(&total); err != nil {
		return ListPage{}, fmt.Errorf("artifactsdomain: count the listing: %w", err)
	}
	orderBy, past, pastArgs := s.order()
	query := `SELECT ` + headColumns + `, COALESCE(r.opened_at, 0) AS opened,
		EXISTS (SELECT 1 FROM artifact_pins p WHERE p.artifact_id = a.id AND p.seat = ?)` +
		from + where
	args := append([]any{s.record}, baseArgs...)
	if past != "" {
		query += ` AND ` + past
		args = append(args, pastArgs...)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY `+orderBy+` LIMIT ?`,
		append(args, s.limit+1)...)
	if err != nil {
		return ListPage{}, fmt.Errorf("artifactsdomain: read the listing: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := ListPage{Prefix: at.prefix, Contents: []ListItem{}, Total: total}
	if s.delimited {
		page.Delimiter = ListDelimiter
	}
	var keys []string
	for rows.Next() {
		var opened int64
		var pinned int
		head, err := scanHead(rows, &opened, &pinned) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return ListPage{}, fmt.Errorf("artifactsdomain: read the listing: %w", err)
		}
		item := ListItem{Head: head, Pinned: pinned != 0}
		if opened > 0 {
			item.OpenedAt = store.DecodeTime(opened)
		}
		// A FOLDER THE VIEWER CANNOT SEE IS NEVER NAMED, by its path or by
		// its id: the row reads as at the top level they see, where the
		// listing places it, and two rows sharing an id would otherwise say
		// they sit together somewhere hidden.
		if tree.live(head.Folder) != nil {
			item.FolderPath = tree.path(head.Folder)
		} else {
			item.Folder = ""
		}
		page.Contents = append(page.Contents, item)
		switch s.sort {
		case SortCreated:
			keys = append(keys, strconv.FormatInt(store.EncodeTime(head.CreatedAt), 10))
		case SortTitle:
			keys = append(keys, head.Title)
		case SortOpened:
			keys = append(keys, strconv.FormatInt(opened, 10))
		default:
			keys = append(keys, strconv.FormatInt(store.EncodeTime(head.UpdatedAt), 10))
		}
	}
	if err := rows.Err(); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return ListPage{}, err
	}
	if len(page.Contents) > s.limit {
		page.Contents = page.Contents[:s.limit]
		last := page.Contents[s.limit-1]
		cursor, err := json.Marshal(listCursor{Sort: s.sort, Key: keys[s.limit-1], //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			ID: last.ID.String(), Folder: at.folder, Delimited: s.delimited})
		if err != nil {
			return ListPage{}, fmt.Errorf("artifactsdomain: encode the listing cursor: %w", err)
		}
		page.NextContinuationToken = base64.RawURLEncoding.EncodeToString(cursor)
		page.IsTruncated = true
	}
	if s.delimited && s.after == nil {
		if page.CommonPrefixes, err = s.commonPrefixes(ctx, tx, tree, at); err != nil {
			return ListPage{}, err
		}
	}
	page.KeyCount = len(page.Contents) + len(page.CommonPrefixes)
	ids := make([]string, len(page.Contents))
	for i, item := range page.Contents {
		ids[i] = item.ID.String()
	}
	shares, err := readShares(ctx, tx, ids)
	if err != nil {
		return ListPage{}, err
	}
	for i := range page.Contents {
		page.Contents[i].Shares = shares[ids[i]]
	}
	return page, nil
}

// commonPrefixes is a one-level page's child folders, A to Z, each with what
// sits directly in it that the page's viewers can see: the artifacts of the
// page's status by the listing's own predicate, and its live folders (every
// one of which the viewers see, since a tree is seen whole or not at all).
func (s listSpec) commonPrefixes(ctx context.Context, tx *sql.Tx, tree *folderTree,
	at placed) ([]CommonPrefix, error) {

	kids := tree.kids[at.folder]
	if len(kids) == 0 {
		return nil, nil
	}
	ids := make([]string, len(kids))
	for i, f := range kids {
		ids[i] = f.id
	}
	counts, err := folderCounts(ctx, tx, s.viewers, s.status, ids)
	if err != nil {
		return nil, err
	}
	out := make([]CommonPrefix, len(kids))
	for i, f := range kids {
		out[i] = CommonPrefix{
			Prefix: at.prefix + f.name + ListDelimiter, Folder: f.id, Name: f.name,
			Visibility: visibilityOf(tree.shares[f.root]), Items: counts[f.id],
			Folders: len(tree.kids[f.id]),
		}
	}
	return out, nil
}

// folderCounts is how many artifacts of status every one of viewers can see
// sit directly in each folder of ids (nil for every folder), by the listing's
// one predicate ([visibleClause]) in the caller's transaction.
func folderCounts(ctx context.Context, tx *sql.Tx, viewers []Viewer, status Status,
	ids []string) (map[string]int, error) {

	visible, args := everyClause(viewers, visibleClause)
	query := `SELECT a.folder_id, COUNT(*) FROM artifacts a WHERE a.status = ? AND ` + visible
	args = append([]any{string(status)}, args...)
	if ids != nil {
		query += ` AND a.folder_id IN (SELECT value FROM json_each(?))`
		args = append(args, jsonIDs(ids))
	}
	rows, err := tx.QueryContext(ctx, query+` GROUP BY a.folder_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: count what each folder holds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var folder string
		var n int
		if err := rows.Scan(&folder, &n); err != nil {
			return nil, fmt.Errorf("artifactsdomain: count what each folder holds: %w", err)
		}
		out[folder] = n
	}
	return out, rows.Err()
}

// decodeListCursor reads a listing cursor, refusing one minted for another
// order.
func decodeListCursor(raw string, sort ListSort) (listCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return listCursor{}, fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	var c listCursor
	if err := json.Unmarshal(data, &c); err != nil || c.ID == "" || c.Sort != sort {
		return listCursor{}, ErrBadCursor
	}
	if sort != SortTitle {
		if _, err := strconv.ParseInt(c.Key, 10, 64); err != nil {
			return listCursor{}, ErrBadCursor
		}
	}
	return c, nil
}

// pageLimit is a page size a caller asked for, defaulted and bounded.
func pageLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	}
	return limit
}

// intCursor reads a cursor that is one integer (a version number or a history
// position), zero for the first page.
func intCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil || n <= 0 {
		return 0, ErrBadCursor
	}
	return n, nil
}

// encodeIntCursor is [intCursor]'s inverse.
func encodeIntCursor(n int64) string {
	data, _ := json.Marshal(n)
	return base64.RawURLEncoding.EncodeToString(data)
}
