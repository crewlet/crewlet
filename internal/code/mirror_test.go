package code_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S MIRRORS AND ITS IMPORT (mirror.go): who may set one, what a
// record carries of its credential, and what a repository that moves only by
// what the code host fetches — a pull mirror, an import — refuses.

// mirrorSource, mirrorTarget and importSource are a remote at another host as
// a person names one: its URL, and its credential as the ${VAR} that holds it.
var (
	mirrorSource = &code.MirrorSource{URL: "https://forge.example.com/acme/web.git", Credential: "${FORGE_TOKEN}"}
	mirrorTarget = &code.MirrorTarget{URL: "https://forge.example.com/acme/web-public.git",
		Credential: "${FOUNDER_FORGE_TOKEN}", As: "founder"}
	importSource = &code.ImportSource{URL: "https://forge.example.com/acme/api.git", Credential: "${FORGE_TOKEN}"}
)

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
	for _, req := range []code.LifecycleRequest{
		withFrom, withTo, imports,
		lcSettings(lead, id, code.RepoChange{MirrorFrom: mirrorSource}),
		lcSettings(lead, id, code.RepoChange{MirrorTo: mirrorTarget}),
	} {
		got := n.life(req)
		if got.Refusal != code.LifecycleNotPermitted ||
			!strings.Contains(got.Detail, "only a person sets a repository's mirror or import") {
			t.Errorf("the lead's %s of %s: %+v, want not_permitted naming who sets a mirror", req.Kind,
				req.Name+req.RepoID, got)
		}
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused requests appended %d records, want none", end-before)
	}
	n.lands(lcSettings(lead, id, code.RepoChange{Description: new("the lead still maintains it")}))

	// A PERSON SETS EACH, and a seat may not REMOVE what a person set.
	n.lands(lcSettings(lcPerson, id, code.RepoChange{MirrorFrom: mirrorSource, MirrorTo: mirrorTarget}))
	if row := n.resolved("shared"); !row.Mirrored || row.Status != code.RepoStatusActive {
		t.Fatalf("shared after a person's mirror_from is %s mirrored=%v, want an active pull mirror", row.Status,
			row.Mirrored)
	}
	for _, change := range []code.RepoChange{
		{MirrorFrom: &code.MirrorSource{}}, {MirrorTo: &code.MirrorTarget{}},
	} {
		if got := n.life(lcSettings(lead, id, change)); got.Refusal != code.LifecycleNotPermitted {
			t.Errorf("the lead removing a mirror: %+v, want not_permitted", got)
		}
	}
	n.lands(lcSettings(lcPerson, id, code.RepoChange{MirrorFrom: &code.MirrorSource{}}))
	if row := n.resolved("shared"); row.Mirrored {
		t.Fatal("shared is still a pull mirror after a person removed its mirror_from")
	}

	for _, tc := range []struct {
		name   string
		req    code.LifecycleRequest
		status code.RepoStatus
	}{
		{"person-from", withCreate(lcCreate(lcPerson, "person-from", unitOwned("eng")),
			func(r *code.LifecycleRequest) { r.Create.MirrorFrom = mirrorSource }), code.RepoStatusImporting},
		{"person-to", withCreate(lcCreate(lcPerson, "person-to", unitOwned("eng")),
			func(r *code.LifecycleRequest) { r.Create.MirrorTo = mirrorTarget }), code.RepoStatusActive},
		{"person-import", withCreate(lcCreate(lcPerson, "person-import", unitOwned("eng")),
			func(r *code.LifecycleRequest) { r.Import = importSource }), code.RepoStatusImporting},
	} {
		n.lands(tc.req)
		if got := n.resolved(tc.name).Status; got != tc.status {
			t.Errorf("a person's create of %s made it %s, want %s", tc.name, got, tc.status)
		}
	}
}

// withCreate is req as edit leaves it.
func withCreate(req code.LifecycleRequest, edit func(*code.LifecycleRequest)) code.LifecycleRequest {
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
		for what, req := range map[string]code.LifecycleRequest{
			"a pull mirror": lcSettings(lcPerson, id, code.RepoChange{MirrorFrom: &code.MirrorSource{
				URL: mirrorSource.URL, Credential: credential}}),
			"a push mirror": lcSettings(lcPerson, id, code.RepoChange{MirrorTo: &code.MirrorTarget{
				URL: mirrorTarget.URL, Credential: credential, As: "founder"}}),
			"an import": withCreate(lcCreate(lcPerson, "imported", unitOwned("eng")),
				func(r *code.LifecycleRequest) {
					r.Import = &code.ImportSource{URL: importSource.URL, Credential: credential}
				}),
		} {
			_, err := n.store.Lifecycle(t.Context(), req)
			if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "credential") {
				t.Errorf("%s with credential %q: %v, want ErrInvalid naming the credential", what, credential, err)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Errorf("%s: the refusal %q quotes the token", what, err)
			}
		}
		declared := suiteSettings()
		declared.MirrorFrom = &code.MirrorSource{URL: mirrorSource.URL, Credential: credential}
		if err := (code.Declared{Name: "declared", Settings: declared}).Validate(); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("a declaration's pull mirror with credential %q: %v, want ErrInvalid", credential, err)
		}
	}

	n.lands(lcSettings(lcPerson, id, code.RepoChange{MirrorFrom: mirrorSource, MirrorTo: mirrorTarget}))
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
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), code.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || got.Status != code.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", got, err)
	}
	n.drain()
	repo := got.RepoID
	c, _ := committerOn(t, n, n.pub)
	before := n.end()
	request := purgeOf(repo)
	res, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Request: &request})
	if err != nil {
		t.Fatalf("ProposeExpunge: %v", err)
	}
	if res.Refusal != code.RefusalRepoImporting || res.Detail != code.ImportingDetail("imported") ||
		res.Outcome != "" {
		t.Fatalf("a purge of a repository being imported answered %+v, want refused repo_importing, unrecorded",
			res)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused purge appended %d records", end-before)
	}

	// A PUSH IS REFUSED THE SAME WAY, recorded per command.
	pushed, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/main", code.ZeroOID, oidOf("A"))))
	if err != nil || pushed.Answer.Refused != code.RefusalRepoImporting || len(pushed.Answer.Commands) != 1 ||
		pushed.Answer.Commands[0].Reason != code.RefusalRepoImporting {
		t.Fatalf("a push to a repository being imported answered %+v, %v; want repo_importing", pushed, err)
	}

	n.exec(`UPDATE code_repos SET status = 'active', imported_at = 1 WHERE id = ?`, repo)
	request = purgeOf(repo)
	res, err = c.ProposeExpunge(t.Context(), code.ExpungeProposal{Request: &request})
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
	s.Owner, s.MirrorFrom = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}, mirrorSource
	n.ensure("web", s, t2)
	want := code.MirroredDetail("web", code.ManagedConfig)
	for _, p := range []code.ChangeProposal{
		opening(repo, releaseRef, mainRef, ada),
		retitled(repo, live.ChangeID, ada, live.Version, "mirrored"),
	} {
		before := n.end()
		res, err := c.ProposeChange(ctx, p)
		requireRefused(t, n, before, res, err, code.ChangeRefusalRepoMirrored, want)
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
		func(r *code.LifecycleRequest) { r.Import = importSource })).Repo.ID
	before := n.end()
	if got := n.life(lcOn(code.LifecycleArchive, lcPerson, imported)); got.Refusal != code.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "being imported") {
		t.Errorf("a person's archive of a repository being imported: %+v, want not_permitted naming the import",
			got)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused archive appended %d records, want none", end-before)
	}
	n.lands(lcSettings(lcPerson, imported, code.RepoChange{Description: new("filled from the forge")}))
	if got := n.resolved("imported"); got.Status != code.RepoStatusImporting || got.Description == "" {
		t.Fatalf("imported after a settings change is %s %q, want importing with its description", got.Status,
			got.Description)
	}
	n.lands(lcOn(code.LifecycleDelete, lcPerson, imported))
	n.lands(lcOn(code.LifecycleRestore, lcPerson, imported))
	if got := n.resolved("imported").Status; got != code.RepoStatusImporting {
		t.Fatalf("imported restored before its import was done is %s, want importing", got)
	}

	mirror := n.lands(withCreate(lcCreate(lcPerson, "mirror", unitOwned("eng")),
		func(r *code.LifecycleRequest) { r.Create.MirrorFrom = mirrorSource })).Repo.ID
	n.exec(`UPDATE code_repos SET status = 'active', imported_at = 1 WHERE id = ?`, mirror)
	before = n.end()
	if got := n.life(lcOn(code.LifecycleArchive, lcPerson, mirror)); got.Refusal != code.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "pull mirror") {
		t.Errorf("a person's archive of a pull mirror: %+v, want not_permitted naming the mirror", got)
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused archive appended %d records, want none", end-before)
	}

	// dev sits in Infra, under Engineering: it reads a repository Sales owns
	// only while the company sees it.
	asDev := &code.MirrorTarget{URL: mirrorTarget.URL, Credential: mirrorTarget.Credential, As: "dev"}
	unseen := withCreate(lcCreate(lcPerson, "pricing", unitOwned("sales")), func(r *code.LifecycleRequest) {
		r.Create.Visibility, r.Create.MirrorTo = code.VisibilityUnit, asDev
	})
	if got := n.life(unseen); got.Refusal != code.LifecycleNotPermitted ||
		!strings.Contains(got.Detail, "mirror_to.as: seat dev does not read pricing") {
		t.Errorf("a person's create pushing as a seat that does not read it: %+v, want not_permitted naming "+
			"mirror_to.as", got)
	}
	pricing := n.lands(withCreate(lcCreate(lcPerson, "pricing", unitOwned("sales")),
		func(r *code.LifecycleRequest) { r.Create.MirrorTo = asDev })).Repo.ID
	unit := code.VisibilityUnit
	if got := n.life(lcSettings(lcPerson, pricing, code.RepoChange{Visibility: &unit})); got.Refusal !=
		code.LifecycleNotPermitted || !strings.Contains(got.Detail, "mirror_to.as: seat dev does not read") {
		t.Errorf("a person narrowing pricing past its push mirror's seat: %+v, want not_permitted naming "+
			"mirror_to.as", got)
	}
}

// NOTHING MERGES INTO A PULL MIRROR OR A REPOSITORY BEING IMPORTED: a merge
// moves its target, and a pull mirror moves only by its syncs from its
// source, so a merge of a change that meets its gate is refused repo_mirrored,
// and so is each of a queue's steps toward one — an enqueue, a train's
// candidate, a landing of a train built before the repository became a mirror
// — in the domain's one sentence for a pull mirror, nothing appended; an EJECT
// still lands, moving no ref, so a repository that became a mirror sheds its
// queue. A repository being imported takes no write at all: the same merge and
// enqueue are refused repo_importing.
//
// Mutation: drop the mirror arm from decideMerge — the merge lands, red; drop
// the enqueue, the candidate or the landing from decideQueue's — that record
// lands or is refused for another reason, red; refuse every queue record on a
// mirror — the queue can never be shed, red; drop either importing arm, red at
// its write.
func TestNothingMergesIntoAPullMirror(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	ctx := t.Context()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)

	// web merges into main directly, its change meeting main's gate; api
	// merges through its queue, one change queued and another open.
	web, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	approved(t, n, w, web, change)
	api, queuedChange := queueRepo(t, n, c, "api")
	ready(t, n, w, api, queuedChange)
	enqueue := enqueueing(api, queuedChange, ada, oidOf("B"))
	entered, err := c.ProposeQueue(ctx, enqueue)
	requireQueued(t, entered, err, code.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	train := training(api, oidOf("A"), entryID, oidOf("M"))
	built, err := c.ProposeQueue(ctx, train)
	requireRecorded(t, built, err)
	n.passed(api, oidOf("M"), mainRef, "unit")
	other, err := c.ProposeChange(ctx, opening(api, releaseRef, mainRef, dev))
	requireChanged(t, other, err)

	for _, name := range []string{"web", "api"} {
		s := suiteSettings()
		s.Owner, s.MirrorFrom = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}, mirrorSource
		if name == "web" {
			s.Protections[0].DirectPush, s.Protections[0].Gate = code.DirectPersons, mergeGate()
		} else {
			s.Protections[0].Gate = queueGate()
		}
		n.ensure(name, s, t2)
	}

	before := n.end()
	res, err := c.ProposeMerge(ctx, merging(web, change, ada, code.MergeMethodSquash, oidOf("A"), oidOf("B"),
		oidOf("S")))
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalRepoMirrored,
		code.MirroredDetail("web", code.ManagedConfig))
	queued, err := c.ProposeQueue(ctx, enqueueing(api, other.ChangeID, ada, oidOf("R")))
	requireQueueRefused(t, n, before, queued, err, code.MergeRefusalRepoMirrored,
		code.MirroredDetail("api", code.ManagedConfig))
	queued, err = c.ProposeQueue(ctx, training(api, oidOf("A"), entryID, oidOf("N")))
	requireQueueRefused(t, n, before, queued, err, code.MergeRefusalRepoMirrored,
		code.MirroredDetail("api", code.ManagedConfig))
	queued, err = c.ProposeQueue(ctx, landingOf(api, code.LandingPreLand, train.Candidate.Train, oidOf("A"),
		code.LandEntry{EntryID: entryID, ChangeID: queuedChange, Commit: oidOf("M")}))
	requireQueueRefused(t, n, before, queued, err, code.MergeRefusalRepoMirrored,
		code.MirroredDetail("api", code.ManagedConfig))
	if main := n.target(api, mainRef); main != oidOf("A") {
		t.Fatalf("the pull mirror's main is at %q, want A: nothing but a sync moves it", main)
	}
	shed, err := c.ProposeQueue(ctx, queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: api, Entries: []code.Ejection{{EntryID: entryID, Reason: code.EjectConflict}}}
	}))
	requireRecorded(t, shed, err)

	n.exec(`UPDATE code_repos SET status = 'importing' WHERE id IN (?, ?)`, web, api)
	before = n.end()
	res, err = c.ProposeMerge(ctx, merging(web, change, ada, code.MergeMethodSquash, oidOf("A"), oidOf("B"),
		oidOf("S")))
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalRepoImporting, code.ImportingDetail("web"))
	queued, err = c.ProposeQueue(ctx, enqueueing(api, other.ChangeID, ada, oidOf("R")))
	requireQueueRefused(t, n, before, queued, err, code.MergeRefusalRepoImporting, code.ImportingDetail("api"))
}
