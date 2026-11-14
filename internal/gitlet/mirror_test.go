package gitlet_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S MIRRORS AND ITS IMPORT (mirror.go): who may set one, what a
// record carries of its credential, and what a repository that moves only by
// what Gitlet fetches — a pull mirror, an import — refuses.

// mirrorSource, mirrorTarget and importSource are a remote at another host as
// a person names one: its URL, and its credential as the ${VAR} that holds it.
var (
	mirrorSource = &gitlet.MirrorSource{URL: "https://forge.example.com/acme/web.git", Credential: "${FORGE_TOKEN}"}
	mirrorTarget = &gitlet.MirrorTarget{URL: "https://forge.example.com/acme/web-public.git",
		Credential: "${FOUNDER_FORGE_TOKEN}", As: "founder"}
	importSource = &gitlet.ImportSource{URL: "https://forge.example.com/acme/api.git", Credential: "${FORGE_TOKEN}"}

	// forgeCredentials is the code.remote_credentials every harness node's
	// policy declares: the fixtures' variables, for the fixtures' forge.
	forgeCredentials = config.CodeRemoteCredentials{
		"FORGE_TOKEN":         {Hosts: []string{"forge.example.com"}},
		"FOUNDER_FORGE_TOKEN": {Hosts: []string{"forge.example.com"}},
	}
)

// A PERSON'S MIRROR OR IMPORT NAMES A CREDENTIAL code.remote_credentials
// DECLARES FOR ITS HOST: a person — who may create any project and set any
// mirror — asking to create a repository importing from, or mirroring, a
// remote with a variable the table does not declare (the node's own object
// store key, say), or with a declared variable toward a host it is not
// declared for, or setting either mirror of a repository so, is refused
// not_permitted naming the table and the host, the credential never quoted,
// and nothing is appended; the deciding node reads the table of its epoch at
// the request, so declaring the variable for that host lets the same request
// land. A mirror removed names no remote, and is never refused for it.
//
// Mutation: drop remoteCredentialRefusal's call from lifecycleCreate — the
// creates land, red; from decideOnRepo — the settings changes land, red.
// Read only the create's import in remoteCredentialRefusal — the mirrors'
// creates land, red.
func TestAPersonsRemoteCredentialIsOneDeclaredForItsHost(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "web", unitOwned("eng"))
	undeclared := &gitlet.ImportSource{URL: importSource.URL, Credential: "${AWS_SECRET_ACCESS_KEY}"}
	elsewhere := "https://attacker.example.com/x.git"
	cases := map[string]gitlet.LifecycleRequest{
		"an import naming an undeclared variable": withCreate(lcCreate(lcPerson, "leak-a", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) { r.Import = undeclared }),
		"an import sending a declared one elsewhere": withCreate(lcCreate(lcPerson, "leak-b", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) {
				r.Import = &gitlet.ImportSource{URL: elsewhere, Credential: "${FORGE_TOKEN}"}
			}),
		"a create's pull mirror sending one elsewhere": withCreate(lcCreate(lcPerson, "leak-c", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) {
				r.Create.MirrorFrom = &gitlet.MirrorSource{URL: elsewhere, Credential: "${FORGE_TOKEN}"}
			}),
		"a create's push mirror sending one elsewhere": withCreate(lcCreate(lcPerson, "leak-d", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) {
				r.Create.MirrorTo = &gitlet.MirrorTarget{URL: elsewhere, Credential: "${FOUNDER_FORGE_TOKEN}",
					As: "founder"}
			}),
		"a pull mirror set with an undeclared variable": lcSettings(lcPerson, id, gitlet.RepoChange{
			MirrorFrom: &gitlet.MirrorSource{URL: mirrorSource.URL, Credential: "${CREWLET_API_TOKEN_FOUNDER}"}}),
		"a push mirror set to send one elsewhere": lcSettings(lcPerson, id, gitlet.RepoChange{
			MirrorTo: &gitlet.MirrorTarget{URL: elsewhere, Credential: "${FOUNDER_FORGE_TOKEN}", As: "founder"}}),
	}
	before := n.end()
	for what, req := range cases {
		got := n.life(req)
		if got.Refusal != gitlet.LifecycleNotPermitted || !strings.Contains(got.Detail, "code.remote_credentials") {
			t.Errorf("%s: %+v, want not_permitted naming code.remote_credentials", what, got)
		}
		if strings.Contains(got.Detail, "${") {
			t.Errorf("%s: the refusal %q quotes the credential", what, got.Detail)
		}
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused requests appended %d records, want none", end-before)
	}

	// THE TABLE IS THE EPOCH'S, read at the request: declared for the host,
	// the same import lands.
	n.scratch.RemoteCredentials = config.CodeRemoteCredentials{
		"FORGE_TOKEN":           {Hosts: []string{"forge.example.com"}},
		"AWS_SECRET_ACCESS_KEY": {Hosts: []string{"forge.example.com"}},
	}
	n.lands(cases["an import naming an undeclared variable"])
	// A MIRROR REMOVED sends nothing, whatever the table says.
	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: mirrorSource}))
	n.scratch.RemoteCredentials = nil
	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: &gitlet.MirrorSource{}}))
}

// A MIRROR AND AN IMPORT ARE A PERSON'S (U49 a): a seat — its unit's lead at
// full_access, which may make, change and delete the project — asking to create
// one with a pull mirror, a push mirror or an import, or to set or remove
// either mirror of one, is refused not_permitted naming the rule, and nothing
// is appended; a person asking the same is answered applied, and the rows say
// it. The seat's other settings change still lands, so the refusal is the
// mirror's and not the lead's grant.
//
// Mutation: drop the mirrorsAsked arm from lifecycleRefusal — the lead's
// creates and changes land, red.
func TestAnAgentCannotSetAMirror(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	lead := agent("cto")

	withFrom := lcCreate(lead, "lead-from", unitOwned("eng"))
	withFrom.Create.MirrorFrom = mirrorSource
	withTo := lcCreate(lead, "lead-to", unitOwned("eng"))
	withTo.Create.MirrorTo = mirrorTarget
	imports := lcCreate(lead, "lead-import", unitOwned("eng"))
	imports.Import = importSource

	id := n.made(lcPerson, "shared", unitOwned("eng"))
	before := n.end()
	for _, req := range []gitlet.LifecycleRequest{
		withFrom, withTo, imports,
		lcSettings(lead, id, gitlet.RepoChange{MirrorFrom: mirrorSource}),
		lcSettings(lead, id, gitlet.RepoChange{MirrorTo: mirrorTarget}),
	} {
		got := n.life(req)
		if got.Refusal != gitlet.LifecycleNotPermitted ||
			!strings.Contains(got.Detail, "only a person sets a repository's mirror or import") {
			t.Errorf("the lead's %s of %s: %+v, want not_permitted naming who sets a mirror", req.Kind,
				req.Name+req.RepoID, got)
		}
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused requests appended %d records, want none", end-before)
	}
	n.lands(lcSettings(lead, id, gitlet.RepoChange{Description: new("the lead still maintains it")}))

	// A PERSON SETS EACH, and a seat may not REMOVE what a person set.
	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: mirrorSource, MirrorTo: mirrorTarget}))
	if row := n.resolved("shared"); !row.Mirrored || row.Status != gitlet.RepoStatusImporting {
		t.Fatalf("shared after a person's mirror_from is %s mirrored=%v, want a pull mirror importing from its "+
			"source", row.Status, row.Mirrored)
	}
	for _, change := range []gitlet.RepoChange{
		{MirrorFrom: &gitlet.MirrorSource{}}, {MirrorTo: &gitlet.MirrorTarget{}},
	} {
		if got := n.life(lcSettings(lead, id, change)); got.Refusal != gitlet.LifecycleNotPermitted {
			t.Errorf("the lead removing a mirror: %+v, want not_permitted", got)
		}
	}
	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: &gitlet.MirrorSource{}}))
	if row := n.resolved("shared"); row.Mirrored {
		t.Fatal("shared is still a pull mirror after a person removed its mirror_from")
	}

	for _, tc := range []struct {
		name   string
		req    gitlet.LifecycleRequest
		status gitlet.RepoStatus
	}{
		{"person-from", withCreate(lcCreate(lcPerson, "person-from", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) { r.Create.MirrorFrom = mirrorSource }), gitlet.RepoStatusImporting},
		{"person-to", withCreate(lcCreate(lcPerson, "person-to", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) { r.Create.MirrorTo = mirrorTarget }), gitlet.RepoStatusActive},
		{"person-import", withCreate(lcCreate(lcPerson, "person-import", unitOwned("eng")),
			func(r *gitlet.LifecycleRequest) { r.Import = importSource }), gitlet.RepoStatusImporting},
	} {
		n.lands(tc.req)
		if got := n.resolved(tc.name).Status; got != tc.status {
			t.Errorf("a person's create of %s made it %s, want %s", tc.name, got, tc.status)
		}
	}
}

// withCreate is req as edit leaves it.
func withCreate(req gitlet.LifecycleRequest, edit func(*gitlet.LifecycleRequest)) gitlet.LifecycleRequest {
	edit(&req)
	return req
}

// A CREDENTIAL IS A REFERENCE, NEVER A VALUE: every credential a mirror or an
// import names is one whole ${VAR} — a token written in its place, or a
// reference with anything around it, is refused as invalid wherever it is
// asked (a person's create, a person's settings change, a declaration) and
// the refusal never quotes it; and what a record carries, and its row keeps,
// is the reference as it was written, never what it resolves to — this domain
// resolves nothing.
//
// Mutation: accept any non-empty credential in remoteInvalid — every literal
// below is recorded, red.
func TestACredentialIsAReferenceNeverAValue(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "web", unitOwned("eng"))
	const token = "ghp_0123456789abcdefghijABCDEFGHIJ"
	for _, credential := range []string{token, "${FORGE_TOKEN}x", "Bearer ${FORGE_TOKEN}", "$FORGE_TOKEN",
		"${FORGE_TOKEN}${OTHER}", "${}"} {
		for what, req := range map[string]gitlet.LifecycleRequest{
			"a pull mirror": lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: &gitlet.MirrorSource{
				URL: mirrorSource.URL, Credential: credential}}),
			"a push mirror": lcSettings(lcPerson, id, gitlet.RepoChange{MirrorTo: &gitlet.MirrorTarget{
				URL: mirrorTarget.URL, Credential: credential, As: "founder"}}),
			"an import": withCreate(lcCreate(lcPerson, "imported", unitOwned("eng")),
				func(r *gitlet.LifecycleRequest) {
					r.Import = &gitlet.ImportSource{URL: importSource.URL, Credential: credential}
				}),
		} {
			_, err := n.store.Lifecycle(t.Context(), req)
			if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "credential") {
				t.Errorf("%s with credential %q: %v, want ErrInvalid naming the credential", what, credential, err)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Errorf("%s: the refusal %q quotes the token", what, err)
			}
		}
		declared := suiteSettings()
		declared.MirrorFrom = &gitlet.MirrorSource{URL: mirrorSource.URL, Credential: credential}
		if err := (gitlet.Declared{Name: "declared", Settings: declared}).Validate(); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("a declaration's pull mirror with credential %q: %v, want ErrInvalid", credential, err)
		}
	}

	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{MirrorFrom: mirrorSource, MirrorTo: mirrorTarget}))
	var from, to string
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT mirror_from, mirror_to FROM code_repos WHERE id = ?`,
			id).Scan(&from, &to)
	}); err != nil {
		t.Fatalf("read web's mirrors: %v", err)
	}
	for column, got := range map[string]string{"mirror_from": from, "mirror_to": to} {
		if !strings.Contains(got, `"credential":"${`) {
			t.Errorf("%s holds %s, want the credential as its ${VAR} reference", column, got)
		}
	}
}

// AN IMPORTING REPOSITORY IS NOT EXPUNGED: what of an import's packs no ref
// reaches yet is what a later chunk's refs will, so a person's purge of a
// repository being imported is refused repo_importing, its sentence naming
// the import, and nothing is appended; once the repository is active the same
// purge is recorded.
//
// Mutation: drop the importing arm from decideExpunge — the purge is
// recorded, red.
func TestAnImportingRepositoryIsNotExpunged(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), gitlet.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || got.Status != gitlet.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", got, err)
	}
	n.drain()
	repo := got.RepoID
	c, _ := committerOn(t, n, n.pub)
	before := n.end()
	request := purgeOf(repo)
	res, err := c.ProposeExpunge(t.Context(), gitlet.ExpungeProposal{Request: &request})
	if err != nil {
		t.Fatalf("ProposeExpunge: %v", err)
	}
	if res.Refusal != gitlet.RefusalRepoImporting || res.Detail != gitlet.ImportingDetail("imported") ||
		res.Outcome != "" {
		t.Fatalf("a purge of a repository being imported answered %+v, want refused repo_importing, unrecorded",
			res)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused purge appended %d records", end-before)
	}

	// A PUSH IS REFUSED THE SAME WAY, recorded per command.
	pushed, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/main", gitlet.ZeroOID, oidOf("A"))))
	if err != nil || pushed.Answer.Refused != gitlet.RefusalRepoImporting || len(pushed.Answer.Commands) != 1 ||
		pushed.Answer.Commands[0].Reason != gitlet.RefusalRepoImporting {
		t.Fatalf("a push to a repository being imported answered %+v, %v; want repo_importing", pushed, err)
	}

	n.exec(`UPDATE code_repos SET status = 'active', imported_at = 1 WHERE id = ?`, repo)
	request = purgeOf(repo)
	res, err = c.ProposeExpunge(t.Context(), gitlet.ExpungeProposal{Request: &request})
	requireExpunged(t, res, err)
}

// A PULL MIRROR TAKES NO CHANGE OF ITS OWN: it moves only by its syncs, so a
// change opened on one, and every write of a live one that could lead to a
// merge — an edit here — is refused repo_mirrored in the domain's one sentence
// for a pull mirror, and nothing is appended; a CLOSE still lands, moving no
// ref, so a repository that became a mirror with changes live sheds them.
//
// Mutation: drop the mirror arm from decideChange — the open is recorded,
// red; refuse the close too — the live change can never be shed, red.
func TestAPullMirrorTakesNoChange(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	live, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, live, err)

	s := suiteSettings()
	s.Owner, s.MirrorFrom = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}, mirrorSource
	n.ensure("web", s, t2)
	endImport(t, n, c, repo, 0)
	want := gitlet.MirroredDetail("web", gitlet.ManagedConfig)
	for _, p := range []gitlet.ChangeProposal{
		opening(repo, releaseRef, mainRef, ada),
		retitled(repo, live.ChangeID, ada, live.Version, "mirrored"),
	} {
		before := n.end()
		res, err := c.ProposeChange(ctx, p)
		requireRefused(t, n, before, res, err, gitlet.ChangeRefusalRepoMirrored, want)
	}
	closed, err := c.ProposeChange(ctx, closing(repo, live.ChangeID, ada, live.Version))
	if err != nil || closed.Outcome != statelog.OutcomeApplied || closed.Refusal != "" {
		t.Fatalf("a close of a pull mirror's live change answered %+v, %v; want it applied", closed, err)
	}
}

// A REPOSITORY BEING IMPORTED STAYS IMPORTING, AND A PULL MIRROR STAYS ONE ITS
// SOURCE CAN MOVE: whatever a person asks of either, nothing leaves an import
// half done for good, or a mirror no sync reaches.
//
//   - An archive of a repository being imported is refused not_permitted,
//     naming the import, and nothing is appended — the status that says it is
//     importing is the one an archive would take.
//   - A settings change of one lands and leaves it importing, and so does a
//     delete followed by a restore: the import resumes from what of it landed.
//   - Once its import is done, an archive of a pull mirror is refused,
//     naming the mirror — an archived repository takes no sync.
//   - A push mirror's seat reads what it publishes, judged on a person's
//     create and on a settings change that moves the judgement: a seat outside
//     the owning unit of a `visibility: unit` repository is refused
//     not_permitted naming mirror_to.as.
//
// Mutation: drop either arm of archiveRefusal, red at its archive; settle a
// settings change's status, or a restore's, from the settings alone, red at
// the importing row; drop either MirrorSeatRefusal call, red at its request.
func TestAnImportAndAPullMirrorKeepWhatTheyFollow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()

	imported := n.lands(withCreate(lcCreate(lcPerson, "imported", unitOwned("eng")),
		func(r *gitlet.LifecycleRequest) { r.Import = importSource })).Repo.ID
	before := n.end()
	if got := n.life(lcOn(gitlet.LifecycleArchive, lcPerson, imported)); got.Refusal != gitlet.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "being imported") {
		t.Errorf("a person's archive of a repository being imported: %+v, want not_permitted naming the import",
			got)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused archive appended %d records, want none", end-before)
	}
	n.lands(lcSettings(lcPerson, imported, gitlet.RepoChange{Description: new("filled from the forge")}))
	if got := n.resolved("imported"); got.Status != gitlet.RepoStatusImporting || got.Description == "" {
		t.Fatalf("imported after a settings change is %s %q, want importing with its description", got.Status,
			got.Description)
	}
	n.lands(lcOn(gitlet.LifecycleDelete, lcPerson, imported))
	n.lands(lcOn(gitlet.LifecycleRestore, lcPerson, imported))
	if got := n.resolved("imported").Status; got != gitlet.RepoStatusImporting {
		t.Fatalf("imported restored before its import was done is %s, want importing", got)
	}

	mirror := n.lands(withCreate(lcCreate(lcPerson, "mirror", unitOwned("eng")),
		func(r *gitlet.LifecycleRequest) { r.Create.MirrorFrom = mirrorSource })).Repo.ID
	n.exec(`UPDATE code_repos SET status = 'active', imported_at = 1 WHERE id = ?`, mirror)
	before = n.end()
	if got := n.life(lcOn(gitlet.LifecycleArchive, lcPerson, mirror)); got.Refusal != gitlet.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "pull mirror") {
		t.Errorf("a person's archive of a pull mirror: %+v, want not_permitted naming the mirror", got)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused archive appended %d records, want none", end-before)
	}

	// dev sits in Infra, under Engineering: it reads a repository Sales owns
	// only while the company sees it.
	asDev := &gitlet.MirrorTarget{URL: mirrorTarget.URL, Credential: mirrorTarget.Credential, As: "dev"}
	unseen := withCreate(lcCreate(lcPerson, "pricing", unitOwned("sales")), func(r *gitlet.LifecycleRequest) {
		r.Create.Visibility, r.Create.MirrorTo = gitlet.VisibilityUnit, asDev
	})
	if got := n.life(unseen); got.Refusal != gitlet.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "mirror_to.as: seat dev does not read pricing") {
		t.Errorf("a person's create pushing as a seat that does not read it: %+v, want not_permitted naming "+
			"mirror_to.as", got)
	}
	pricing := n.lands(withCreate(lcCreate(lcPerson, "pricing", unitOwned("sales")),
		func(r *gitlet.LifecycleRequest) { r.Create.MirrorTo = asDev })).Repo.ID
	unit := gitlet.VisibilityUnit
	if got := n.life(lcSettings(lcPerson, pricing, gitlet.RepoChange{Visibility: &unit})); got.Refusal !=
		gitlet.LifecycleNotPermitted || !strings.Contains(got.Detail, "mirror_to.as: seat dev does not read") {
		t.Errorf("a person narrowing pricing past its push mirror's seat: %+v, want not_permitted naming "+
			"mirror_to.as", got)
	}
}

// A PULL MIRROR IS IMPORTED FIRST, WHENEVER IT BECOMES ONE (M6-D16): an
// ACTIVE repository a settings change makes a pull mirror — a declaration's,
// or a person's through a tool — is importing from the mirror's own source,
// its import begun afresh (no chunk landed, no end, no error), so the
// source's history arrives in an import's resumable chunks rather than in one
// sync; and so is a pull mirror moved to ANOTHER source, its import counted
// from the first chunk again, which the next chunk 1 proves by landing. A
// credential changed alone is the same source, and the mirror stays active;
// a repository already being imported keeps the source its import fetches.
// Each status a settle or a tool answers is the one its apply wrote.
//
// Mutation: drop mirrorImportSQL's statement from applySettings — every
// repository stays active, red; start the import on any settings record that
// names a pull mirror (compare no URL) — the credential change imports, red;
// leave import_seq as the last import left it — the new import's chunk 1 is
// refused stale, red; answer the settle's status from the settings alone —
// Ensured says active, red.
func TestAPullMirrorIsImportedFirstWheneverItBecomesOne(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	ctx := t.Context()
	c, _ := committerOn(t, n, n.pub)
	main := "refs/heads/main"
	detail := func(name string) gitlet.RepoDetail {
		t.Helper()
		a, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepo, Repo: name})
		if err != nil || a.Repo == nil {
			t.Fatalf("read %s's detail: %+v, %v", name, a.Repo, err)
		}
		return *a.Repo
	}
	importing := func(name string, from *gitlet.MirrorSource) {
		t.Helper()
		d := detail(name)
		want := gitlet.ImportSource{URL: from.URL, Credential: from.Credential}
		if got := d.Remotes.Import; d.Status != gitlet.RepoStatusImporting || !d.Mirrored || got == nil ||
			got.Source != want || got.Chunks != 0 || !got.DoneAt.IsZero() || got.Error != "" {
			t.Fatalf("%s is %s (mirrored %v) importing %+v, want a pull mirror importing from %+v afresh: no "+
				"chunk, no end, no error", name, d.Status, d.Mirrored, got, want)
		}
	}
	active := func(name string) {
		t.Helper()
		if d := detail(name); d.Status != gitlet.RepoStatusActive || !d.Mirrored {
			t.Fatalf("%s is %s (mirrored %v), want an active pull mirror", name, d.Status, d.Mirrored)
		}
	}

	// A DECLARED REPOSITORY, active with main pushed, made a pull mirror; and
	// one being imported, made one at the same activation, which keeps the
	// source its import fetches.
	api := repoOn(t, n, "api")
	if _, err := n.store.EnsureRepo(ctx, n.activate(t1), gitlet.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource}); err != nil {
		t.Fatalf("ensure imported: %v", err)
	}
	n.drain()
	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	if got := n.ensure("api", s, t2); got.Status != gitlet.RepoStatusImporting {
		t.Fatalf("the settle making api a pull mirror answered %s, want importing", got.Status)
	}
	importing("api", mirrorSource)
	first := chunkOf(n, api, 1, 3, update(main, oidOf("A"), oidOf("B")))
	res, err := c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &first})
	requireSynced(t, "the source's first chunk", res, err)
	endImport(t, n, c, api, 1)
	active("api")
	n.ensure("imported", s, t2)
	if d := detail("imported"); d.Status != gitlet.RepoStatusImporting || !d.Mirrored || d.Remotes.Import == nil ||
		d.Remotes.Import.Source != *importSource {
		t.Fatalf("imported made a pull mirror while importing is %s (mirrored %v) importing %+v, want "+
			"importing from its own source %+v", d.Status, d.Mirrored, d.Remotes.Import, *importSource)
	}

	// THE SAME SOURCE, ITS CREDENTIAL CHANGED: no import.
	same := suiteSettings()
	same.MirrorFrom = &gitlet.MirrorSource{URL: mirrorSource.URL, Credential: "${OTHER_FORGE_TOKEN}"}
	if got := n.ensure("api", same, t3); got.Status != gitlet.RepoStatusActive {
		t.Fatalf("the settle of api's new credential answered %s, want active", got.Status)
	}
	active("api")

	// ANOTHER SOURCE: imported again, from its first chunk — which an active
	// pull mirror, importing nothing, refuses, and which the last import's
	// chunk count would have refused too.
	moved := &gitlet.MirrorSource{URL: "https://forge.example.com/acme/web-moved.git", Credential: "${FORGE_TOKEN}"}
	first = chunkOf(n, api, 1, 3, update(main, oidOf("B"), oidOf("C")))
	before := n.end()
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &first})
	requireStale(t, n, "a chunk of an active pull mirror", before, res, err, gitlet.RefusalStale)
	s.MirrorFrom = moved
	if got := n.ensure("api", s, harnessAt); got.Status != gitlet.RepoStatusImporting {
		t.Fatalf("the settle moving api to another source answered %s, want importing", got.Status)
	}
	importing("api", moved)
	first = chunkOf(n, api, 1, 3, update(main, oidOf("B"), oidOf("C")))
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &first})
	requireSynced(t, "the new source's first chunk", res, err)
	endImport(t, n, c, api, 1)
	active("api")

	// A PERSON'S, on a repository a tool made.
	web := n.made(lcPerson, "web", unitOwned("eng"))
	if got := n.lands(lcSettings(lcPerson, web, gitlet.RepoChange{MirrorFrom: mirrorSource})); got.Repo.Status !=
		gitlet.RepoStatusImporting {
		t.Fatalf("a person making web a pull mirror was answered %s, want importing", got.Repo.Status)
	}
	importing("web", mirrorSource)
}

// endImport lands the end of repo's import at its seq-th chunk through c, as
// its home does once the source's refs have landed: repo is ACTIVE after it.
func endImport(t *testing.T, n *node, c *gitlet.Committer, repo string, seq int64) {
	t.Helper()
	done := gitlet.ImportDone{RepoID: repo, Node: n.id, Seq: seq}
	res, err := c.ProposeImport(t.Context(), gitlet.ImportProposal{Done: &done})
	requireSynced(t, "the import's end", res, err)
}

// endImportShort lands the end of repo's import at its seq-th chunk through
// c with why it ended short: repo is ACTIVE after it and, being a pull
// mirror, never synced, which an import that landed whole would not leave it.
func endImportShort(t *testing.T, n *node, c *gitlet.Committer, repo string, seq int64, why string) {
	t.Helper()
	done := gitlet.ImportDone{RepoID: repo, Node: n.id, Seq: seq, Error: why}
	res, err := c.ProposeImport(t.Context(), gitlet.ImportProposal{Done: &done})
	requireSynced(t, "the import's end short", res, err)
}

// NOTHING MERGES INTO A PULL MIRROR OR A REPOSITORY BEING IMPORTED: a merge
// moves its target, and a pull mirror moves only by its syncs from its
// source, so a merge of a change that meets its gate is refused repo_mirrored,
// and so is each of a queue's steps toward one — an enqueue, a train's
// candidate, a landing of a train built before the repository became a mirror
// — in the domain's one sentence for a pull mirror, nothing appended; an EJECT
// repo_mirrored still lands, moving no ref, so a repository that became a
// mirror sheds its queue — and is refused stale, nothing appended, on one that
// follows no source; its queue work says which it is (QueueDue.Mirrored), what
// its home sheds the queue by. A repository being imported takes no write at
// all: the same merge and enqueue are refused repo_importing.
//
// Mutation: drop the mirror arm from decideMerge — the merge lands, red; drop
// the enqueue, the candidate or the landing from decideQueue's — that record
// lands or is refused for another reason, red; refuse every queue record on a
// mirror — the queue can never be shed, red; drop decideEject's guard on the
// reason — the eject of a repository that is no mirror lands, red; answer the
// queue work's Mirrored false, or true, red at its read; drop either
// importing arm, red at its write.
func TestNothingMergesIntoAPullMirror(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	ctx := t.Context()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)

	// web merges into main directly, its change meeting main's gate; api
	// merges through its queue, one change queued and another open.
	web, change := mergeRepo(t, n, c, "web", gitlet.DirectPersons, mergeGate())
	approved(t, n, w, web, change)
	api, queuedChange := queueRepo(t, n, c, "api")
	ready(t, n, w, api, queuedChange)
	enqueue := enqueueing(api, queuedChange, ada, oidOf("B"))
	entered, err := c.ProposeQueue(ctx, enqueue)
	requireQueued(t, entered, err, gitlet.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	train := training(api, oidOf("A"), entryID, oidOf("M"))
	built, err := c.ProposeQueue(ctx, train)
	requireRecorded(t, built, err)
	n.passed(api, oidOf("M"), mainRef, "unit")
	other, err := c.ProposeChange(ctx, opening(api, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	// AN ENTRY IS EJECTED repo_mirrored ONLY FROM A PULL MIRROR: api follows
	// no source yet, so the reason is stale.
	before := n.end()
	early, err := c.ProposeQueue(ctx, queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: api,
			Entries: []gitlet.Ejection{{EntryID: entryID, Reason: gitlet.EjectRepoMirrored}}}
	}))
	requireQueueRefused(t, n, before, early, err, gitlet.MergeRefusalStale, "api follows no source now")
	if due, err := n.reader.QueueDue(ctx, gitlet.QueueDueQuery{RepoID: api, Fresh: stale}); err != nil ||
		due.DueQueue == nil || due.DueQueue.Mirrored {
		t.Fatalf("api's queue work before its mirror_from is %+v, %v; want it read as no pull mirror's",
			due.DueQueue, err)
	}

	for _, name := range []string{"web", "api"} {
		s := suiteSettings()
		s.Owner, s.MirrorFrom = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}, mirrorSource
		if name == "web" {
			s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, mergeGate()
		} else {
			s.Protections[0].Gate = queueGate()
		}
		endImport(t, n, c, n.ensure(name, s, t2).RepoID, 0)
	}

	before = n.end()
	res, err := c.ProposeMerge(ctx, merging(web, change, ada, gitlet.MergeMethodSquash, oidOf("A"), oidOf("B"),
		oidOf("S")))
	requireMergeRefused(t, n, before, res, err, gitlet.MergeRefusalRepoMirrored,
		gitlet.MirroredDetail("web", gitlet.ManagedConfig))
	queued, err := c.ProposeQueue(ctx, enqueueing(api, other.ChangeID, ada, oidOf("R")))
	requireQueueRefused(t, n, before, queued, err, gitlet.MergeRefusalRepoMirrored,
		gitlet.MirroredDetail("api", gitlet.ManagedConfig))
	queued, err = c.ProposeQueue(ctx, training(api, oidOf("A"), entryID, oidOf("N")))
	requireQueueRefused(t, n, before, queued, err, gitlet.MergeRefusalRepoMirrored,
		gitlet.MirroredDetail("api", gitlet.ManagedConfig))
	queued, err = c.ProposeQueue(ctx, landingOf(api, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: entryID, ChangeID: queuedChange, Commit: oidOf("M")}))
	requireQueueRefused(t, n, before, queued, err, gitlet.MergeRefusalRepoMirrored,
		gitlet.MirroredDetail("api", gitlet.ManagedConfig))
	if main := n.target(api, mainRef); main != oidOf("A") {
		t.Fatalf("the pull mirror's main is at %q, want A: nothing but a sync moves it", main)
	}
	// ITS HOME READS IT A PULL MIRROR off its queue work, which is what has
	// it shed the queue rather than build a train.
	due, err := n.reader.QueueDue(ctx, gitlet.QueueDueQuery{RepoID: api, Fresh: stale})
	if err != nil || due.DueQueue == nil || !due.DueQueue.Mirrored || len(due.DueQueue.Entries) != 1 {
		t.Fatalf("api's queue work is %+v, %v; want its one live entry, read as a pull mirror's", due.DueQueue,
			err)
	}
	shed, err := c.ProposeQueue(ctx, queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: api,
			Entries: []gitlet.Ejection{{EntryID: entryID, Reason: gitlet.EjectRepoMirrored}}}
	}))
	requireRecorded(t, shed, err)

	n.exec(`UPDATE code_repos SET status = 'importing' WHERE id IN (?, ?)`, web, api)
	before = n.end()
	res, err = c.ProposeMerge(ctx, merging(web, change, ada, gitlet.MergeMethodSquash, oidOf("A"), oidOf("B"),
		oidOf("S")))
	requireMergeRefused(t, n, before, res, err, gitlet.MergeRefusalRepoImporting, gitlet.ImportingDetail("web"))
	queued, err = c.ProposeQueue(ctx, enqueueing(api, other.ChangeID, ada, oidOf("R")))
	requireQueueRefused(t, n, before, queued, err, gitlet.MergeRefusalRepoImporting, gitlet.ImportingDetail("api"))
}

// A PUSH MIRROR PUSHES AS A PERSON ONLY WHERE THEY USE GITLET (ADR-0110).
//
// A push mirror publishes only what the seat it pushes as reads, and a person
// reads only through Gitlet, so a person's create, and a person's settings
// change, that sets a push mirror pushing as the founder's seat is refused
// not_permitted naming the seat and the permission while the identity
// directory says the founder is not given Gitlet — nothing appended, the
// directory asked about that seat alone — and lands once they are. An owner
// change under a mirror already set asks the directory nothing: it moves no
// person's app, and the push asks again before every push.
//
// Mutation: hand MirrorSeatRefusal false whatever the directory says — the
// founder's mirror lands without Gitlet, red.
func TestAPushMirrorPushesAsAPersonOnlyWhereTheyUseGitlet(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	var others []iam.Grant
	for _, app := range iam.Apps {
		if app != iam.GrantGitlet {
			others = append(others, app)
		}
	}
	n.people.grants["founder"] = others
	id := n.made(lcPerson, "web", unitOwned("eng"))
	as := func() *gitlet.MirrorTarget {
		return &gitlet.MirrorTarget{URL: "https://forge.example.com/b/web.git", Credential: "${FOUNDER_FORGE_TOKEN}",
			As: "founder"}
	}
	create := withCreate(lcCreate(lcPerson, "mirrored", unitOwned("eng")),
		func(r *gitlet.LifecycleRequest) { r.Create.MirrorTo = as() })
	set := lcSettings(lcPerson, id, gitlet.RepoChange{MirrorTo: as()})
	before := n.end()
	for what, req := range map[string]gitlet.LifecycleRequest{"a create": create, "a settings change": set} {
		got := n.life(req)
		if got.Refusal != gitlet.LifecycleNotPermitted || !strings.Contains(got.Detail, "nobody holding founder") ||
			!strings.Contains(got.Detail, "Gitlet") {
			t.Errorf("%s pushing as the founder without Gitlet: %+v, want not_permitted naming them", what, got)
		}
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused requests appended %d records, want none", end-before)
	}
	if got := n.people.seats(); len(got) != 2 || got[0] != "founder" || got[1] != "founder" {
		t.Errorf("the directory was asked about %v, want the founder's seat once a request", got)
	}

	n.people.grants["founder"] = []iam.Grant{iam.GrantGitlet}
	n.lands(create)
	n.lands(set)
	asked := len(n.people.seats())
	plat := unitOwned("plat")
	n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{Owner: &plat}))
	if got := len(n.people.seats()); got != asked {
		t.Errorf("an owner change under a mirror already set asked the directory %d time(s), want none", got-asked)
	}
}
