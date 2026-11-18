package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
)

// A REPOSITORY'S MIRRORS AND ITS IMPORT, each a remote at another host:
//   - a PULL MIRROR ([RepoSettings.MirrorFrom]) is imported first whenever it becomes one
//     ([repoRemotes.startsImport]), then moves only by its home's syncs: a push or a change's
//     write is refused [RefusalRepoMirrored] ([ChangeRefusalRepoMirrored]);
//   - a PUSH MIRROR ([RepoSettings.MirrorTo]) pushes as a seat that reads the repository
//     ([MirrorSeatRefusal]), never a bot, and never seats' branches ([gitref.PatternReachesSeats]);
//   - an IMPORT ([CreateRepo.Import]) fills it at its create, [RepoStatusImporting] until done, taking
//     no write, not even an expunge ([RefusalRepoImporting]).
//
// Only a person or the configuration sets one, as each dials another host or sends it the company's code
// (an agent is refused not_permitted whatever its grant: U49 a, [lifecycleRefusal]), never on a scratch
// repository, with a credential code.remote_credentials declares for that host ([remoteCredentialRefusal]).

// BASE FORMAT ([RecordVersion]): every build that meets these mirror and import fields reads them.

// MirrorSource is the repository a PULL MIRROR follows ([RepoSettings.MirrorFrom]):
// the one it is imported from when it becomes a mirror, and synced from after.
type MirrorSource struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what Gitlet fetches with: one whole ${VAR}
	// reference, or "" to fetch anonymously.
	Credential string `json:"credential,omitempty"`
}

// MirrorTarget is the repository a PUSH MIRROR pushes to ([RepoSettings.MirrorTo]).
type MirrorTarget struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what Gitlet pushes with: one whole ${VAR}
	// reference to the As seat's own token at that host — required, since a
	// push authenticates.
	Credential string `json:"credential"`
	// As is the seat whose identity the push carries at the other host, by its
	// handle: a seat that reads the repository ([MirrorSeatRefusal]).
	As string `json:"as"`
	// Refs are the ref patterns it pushes (gitref's protection patterns), each
	// once, at most [MaxMirrorRefs], none reaching a seat's own namespace
	// ([gitref.PatternReachesSeats]). None means the default branch and
	// refs/tags/*, read at each push so a rename of the branch moves with it.
	Refs []string `json:"refs,omitempty"`
}

// ImportSource is the repository a repository is filled from when it is
// created ([CreateRepo.Import]).
type ImportSource struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what Gitlet fetches with: one whole ${VAR}
	// reference, or "" to fetch anonymously.
	Credential string `json:"credential,omitempty"`
}

// invalid is why m is no pull mirror a record carries, or "".
func (m MirrorSource) invalid() string { return remoteInvalid(m.URL, m.Credential, false) }

// invalid is why i is no import a record carries, or "".
func (i ImportSource) invalid() string { return remoteInvalid(i.URL, i.Credential, false) }

// invalid is why m is no push mirror a record carries, or "": its remote, its
// seat's handle, and its refs.
func (m MirrorTarget) invalid() string {
	if why := remoteInvalid(m.URL, m.Credential, true); why != "" {
		return why
	}
	switch {
	case m.As == "":
		return "as: a push mirror pushes as a seat's own identity at the other host, never a bot: name the seat"
	case !org.ValidHandle(m.As):
		return fmt.Sprintf("as: %q is no seat's handle", m.As)
	case len(m.Refs) > MaxMirrorRefs:
		return fmt.Sprintf("refs: %d patterns, past the %d one push mirror may name", len(m.Refs), MaxMirrorRefs)
	}
	for i, pattern := range m.Refs {
		switch {
		case !gitref.ValidPattern(pattern):
			return fmt.Sprintf("refs[%d]: %q is not refs/heads/<glob> or refs/tags/<glob>", i, pattern)
		case gitref.PatternReachesSeats(pattern):
			return fmt.Sprintf("refs[%d]: %s reaches %s<handle>/, where each seat's own branches wait for a "+
				"person, and a mirror publishes what the company merged", i, pattern, gitref.SeatsRoot)
		case slices.Contains(m.Refs[:i], pattern):
			return fmt.Sprintf("refs[%d]: %s is named twice", i, pattern)
		}
	}
	return ""
}

// equal compares two push mirrors field by field, the refs in their order.
func (m MirrorTarget) equal(o MirrorTarget) bool {
	return m.URL == o.URL && m.Credential == o.Credential && m.As == o.As && slices.Equal(m.Refs, o.Refs)
}

// remoteInvalid is why a remote is none a record carries, or "": a URL of at most
// [MaxRemoteURLBytes] the dial reads ([gitref.ParseRemoteURL]), and a credential
// (required where needCredential) that is ONE WHOLE ${VAR} REFERENCE of at most
// [MaxCredentialRefBytes], nothing around it, resolved only at the dial (M6-D17):
// a token would sit at rest in every settings record and every backup. The
// sentence never quotes the credential: one that is not a reference is a token.
func remoteInvalid(url, credential string, needCredential bool) string {
	switch {
	case url == "":
		return "url: a remote names the repository at the other host, an https URL"
	case len(url) > MaxRemoteURLBytes:
		return fmt.Sprintf("url: %d bytes, past the %d a remote's URL may hold", len(url), MaxRemoteURLBytes)
	}
	if _, err := gitref.ParseRemoteURL(url); err != nil {
		return "url: " + err.Error()
	}
	switch {
	case credential == "" && needCredential:
		return "credential: a push authenticates, so a push mirror names the ${VAR} that holds its token"
	case credential == "":
		return ""
	case len(credential) > MaxCredentialRefBytes:
		return fmt.Sprintf("credential: %d bytes, past the %d a credential's reference may hold", len(credential),
			MaxCredentialRefBytes)
	}
	if name, whole := envref.Whole(credential); !whole || credential != "${"+name+"}" {
		return "credential: a credential is one whole ${VAR} reference and nothing else — never the token " +
			"itself, which every record of the repository's settings would carry"
	}
	return ""
}

// MirrorSeatRefusal is why the seat as may not be the one a push mirror of
// repo pushes as, or "": a roster seat that reads repo as it stands — a
// person's seat every repository where its holder uses Gitlet (ADR-0088,
// ADR-0110), an agent's what its grant reads ([GrantOf]). noGitlet is the
// identity directory's answer for a person's seat, read by the caller: held by
// nobody, by a person who may not act, or by one not given the gitlet app.
//
// THE ONE JUDGEMENT of a push mirror's seat: a person's settings change and
// create; code.repos through config's CodeRepo.mirrorSeatReads, held to this
// one on the chart alone (a configuration reads no identity directory); and the
// mirror before each push, since the chart may take the seat's read away and an
// administrator the person's app.
func MirrorSeatRefusal(as string, repo RepoRef, roster Roster, noGitlet bool) string {
	if roster == nil {
		return "this node holds no chart to judge a push mirror's seat by"
	}
	s, ok := roster.Seat(as)
	switch {
	case !ok:
		return fmt.Sprintf("mirror_to.as: %s is no seat of the chart, and a push mirror pushes as a seat's own "+
			"identity at the other host", as)
	case s.Human && noGitlet:
		return fmt.Sprintf("mirror_to.as: nobody holding %s uses Gitlet now — the person holding it is not "+
			"given the gitlet permission or may not act, or nobody holds it — and a push mirror publishes only "+
			"what the seat it pushes as reads (ADR-0110): an administrator gives them Gitlet, or the mirror "+
			"names a seat that reads %s", as, repo.Name)
	case s.Human:
		return ""
	}
	if g := GrantOf(Principal{Kind: PrincipalAgent, Seat: as}, repo, roster); !g.MayRead() {
		return fmt.Sprintf("mirror_to.as: seat %s does not read %s, and a push mirror publishes only what the "+
			"seat it pushes as reads — %s", as, repo.Name, g.ReadRefusal())
	}
	return ""
}

// remoteCredentialRefusal is why req names a credential that code.remote_credentials
// (declared) does not declare for its remote's host, or "": every remote a
// create or a settings change carries a credential for, whoever asks.
//
// The home node resolves a credential from its secret store, then its
// environment, and sends the value to the remote's host, so an unbounded
// reference would let any member who may create a repository send any value it
// resolves to a host of their choosing. An administrator declares which
// variable may go to which host; this holds a request to it before anything is
// recorded; the dial holds it again under its node's table then
// (config.CodeRemoteCredentials.Refusal, the one judgement). A seat is refused
// every mirror and import before this ([mirrorsAsked]).
func remoteCredentialRefusal(req LifecycleRequest, declared config.CodeRemoteCredentials) string {
	type remote struct{ field, url, credential string }
	var asked []remote
	switch {
	case req.Kind == LifecycleCreate:
		if i := req.Import; i != nil {
			asked = append(asked, remote{"import", i.URL, i.Credential})
		}
		if m := req.Create.MirrorFrom; m != nil {
			asked = append(asked, remote{"mirror_from", m.URL, m.Credential})
		}
		if m := req.Create.MirrorTo; m != nil {
			asked = append(asked, remote{"mirror_to", m.URL, m.Credential})
		}
	case req.Kind == LifecycleSettings && req.Change != nil:
		// A MIRROR NAMED WITH NO URL IS REMOVED, and sends nothing anywhere.
		if m := req.Change.MirrorFrom; m != nil && m.URL != "" {
			asked = append(asked, remote{"mirror_from", m.URL, m.Credential})
		}
		if m := req.Change.MirrorTo; m != nil && m.URL != "" {
			asked = append(asked, remote{"mirror_to", m.URL, m.Credential})
		}
	}
	for _, r := range asked {
		if why := declared.Refusal(r.url, r.credential); why != "" {
			return r.field + ".credential: " + why
		}
	}
	return ""
}

// ImportingDetail is the sentence of a write refused repo_importing. ONE
// SPELLING for every place that refuses one (ADR-0008), since a person's git
// prints it as the reason a push was refused.
func ImportingDetail(name string) string {
	return fmt.Sprintf("repository %s is being imported from another host, and takes no write until its "+
		"import is done: fetch it meanwhile, and push once it is", name)
}

// repoRemotes is what a decide reads of a repository's other hosts beside its
// row, which no RepoRef carries whole: its mirrors, which every record of its
// settings carries, and whether its import is still to be done, which a
// restore brings it back to.
type repoRemotes struct {
	from *MirrorSource
	to   *MirrorTarget
	// importPending is a repository its create imported whose import is not
	// done: import_source set and imported_at none.
	importPending bool
}

// restoredStatus is the status a restore with the settings s brings the
// repository back in: IMPORTING where its import is still to be done — the
// import resumes from what of it landed — and otherwise what s says.
func (r repoRemotes) restoredStatus(s RepoSettings) RepoStatus {
	if r.importPending {
		return RepoStatusImporting
	}
	return s.status()
}

// startsImport is the import a settings record carrying s starts on a repository whose
// mirrors are r: from s's pull mirror's source where the repository follows none or another
// URL, else nil (a credential changed alone is the same source). The apply starts it only on an
// ACTIVE repository (mirrorImportSQL): an archived one becomes no pull mirror, and one being
// imported keeps its import's source.
//
// M6-D16: A PULL MIRROR IS IMPORTED FIRST, whenever it becomes one: an import
// lands the whole history in chunks resuming from the rows (CompactPackBytes
// packs before the refs needing them, MaxImportRefs refs, each at the next
// import_seq) behind the importing fence that keeps writes, the compactor and an
// expunge off what is half landed; a sync's fetch gets one push's deadline
// (code.limits.max_push_bytes, gitproc.RemoteDeadline), too little for a long history.
func (r repoRemotes) startsImport(s RepoSettings) *ImportSource {
	switch {
	case s.MirrorFrom == nil:
		return nil
	case r.from != nil && r.from.URL == s.MirrorFrom.URL:
		return nil
	}
	return &ImportSource{URL: s.MirrorFrom.URL, Credential: s.MirrorFrom.Credential}
}

// mirrorFrom is the pull mirror as a record carries it: a copy.
func (r repoRemotes) mirrorFrom() *MirrorSource {
	if r.from == nil {
		return nil
	}
	m := *r.from
	return &m
}

// mirrorTo is the push mirror as a record carries it: a copy, its refs too.
func (r repoRemotes) mirrorTo() *MirrorTarget {
	if r.to == nil {
		return nil
	}
	m := *r.to
	m.Refs = slices.Clone(r.to.Refs)
	return &m
}

// remotesIn reads the repository id's mirrors and whether its import is still
// to be done, in tx — a row this build cannot read is the log's fault, never a
// repository with no mirror.
func remotesIn(ctx context.Context, tx *sql.Tx, id string) (repoRemotes, error) {
	var (
		out      repoRemotes
		from, to string
		pending  bool
	)
	err := tx.QueryRowContext(ctx, remotesSQL, id).Scan(&from, &to, &pending)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoRemotes{}, nil
	case err != nil:
		return repoRemotes{}, fmt.Errorf("gitlet: read repository %s's mirrors: %w", id, err)
	}
	if out.from, err = remoteOf[MirrorSource](from); err != nil {
		return repoRemotes{}, fmt.Errorf("gitlet: read repository %s's mirror_from: %w", id, err)
	}
	if out.to, err = remoteOf[MirrorTarget](to); err != nil {
		return repoRemotes{}, fmt.Errorf("gitlet: read repository %s's mirror_to: %w", id, err)
	}
	out.importPending = pending
	return out, nil
}

// remoteColumns is the settings' mirrors and an import as code_repos holds them
// in mirror_from, mirror_to and import_source: each its JSON, or "" for none.
//
// EACH MARSHALLED AS ITS OWN TYPE, never through one generic helper: the cache's
// writer gate (cache.TestNothingButSyncWritesACache) walks this package and
// refuses a value it cannot type handed to a callee it cannot type, as a type
// parameter's *T handed to json.Marshal is.
func remoteColumns(s RepoSettings, imports *ImportSource) (from, to, imported string, err error) {
	var b []byte
	if s.MirrorFrom != nil {
		if b, err = json.Marshal(s.MirrorFrom); err != nil {
			return "", "", "", fmt.Errorf("gitlet: encode mirror_from: %w", err)
		}
		from = string(b)
	}
	if s.MirrorTo != nil {
		if b, err = json.Marshal(s.MirrorTo); err != nil {
			return "", "", "", fmt.Errorf("gitlet: encode mirror_to: %w", err)
		}
		to = string(b)
	}
	if imports != nil {
		if b, err = json.Marshal(imports); err != nil {
			return "", "", "", fmt.Errorf("gitlet: encode import_source: %w", err)
		}
		imported = string(b)
	}
	return from, to, imported, nil
}

// remoteOf reads a column [remoteColumns] wrote: nil for the empty string.
func remoteOf[T any](column string) (*T, error) {
	if column == "" {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal([]byte(column), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// remotesSQL reads a repository's mirrors and whether its import is pending,
// by its primary key — registered in plan_test.go.
const remotesSQL = `SELECT mirror_from, mirror_to, import_source != '' AND imported_at = 0
	FROM code_repos WHERE id = ?`

// MirroredDetail is the sentence of a write refused repo_mirrored, naming where
// its writes go by who set the mirror (code.repos, or a person's tool). One
// spelling for every place that refuses one, as [ImportingDetail] is.
func MirroredDetail(name string, managedBy ManagedBy) string {
	if managedBy == ManagedTools {
		return fmt.Sprintf("repository %s is a pull mirror: it moves only by Gitlet's syncs from its "+
			"source, and takes no push and no change of its own — push to its source, or ask a person to "+
			"remove its mirror_from", name)
	}
	return fmt.Sprintf("repository %s is a pull mirror (code.repos mirror_from): it moves only by Gitlet's "+
		"syncs from its source, and takes no push and no change of its own — push to its source", name)
}
