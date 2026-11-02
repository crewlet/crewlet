package notify_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

func TestAnExternalIDResolvesToItsSeat(t *testing.T) {
	r := registry(t)
	if err := r.Register("mattermost", "lead-bot", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}

	p, ok := r.ByExternalID("mattermost", "lead-bot")
	if !ok || p.Handle != "engineering-lead" {
		t.Fatalf("resolved %+v, want the lead", p)
	}
	if got := r.ExternalID("mattermost", "engineering-lead"); got != "lead-bot" {
		t.Fatalf("outbound id is %q, want lead-bot", got)
	}
	if !r.Knows("mattermost", "lead-bot") {
		t.Fatal("Knows missed a registered id")
	}
	// A miss is ordinary: most senders on a shared channel are strangers.
	if _, ok := r.ByExternalID("mattermost", "some-outsider"); ok {
		t.Fatal("an unregistered id resolved to somebody")
	}
	if r.Knows("mattermost", "some-outsider") {
		t.Fatal("Knows claimed an outsider")
	}
}

// AN ENGINE SURFACE'S ID IS THE SEAT'S HANDLE. A namespace the registry is
// built with as one whose ids are handles answers every question about an id
// the way the seat index answers the handle: a person's comment on the
// engine's own tracker comes from the seat they write as, wherever a reader
// asks who sent it — and nothing can be registered there to answer first.
func TestAHandleNamespaceAnswersByTheHandleItsIDIs(t *testing.T) {
	o := company()
	o.Normalize()
	r := notify.NewRegistry(o, nil, "work")

	p, ok := r.ByExternalID("work", "dana-founder")
	if !ok || p.Handle != "dana-founder" || !p.Human {
		t.Fatalf("a person's handle on a handle namespace resolved to %+v, %v, "+
			"want the person's seat — read as an outside party, a turn they "+
			"woke acts for nobody", p, ok)
	}
	if !r.Knows("work", "engineering-lead") {
		t.Error("Knows missed a seat's handle on a handle namespace")
	}
	if got := r.ExternalID("work", "backend-engineer"); got != "backend-engineer" {
		t.Errorf("a seat's id on a handle namespace is %q, want its handle", got)
	}

	// A HANDLE THE ORG DOES NOT HOLD is nobody, as it is by handle.
	if _, ok := r.ByExternalID("work", "left-the-company"); ok {
		t.Error("a handle no seat holds resolved to somebody")
	}
	if r.Knows("work", "left-the-company") || r.ExternalID("work", "left-the-company") != "" {
		t.Error("a handle no seat holds is known on a handle namespace")
	}

	// ONLY THE NAMESPACES DECLARED: a vendor's id that happens to spell a
	// handle is not that seat, and a registry declaring none answers none.
	if _, ok := r.ByExternalID("slack", "dana-founder"); ok {
		t.Error("an undeclared namespace answered an id by the handle it spells")
	}
	if _, ok := registry(t).ByExternalID("work", "dana-founder"); ok {
		t.Error("a registry declaring no handle namespace answered one")
	}

	// AND NOTHING IS REGISTERED THERE, which would be a second name for a
	// seat answering before the handle it contradicts.
	if err := r.Register("work", "someone-else", "engineering-lead"); err == nil {
		t.Fatal("an id was registered on a handle namespace")
	}
	if p, _ := r.ByExternalID("work", "someone-else"); p.Handle != "" {
		t.Errorf("a refused registration resolves to %q", p.Handle)
	}
}

// The bot companion namespace: an inbound payload names an agent poster by
// bot id, while a human typing a mention uses the member id. Both must
// resolve, or a fellow agent's message gets annotated as a stranger while a
// human's identical message is annotated as a colleague.
func TestABotIdentityResolvesThroughTheCompanionNamespace(t *testing.T) {
	r := registry(t)
	if err := r.Register(notify.BotNamespace("mattermost"), "uid-99", "backend-engineer"); err != nil {
		t.Fatalf("register: %v", err)
	}

	p, ok := r.ByExternalID("mattermost", "uid-99")
	if !ok || p.Handle != "backend-engineer" {
		t.Fatalf("a bot id did not resolve through the transport: %+v, %v", p, ok)
	}
	if !r.Knows("mattermost", "uid-99") {
		t.Fatal("Knows missed a bot id")
	}
}

// A seat's external id is not fixed for the life of a process: a transport
// overwrites its configured guess with the name the server reports. The OLD
// id must stop resolving, or the stale alias outlives the seat's own
// decommission and swallows a later seat provisioned under the freed name.
func TestARenameWithdrawsTheIDTheSeatHeldBefore(t *testing.T) {
	r := registry(t)
	if err := r.Register("mattermost", "guessed-name", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register("mattermost", "server-name", "engineering-lead"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	if _, ok := r.ByExternalID("mattermost", "guessed-name"); ok {
		t.Fatal("the old id still resolves to the seat")
	}
	if r.Knows("mattermost", "guessed-name") {
		t.Fatal("Knows still claims the withdrawn id")
	}
	p, ok := r.ByExternalID("mattermost", "server-name")
	if !ok || p.Handle != "engineering-lead" {
		t.Fatalf("the new id resolved to %+v", p)
	}
	if got := r.ExternalID("mattermost", "engineering-lead"); got != "server-name" {
		t.Fatalf("the outbound id is %q, want server-name", got)
	}

	// AND THE FREED NAME IS GENUINELY FREE — the case the whole rule
	// exists for. A later seat provisioned under it must win it.
	if err := r.Register("mattermost", "guessed-name", "backend-engineer"); err != nil {
		t.Fatalf("the freed name was not available: %v", err)
	}
	p, _ = r.ByExternalID("mattermost", "guessed-name")
	if p.Handle != "backend-engineer" {
		t.Fatalf("the freed name resolves to %q", p.Handle)
	}
}

// Re-registering the same pair is idempotent — what makes a reconcile cheap
// to re-run — while a CROSS-SEAT claim is refused rather than silently
// stealing the other seat's mail.
func TestAnIDHeldByAnotherSeatIsRefused(t *testing.T) {
	r := registry(t)
	if err := r.Register("slack", "U777", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register("slack", "U777", "engineering-lead"); err != nil {
		t.Fatalf("re-registering the same pair was refused: %v", err)
	}

	err := r.Register("slack", "U777", "backend-engineer")
	if err == nil {
		t.Fatal("a second seat took an id already held")
	}
	for _, want := range []string{"U777", "engineering-lead", "backend-engineer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not name %q: %v", want, err)
		}
	}
	p, _ := r.ByExternalID("slack", "U777")
	if p.Handle != "engineering-lead" {
		t.Fatalf("the refused claim landed anyway: %q", p.Handle)
	}
}

// A mapping onto a handle no seat holds is INERT — resolution ends at a
// handle lookup that misses — so it must be refused loudly rather than
// accepted and silently unresolvable ever after.
func TestRegisteringAnUnknownSeatIsRefused(t *testing.T) {
	r := registry(t)

	err := r.Register("slack", "U123", "nobody-here")
	if err == nil {
		t.Fatal("an id was registered onto a seat that does not exist")
	}
	if !strings.Contains(err.Error(), "nobody-here") || !strings.Contains(err.Error(), "nimbus") {
		t.Fatalf("the error names neither the seat nor the company: %v", err)
	}
	for _, bad := range []string{"Not A Handle", ""} {
		if err := r.Register("slack", "U123", bad); err == nil {
			t.Fatalf("the handle %q was accepted", bad)
		}
	}
	if err := r.Register("", "U123", "engineering-lead"); err == nil {
		t.Fatal("an empty namespace was accepted")
	}
	if err := r.Register("slack", "", "engineering-lead"); err == nil {
		t.Fatal("an empty external id was accepted")
	}
}

// Between reading an id and removing it, that id may legitimately have moved
// to another seat. Unregistering on behalf of the old owner must not strip
// the new one's live identity.
func TestUnregisterOnlyRemovesWhatTheCallerStillOwns(t *testing.T) {
	r := registry(t)
	if err := r.Register("slack", "U777", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}

	if r.Unregister("slack", "U777", "backend-engineer") {
		t.Fatal("a seat unregistered an id it does not hold")
	}
	p, _ := r.ByExternalID("slack", "U777")
	if p.Handle != "engineering-lead" {
		t.Fatalf("the mapping was stripped anyway: %q", p.Handle)
	}

	if !r.Unregister("slack", "U777", "engineering-lead") {
		t.Fatal("the holder could not unregister its own id")
	}
	if _, ok := r.ByExternalID("slack", "U777"); ok {
		t.Fatal("the id still resolves after being withdrawn")
	}
	if got := r.ExternalID("slack", "engineering-lead"); got != "" {
		t.Fatalf("the reverse entry survived: %q", got)
	}
	if r.Unregister("slack", "U777", "engineering-lead") {
		t.Fatal("unregistering twice reported a second removal")
	}
}

// Withdrawing the OLD id of a seat that has already been re-registered must
// leave its newer reverse entry alone — that is the seat's live identity.
func TestWithdrawingAStaleIDKeepsTheSeatsCurrentOne(t *testing.T) {
	r := registry(t)
	if err := r.Register("mattermost", "old", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Re-register through a different namespace so the forward entry for
	// "old" survives, then withdraw it by hand — the shape a transport's
	// own decommission produces.
	if err := r.Register(notify.BotNamespace("mattermost"), "uid-1", "engineering-lead"); err != nil {
		t.Fatalf("register bot: %v", err)
	}
	if err := r.Register("mattermost", "new", "engineering-lead"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r.Unregister("mattermost", "old", "engineering-lead")

	if got := r.ExternalID("mattermost", "engineering-lead"); got != "new" {
		t.Fatalf("the live outbound id is %q, want new", got)
	}
	if got := r.ExternalID(notify.BotNamespace("mattermost"), "engineering-lead"); got != "uid-1" {
		t.Fatalf("the bot id is %q, want uid-1", got)
	}
}

func TestNamespacesListsOnlyWhatHoldsAMapping(t *testing.T) {
	r := registry(t)
	if got := r.Namespaces(); len(got) != 0 {
		t.Fatalf("a fresh registry lists %v", got)
	}
	if err := r.Register("slack", "U1", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register("gitlab", "g1", "backend-engineer"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := r.Namespaces(); !slices.Equal(got, []string{"gitlab", "slack"}) {
		t.Fatalf("Namespaces = %v", got)
	}

	// A namespace emptied by a withdrawal drops out — it holds nothing.
	r.Unregister("slack", "U1", "engineering-lead")
	if got := r.Namespaces(); !slices.Equal(got, []string{"gitlab"}) {
		t.Fatalf("an emptied namespace is still listed: %v", got)
	}

	// Identities hands back a COPY: a caller iterating it must not be
	// racing a transport's registration.
	ids := r.Identities("gitlab")
	ids["injected"] = "engineering-lead"
	if r.Knows("gitlab", "injected") {
		t.Fatal("mutating the returned map reached the registry")
	}
}

func TestConcurrentRegistrationIsSafe(t *testing.T) {
	r := registry(t)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			// Each goroutine renames the same seat, so every write
			// takes the withdraw-the-previous path as well.
			_ = r.Register("mattermost", "name-"+string(rune('a'+i%26)), "engineering-lead")
			_, _ = r.ByExternalID("mattermost", "name-a")
			_ = r.ExternalID("mattermost", "engineering-lead")
			_ = r.Namespaces()
		})
	}
	wg.Wait()

	// Exactly one id may survive for the seat, and the two directions must
	// agree about which — a rename that half-lands is the bug this whole
	// path exists to prevent.
	live := r.ExternalID("mattermost", "engineering-lead")
	if live == "" {
		t.Fatal("the seat lost its identity entirely")
	}
	ids := r.Identities("mattermost")
	if len(ids) != 1 || ids[live] != "engineering-lead" {
		t.Fatalf("the seat holds %d ids after concurrent renames: %v", len(ids), ids)
	}
}

// assertBijection checks the invariant the whole identity map rests on: in
// every namespace, the forward and reverse maps are exact inverses. Breaking
// it raises nothing — it produces a seat whose outbound identity and inbound
// attribution name two different accounts — so it is asserted directly
// rather than defended by a branch at each write site.
func assertBijection(t *testing.T, r *notify.Registry, namespaces ...string) {
	t.Helper()
	for _, ns := range namespaces {
		forward := r.Identities(ns)
		seen := make(map[string]string, len(forward))
		for id, handle := range forward {
			if prior, dup := seen[handle]; dup {
				t.Fatalf("%s: seat %q holds both %q and %q", ns, handle, prior, id)
			}
			seen[handle] = id
			if back := r.ExternalID(ns, handle); back != id {
				t.Fatalf("%s: %q -> %q, but %q -> %q", ns, id, handle, handle, back)
			}
		}
		for p := range r.All() {
			id := r.ExternalID(ns, p.Handle)
			if id == "" {
				continue
			}
			if got := forward[id]; got != p.Handle {
				t.Fatalf("%s: %q -> %q, but %q -> %q",
					ns, p.Handle, id, id, got)
			}
		}
	}
}

// The invariant survives the full lifecycle: register, rename, hand a freed
// id to another seat, withdraw, re-register.
func TestTheTwoDirectionsStayExactInverses(t *testing.T) {
	r := registry(t)
	const ns = "mattermost"
	bot := notify.BotNamespace(ns)

	steps := []func(){
		func() { _ = r.Register(ns, "guessed", "engineering-lead") },
		func() { _ = r.Register(bot, "uid-1", "engineering-lead") },
		func() { _ = r.Register(ns, "server", "engineering-lead") },
		func() { _ = r.Register(ns, "guessed", "backend-engineer") },
		func() { _ = r.Register(ns, "server", "backend-engineer") }, // refused
		func() { r.Unregister(ns, "server", "engineering-lead") },
		func() { _ = r.Register(ns, "server", "backend-engineer") }, // now free
		func() { _ = r.Register(ns, "third", "dana-founder") },
		func() { r.Unregister(bot, "uid-1", "engineering-lead") },
		func() { r.Unregister(ns, "third", "dana-founder") },
	}
	for i, step := range steps {
		step()
		assertBijection(t, r, ns, bot)
		if t.Failed() {
			t.Fatalf("the bijection broke at step %d", i)
		}
	}
}

// ---------------------------------------------------------------- //
// Human-contact reconcile
// ---------------------------------------------------------------- //

func env(pairs map[string]string) org.EnvLookup {
	return func(name string) (string, bool) {
		v, ok := pairs[name]
		return v, ok
	}
}

func TestHumanContactsRegisterFromTheOrg(t *testing.T) {
	o := company()
	o.Normalize()
	r := notify.NewRegistry(o, nil)

	rec := r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if rec.Registered != 2 {
		t.Fatalf("registered %d identities, want 2: %+v", rec.Registered, rec)
	}
	p, ok := r.ByExternalID("slack", "U0FOUNDER")
	if !ok || p.Handle != "dana-founder" || !p.Human {
		t.Fatalf("the Slack id resolved to %+v", p)
	}
	// GitHub logins are case-normalised by the org model, so the payload's
	// own casing is what has to match.
	if _, ok := r.ByExternalID("github", "danaf"); !ok {
		t.Fatalf("the GitHub login did not resolve: %v", r.Identities("github"))
	}
}

// A reconcile, not an append: an edited id must stop attributing the old
// value to the seat, or it does so for the life of the process.
func TestAReconcileWithdrawsItsOwnStalePairs(t *testing.T) {
	o := company()
	o.Normalize()
	r := notify.NewRegistry(o, nil)
	r.ReconcileHumanContacts(o, env(nil), notify.Standing{})

	next := company()
	next.Roles[2].Contact.SlackUserID = "U0CORRECTED"
	next.Normalize()

	rec := r.ReconcileHumanContacts(next, env(nil), notify.Standing{})
	if rec.Withdrawn != 1 {
		t.Fatalf("withdrew %d, want 1: %+v", rec.Withdrawn, rec)
	}
	if _, ok := r.ByExternalID("slack", "U0FOUNDER"); ok {
		t.Fatal("the corrected id still resolves to the old value")
	}
	if p, ok := r.ByExternalID("slack", "U0CORRECTED"); !ok || p.Handle != "dana-founder" {
		t.Fatalf("the corrected id resolved to %+v", p)
	}
}

// An id moving between two human seats is one withdraw and one register.
// Doing them in the wrong order makes the move collide with itself.
func TestAnIDMovingBetweenHumanSeatsLands(t *testing.T) {
	o := company()
	o.Roles = append(o.Roles, &org.Role{
		Name: "Sam Ops", Kind: org.KindHuman,
		Contact: &org.HumanContact{GitLabUsername: "shared"},
	})
	o.Normalize()
	r := notify.NewRegistry(o, nil)
	r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if p, _ := r.ByExternalID("gitlab", "shared"); p.Handle != "sam-ops" {
		t.Fatalf("the id started at %q", p.Handle)
	}

	next := company()
	next.Roles[2].Contact.GitLabUsername = "shared"
	next.Roles = append(next.Roles, &org.Role{
		Name: "Sam Ops", Kind: org.KindHuman,
		Contact: &org.HumanContact{GitLabUsername: "sam-only"},
	})
	next.Normalize()

	rec := r.ReconcileHumanContacts(next, env(nil), notify.Standing{})
	if len(rec.Conflicts) != 0 {
		t.Fatalf("the move collided with itself: %+v", rec.Conflicts)
	}
	if p, _ := r.ByExternalID("gitlab", "shared"); p.Handle != "dana-founder" {
		t.Fatalf("the id landed on %q, want dana-founder", p.Handle)
	}
}

// A human must never silently take over an AGENT's mapping, or the reverse.
func TestAReconcileNeverTakesAnAgentsIdentity(t *testing.T) {
	o := company()
	o.Roles[2].Contact.GitLabUsername = "contested"
	o.Normalize()
	r := notify.NewRegistry(o, nil)
	if err := r.Register("gitlab", "contested", "backend-engineer"); err != nil {
		t.Fatalf("register: %v", err)
	}

	rec := r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if len(rec.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", rec.Conflicts)
	}
	c := rec.Conflicts[0]
	if c.HeldBy != "backend-engineer" || c.Wanted != "dana-founder" || c.ExternalID != "contested" {
		t.Fatalf("the conflict reads %+v", c)
	}
	if p, _ := r.ByExternalID("gitlab", "contested"); p.Handle != "backend-engineer" {
		t.Fatalf("the agent lost its identity to a human: %q", p.Handle)
	}
	// And the contested pair is NOT owned, so the next reconcile must not
	// withdraw the agent's mapping on its behalf.
	r.ReconcileHumanContacts(company(), env(nil), notify.Standing{})
	if p, _ := r.ByExternalID("gitlab", "contested"); p.Handle != "backend-engineer" {
		t.Fatalf("a later reconcile stripped the agent's identity: %q", p.Handle)
	}
}

// A MATTERMOST USERNAME IS NOT AN ID ANY EVENT CARRIES. A post names its
// author by the 26-character user id, so the name registered as written was a
// key no lookup ever used, and every person on Mattermost reached the agents
// as a stranger. The name is the transport's to resolve, so the reconcile
// leaves it alone — and still counts the ${VAR} behind one that did not
// resolve, because nobody else reports that.
func TestAContactAddressedByNameIsLeftToItsTransport(t *testing.T) {
	o := company()
	o.Roles[2].Contact.MattermostUserID = "dana"
	o.Normalize()
	r := notify.NewRegistry(o, nil)

	rec := r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if rec.Registered != 2 || rec.Unresolved != 0 {
		t.Fatalf("reconcile = %+v, want the Slack and GitHub ids alone", rec)
	}
	if ids := r.Identities("mattermost"); len(ids) != 0 {
		t.Fatalf("the username was registered as though a post could name it: %v", ids)
	}
	if _, ok := r.ByExternalID("slack", "U0FOUNDER"); !ok {
		t.Fatal("skipping the username skipped the seat's other identities")
	}

	o.Roles[2].Contact.MattermostUserID = "${DANA_MATTERMOST}"
	rec = r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if rec.Unresolved != 1 {
		t.Fatalf("an unset variable behind the username went uncounted: %+v", rec)
	}
}

// An unresolved ${VAR} is skipped and COUNTED. Registering the literal text
// would match no payload any third-party app sends, and the failure would
// surface as a person who mysteriously never gets mentioned.
func TestAnUnresolvedReferenceIsSkippedAndCounted(t *testing.T) {
	o := company()
	o.Roles[2].Contact.SlackUserID = "${FOUNDER_SLACK_ID}"
	o.Normalize()
	r := notify.NewRegistry(o, nil)

	rec := r.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if rec.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1: %+v", rec.Unresolved, rec)
	}
	if r.Knows("slack", "${FOUNDER_SLACK_ID}") {
		t.Fatal("the literal reference was registered")
	}

	// The next pass picks it up once the variable is exported.
	rec = r.ReconcileHumanContacts(o, env(map[string]string{"FOUNDER_SLACK_ID": "U0REAL"}), notify.Standing{})
	if rec.Unresolved != 0 || rec.Registered != 2 {
		t.Fatalf("the exported variable did not land: %+v", rec)
	}
	if p, _ := r.ByExternalID("slack", "U0REAL"); p.Handle != "dana-founder" {
		t.Fatalf("resolved to %q", p.Handle)
	}
}

// AN ID TWO SEATS RESOLVE TO IS NEITHER'S. Validation compares contacts as
// the chart writes them, so two ${VAR} references resolving to one member id
// pass it; the reconcile then kept whichever seat it reached last, silently,
// and the other person's mail and attribution went to them. One account is not
// two people, and which of the two it is would be a guess: it is registered
// for neither, the previous holder's registration is withdrawn, and the pass
// names both seats.
func TestAnIDTwoHumanSeatsResolveToIsRegisteredForNeither(t *testing.T) {
	o := company()
	o.Roles[2].Contact.SlackUserID = "${FOUNDER_SLACK_ID}"
	o.Roles = append(o.Roles, &org.Role{
		Name: "Sam Ops", Kind: org.KindHuman,
		Contact: &org.HumanContact{SlackUserID: "${OPS_SLACK_ID}"},
	})
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Fatalf("two references are a collision validation can see: %v", err)
	}
	r := notify.NewRegistry(o, nil)
	r.ReconcileHumanContacts(o, env(map[string]string{
		"FOUNDER_SLACK_ID": "U0FOUNDER", "OPS_SLACK_ID": "U0OPS",
	}), notify.Standing{})
	if p, _ := r.ByExternalID("slack", "U0FOUNDER"); p.Handle != "dana-founder" {
		t.Fatalf("the founder's id started at %q", p.Handle)
	}

	shared := env(map[string]string{
		"FOUNDER_SLACK_ID": "U0FOUNDER", "OPS_SLACK_ID": "U0FOUNDER",
	})
	rec := r.ReconcileHumanContacts(o, shared, notify.Standing{})
	if p, ok := r.ByExternalID("slack", "U0FOUNDER"); ok {
		t.Fatalf("an id two seats resolve to is attributed to %q", p.Handle)
	}
	want := []notify.SharedIdentity{{
		Namespace: "slack", ExternalID: "U0FOUNDER",
		Handles: []string{"dana-founder", "sam-ops"},
	}}
	if !slices.EqualFunc(rec.Shared, want, func(a, b notify.SharedIdentity) bool {
		return a.Namespace == b.Namespace && a.ExternalID == b.ExternalID &&
			slices.Equal(a.Handles, b.Handles)
	}) {
		t.Fatalf("shared = %+v, want %+v", rec.Shared, want)
	}
	if len(rec.Conflicts) != 0 {
		t.Fatalf("a shared declaration was reported as a conflict: %+v", rec.Conflicts)
	}
	// The founder's GitHub login is theirs alone and stays registered.
	if p, _ := r.ByExternalID("github", "danaf"); p.Handle != "dana-founder" {
		t.Fatalf("the founder's other id resolved to %q", p.Handle)
	}

	// A SEAT THE DIRECTORY WITHHOLDS CONTENDS FOR NOTHING: it declares no
	// identity this registry holds, so the other seat's is its own again.
	suspended := notify.NewRegistry(o, nil)
	rec = suspended.ReconcileHumanContacts(o, shared, notify.StandingOf([]notify.Holder{
		{Seat: "sam-ops", Stage: iam.StageSuspended},
	}))
	if len(rec.Shared) != 0 {
		t.Fatalf("a withheld seat contended for an id: %+v", rec.Shared)
	}
	if p, _ := suspended.ByExternalID("slack", "U0FOUNDER"); p.Handle != "dana-founder" {
		t.Fatalf("with the other seat withheld, the id resolved to %q", p.Handle)
	}
}

// A human seat flipped to an agent, or removed outright, must stop being
// reachable at the ids it declared.
func TestASeatThatStopsBeingHumanLosesItsContactIDs(t *testing.T) {
	o := company()
	o.Normalize()
	r := notify.NewRegistry(o, nil)
	r.ReconcileHumanContacts(o, env(nil), notify.Standing{})

	next := company()
	next.Roles = next.Roles[:2] // the human seat is gone
	next.Normalize()

	rec := r.ReconcileHumanContacts(next, env(nil), notify.Standing{})
	if rec.Withdrawn != 2 || rec.Registered != 0 {
		t.Fatalf("reconcile after removal: %+v", rec)
	}
	if r.Knows("slack", "U0FOUNDER") || r.Knows("github", "danaf") {
		t.Fatal("a removed seat is still reachable at its declared ids")
	}
}

func TestReconcilingNothingIsSafe(t *testing.T) {
	r := registry(t)
	rec := r.ReconcileHumanContacts(nil, env(nil), notify.Standing{})
	if rec.Registered != 0 || rec.Withdrawn != 0 || rec.Unresolved != 0 || len(rec.Conflicts) != 0 {
		t.Fatalf("reconciling a nil org did something: %+v", rec)
	}
}
