package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
)

// A REPOSITORY'S MIRRORS AND ITS IMPORT: what of another host a repository
// follows, publishes to, or is filled from — each a remote at another host, a
// URL read by the one rule the dial reads it by ([gitref.ParseRemoteURL]) and
// a credential that is ONE WHOLE ${VAR} REFERENCE, recorded as the reference
// and resolved only when the remote is dialed, from the secret store and then
// the environment (M6-D17). A token written into one would be a secret at rest
// on every record of the repository's settings, which every data node keeps
// and every backup carries; the reference is a pointer, as a configuration's
// is.
//
//   - A PULL MIRROR ([RepoSettings.MirrorFrom]) is imported from its source
//     when it is created — it is [RepoStatusImporting] until its import is
//     done — and then moves only by the syncs its home makes from the source:
//     every push and every write of a change's lifecycle is refused
//     [RefusalRepoMirrored] ([ChangeRefusalRepoMirrored]).
//   - A PUSH MIRROR ([RepoSettings.MirrorTo]) pushes the refs it names to a
//     repository at another host, as one seat's own identity there — a seat
//     that reads the repository ([MirrorSeatRefusal]), never a bot — and never
//     a seat's own branches ([gitref.PatternReachesSeats]).
//   - An IMPORT ([CreateRepo.Import]) fills a repository from another host
//     when it is created, and is read by its create alone: it is
//     [RepoStatusImporting] until the import is done, and takes no write
//     meanwhile ([RefusalRepoImporting]) — not even an expunge.
//
// WHO SETS ONE IS A PERSON, OR THE CONFIGURATION (U49 a): a pull mirror and an
// import dial another host, and a push mirror sends the company's code to one,
// so an agent asking for any of them is refused not_permitted whatever its
// grant on the repository ([lifecycleRefusal]). A scratch repository takes
// none: it is its seat's own experiment.
//
// BASE FORMAT, AS A STATED CONDITION: no build that carries the code domain
// has run without these fields — none of the domain is on main, so no image
// and no release holds them — and while that is so, every build that meets a
// record carrying them reads them. Once a build without them has run, each op
// whose payload carries settings (create, settings, restore) takes {Name:
// "RepoSettings.MirrorFrom", Since: 2, Op: <the op>, Path: []string{"mutation",
// "settings", "mirror_from"}} and the same for "RepoSettings.MirrorTo" at
// "mirror_to", and the create takes {Name: "CreateRepo.Import", Since: 2, Op:
// "create", Path: []string{"mutation", "import"}}, in versionedFields — with
// RecordVersion at 2, their lines under "What each version added" and a
// Carrying record in the statelogtest candidate — since such a build would
// apply a repository's settings without its mirrors, and a create without its
// import, and the rows it wrote would read as none on that node for ever.

// MirrorSource is the repository a PULL MIRROR follows ([RepoSettings.MirrorFrom]):
// the one it is imported from when it is created, and synced from after.
type MirrorSource struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what the code host fetches with: one whole ${VAR}
	// reference, or "" to fetch anonymously.
	Credential string `json:"credential,omitempty"`
}

// MirrorTarget is the repository a PUSH MIRROR pushes to ([RepoSettings.MirrorTo]).
type MirrorTarget struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what the code host pushes with: one whole ${VAR}
	// reference to the As seat's own token at that host — required, since a
	// push authenticates.
	Credential string `json:"credential"`
	// As is the seat whose identity the push carries at the other host, by its
	// handle: a seat that reads the repository ([MirrorSeatRefusal]).
	As string `json:"as"`
	// Refs are the ref patterns it pushes (gitref's protection patterns), each
	// once and at most [MaxMirrorRefs], none reaching a seat's own namespace
	// ([gitref.PatternReachesSeats]) — and none for the default: the
	// repository's default branch and refs/tags/*, read against the default
	// branch the repository has when it pushes, so a rename of that branch
	// moves what it pushes with it.
	Refs []string `json:"refs,omitempty"`
}

// ImportSource is the repository a repository is filled from when it is
// created ([CreateRepo.Import]).
type ImportSource struct {
	// URL is the repository at the other host: an https URL the dial reads
	// ([gitref.ParseRemoteURL]), at most [MaxRemoteURLBytes].
	URL string `json:"url"`
	// Credential is what the code host fetches with: one whole ${VAR}
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

// remoteInvalid is why a remote — its URL and its credential, required where
// needCredential says so — is none a record carries, or "": a URL at most
// [MaxRemoteURLBytes] that the dial reads ([gitref.ParseRemoteURL]), and a
// credential that is ONE WHOLE ${VAR} REFERENCE of at most
// [MaxCredentialRefBytes] — with nothing around it, so the reference a record
// carries is the one a resolve reads. The sentence never quotes the
// credential: one that is not a reference is a token.
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
// repo pushes as, or "": a seat roster holds that reads the repository — a
// PERSON'S seat reads every one (ADR-0058), and an agent's what its grant on it
// reads ([GrantOf]), judged on repo as it stands, its owner and its visibility.
//
// THE ONE JUDGEMENT of a push mirror's seat: a person's settings change and a
// person's create judge it here, a repository code.repos declares is held to
// it by the configuration's own reading of the same rule (config's
// CodeRepo.mirrorSeatReads, which the engine holds to this one), and a push
// mirror asks it again before each push, of the chart it then holds, since a
// later revision of the chart may take the seat's read away.
func MirrorSeatRefusal(as string, repo RepoRef, roster Roster) string {
	if roster == nil {
		return "this node holds no chart to judge a push mirror's seat by"
	}
	s, ok := roster.Seat(as)
	switch {
	case !ok:
		return fmt.Sprintf("mirror_to.as: %s is no seat of the chart, and a push mirror pushes as a seat's own "+
			"identity at the other host", as)
	case s.Human:
		return ""
	}
	if g := GrantOf(Principal{Kind: PrincipalAgent, Seat: as}, repo, roster); !g.MayRead() {
		return fmt.Sprintf("mirror_to.as: seat %s does not read %s, and a push mirror publishes only what the "+
			"seat it pushes as reads — %s", as, repo.Name, g.ReadRefusal())
	}
	return ""
}

// ImportingDetail is the sentence of a write refused repo_importing: which
// repository, that it is being imported, and when it takes the write. ONE
// SPELLING for every place that refuses one (ADR-0008) — the push decide, the
// code host at a push's advertisement and at its request, an engine write's
// landing, an expunge's decide and a change's — where a person's git prints it
// as the reason a push was refused.
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
		return repoRemotes{}, fmt.Errorf("code: read repository %s's mirrors: %w", id, err)
	}
	if out.from, err = remoteOf[MirrorSource](from); err != nil {
		return repoRemotes{}, fmt.Errorf("code: read repository %s's mirror_from: %w", id, err)
	}
	if out.to, err = remoteOf[MirrorTarget](to); err != nil {
		return repoRemotes{}, fmt.Errorf("code: read repository %s's mirror_to: %w", id, err)
	}
	out.importPending = pending
	return out, nil
}

// remoteColumns is the settings' two mirrors and an import as their
// code_repos columns hold them — mirror_from, mirror_to and import_source:
// each its JSON, or the empty string for none.
//
// EACH MARSHALLED AS ITS OWN TYPE, never through one generic helper: every
// file under internal/code is walked by the cache's writer gate
// (cache.TestNothingButSyncWritesACache), which refuses a value it cannot
// type handed to a callee it cannot type either — and a *T of a type
// parameter, handed to json.Marshal, is both.
func remoteColumns(s RepoSettings, imports *ImportSource) (from, to, imported string, err error) {
	var b []byte
	if s.MirrorFrom != nil {
		if b, err = json.Marshal(s.MirrorFrom); err != nil {
			return "", "", "", fmt.Errorf("code: encode mirror_from: %w", err)
		}
		from = string(b)
	}
	if s.MirrorTo != nil {
		if b, err = json.Marshal(s.MirrorTo); err != nil {
			return "", "", "", fmt.Errorf("code: encode mirror_to: %w", err)
		}
		to = string(b)
	}
	if imports != nil {
		if b, err = json.Marshal(imports); err != nil {
			return "", "", "", fmt.Errorf("code: encode import_source: %w", err)
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

// MirroredDetail is the sentence of a write refused repo_mirrored: which
// repository, that it is a pull mirror — set by code.repos for a repository it
// declares, by a person's tool for one a tool made — and where its writes go
// instead. One spelling for every place that refuses one, as [ImportingDetail]
// is.
func MirroredDetail(name string, managedBy ManagedBy) string {
	if managedBy == ManagedTools {
		return fmt.Sprintf("repository %s is a pull mirror: it moves only by the code host's syncs from its "+
			"source, and takes no push and no change of its own — push to its source, or ask a person to "+
			"remove its mirror_from", name)
	}
	return fmt.Sprintf("repository %s is a pull mirror (code.repos mirror_from): it moves only by the code "+
		"host's syncs from its source, and takes no push and no change of its own — push to its source", name)
}
