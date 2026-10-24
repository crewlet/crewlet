package mattermost

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The people: the human seats a post can come from.
//
// # A person is declared by NAME and posts by ID
//
// A human seat's `contact.mattermost_user_id` is a USERNAME, because that is
// what an agent writes to mention them and what it opens a direct message
// with. A post names its author by the 26-character user id, and nothing
// else: so the username, registered as written, was a key no post ever looked
// up, and everything a person wrote reached the agents as a stranger's —
// their manager's instructions with no authority behind them, a founder's
// question read as an outsider's, a thread they started attributed to nobody.
// The party registry therefore holds each person's USER ID, and this
// transport is where it comes from: it asks the server which account holds
// each name ([Client.UsersByUsernames]) and registers what it is answered
// with. [notify.Registry.ReconcileHumanContacts] leaves the name alone for
// that reason ([org.Transport.AddressedByName]).
//
// # When it asks
//
// A name is not a fixed fact about an account: a person renames themselves,
// and a name the chart declares may belong to nobody yet, because the person
// has not joined. So it is asked
//
//   - AT CONNECT, before any socket is attached, so the first post a socket
//     hears is attributed — once the seats' identities are read, which proves
//     the server is up and hands over a token to ask with;
//   - ON EVERY APPLY ([Transport.Reregister]), because the apply is what can
//     name a new person or correct a name; the new registry is filled from
//     what is already known first, with no request, so nobody who resolved a
//     moment ago is a stranger while the question is asked again;
//   - and every [PeopleRefresh], because the two changes AT THE SERVER — an
//     account created under a name the chart already declares, and a rename —
//     reach this engine by no other route.
//
// Mattermost does broadcast both on its websocket (`new_user`,
// `user_updated`, server/channels/app/user.go), and they are deliberately not
// what this listens to: a node holds one socket per seat, so each would
// arrive once per seat; nothing replays one a socket missed while it was down;
// and the answer they would prompt is the same request asked here on a timer
// — so they would buy under a minute of latency at the price of a second path
// that is right only while every socket is.
//
// # Absent is an answer; a failure is not
//
// A name the server answered for maps to that account's id. A name it answered
// WITHOUT — no account holds it, or none the bots may see — is withdrawn and
// reported. And a request that FAILED changes nothing, because a server
// mid-restart says nothing about who anybody is, and treating it as "nobody"
// would make every person a stranger for as long as the outage lasted.
//
// # A person the directory withholds is nobody's here
//
// Whether the person holding a human seat may still be reached is the identity
// directory's to say, not the chart's, and a registry is built from one reading
// of it: a seat whose holder is suspended, retired or removed has every contact
// identity left out ([notify.Registry.Withholding]). The account a name
// resolves to is one of those identities, so a withheld seat gets none here
// either — and it is judged against the registry being WRITTEN, the one reading
// that registry answers for, never against the people handed over beside it: a
// directory rebuild that lands while a transport is connecting rebuilds a
// registry this transport is not yet told about, and a list filtered for the
// previous reading would register a suspended person into the next. Nor is a
// withheld seat judged at all — not reported, and contending for no account
// with anybody, as the human reconcile leaves it out of its duplicates —
// because a name that holds nobody is nothing the operator owes a fix for while
// its owner is not to be reached. The name is still ASKED about, so the
// registry that reinstates them is filled from the last answer at once rather
// than after the next request.
//
// # An account is one person's, or nobody's
//
// What is registered for a person is attributed to them with their authority,
// so an account that is not the person's own is refused rather than
// registered, and reported naming why ([absence]):
//
//   - an account one of this company's own seats posts as, bot or not, and
//     any other BOT's: registered, every post that agent made would reach its
//     colleagues as the person's word — and the person would win, since a
//     lookup consults the people's namespace before the bots';
//   - an account that ANOTHER person's name holds too. The duplicate check
//     compares what the chart declares, so two `${VAR}`s that resolve to one
//     name pass it; and one account cannot be two people, so it is
//     registered for neither rather than for whichever came first, which
//     would be a guess.
//
// # The name is the authority, and a move is said
//
// Whoever holds the name on the server is the person, which is what the chart
// says when it names them by it: the agents already mention and message
// whoever holds `@jane`, and a person who joins after boot is recognised by
// exactly this rule. So a name that comes to hold ANOTHER account — the
// person renamed themselves and somebody took the name, or the account was
// made again — moves the seat to that account on every node alike, since each
// one asks the same server. A node that remembered the old account and
// refused the new one would answer differently from a node started after the
// move, and one post would then reach a seat as the founder's on one node and
// a stranger's on another.
//
// The move is SAID instead: logged when it is seen, and reported on the
// integration's status ([integration.FindingContactMoved]) against the
// account the name held when the company last stated it — the last apply, or
// the first answer since — until the next apply states it again. ANY apply:
// the re-activation a provisioning pass makes after sealing a credential it
// minted clears it too, which is why the log line, not the finding, is the
// record of a move. But ONLY an apply: a registry the identity directory
// rebuilds for the same company states nothing about anybody's name, and a
// suspension elsewhere clearing a report nobody answered would be the report
// withdrawn by an event that says nothing about it. In memory, so a node started after the move has nothing
// to compare against; every node running when it happened sees it within a
// [PeopleRefresh].

// PeopleRefresh is how often a node asks the server again which account each
// person's username names.
//
// A MINUTE, because what it bounds is how long a person is a stranger after
// joining the server under the name the chart already gives them — and
// somebody who has just joined a company's chat is likely to be writing to its
// agents — while what it costs is ONE request per node per minute however
// many people the company names, since the names travel in one batch. Ten nodes is one
// request every six seconds against an instance that serves a typing
// indicator every five per active turn. A rename needs nothing faster: the
// account keeps its id, so until the next ask the person is still recognised
// by it, and the ask is what reports that the chart's name for them is stale.
const PeopleRefresh = time.Minute

// Person is one human seat's Mattermost username.
type Person struct {
	Handle string
	// Username is normalised as the server stores it ([NormalizeUsername]).
	Username string
}

// PeopleFrom reads the human seats' usernames from an org, in its order.
//
// Through [org.HumanContact.ResolvedIdentities], so a `${VAR}` resolves here
// exactly as everywhere else it is read, and one that does not is left out —
// and counted by the human reconcile, which reports it.
func PeopleFrom(o *org.Organization, lookup org.EnvLookup) []Person {
	if o == nil {
		return nil
	}
	var out []Person
	for role := range o.AllRoles() {
		handle := role.Handle()
		if !role.IsHuman() || role.Contact == nil || handle == "" {
			continue
		}
		for _, id := range role.Contact.ResolvedIdentities(lookup) {
			if id.Transport != org.TransportMattermost {
				continue
			}
			if name := NormalizeUsername(id.ExternalID); name != "" {
				out = append(out, Person{Handle: handle, Username: name})
			}
		}
	}
	return out
}

// people is what this transport knows about the human seats. Guarded by the
// transport's mutex.
type people struct {
	// want is the people of the org whose registry this transport writes
	// into, replaced whole by an apply.
	want []Person

	// held is the account the server last answered for each name, and
	// missing the names it last answered without. Facts about the SERVER,
	// not about the org — which is what lets an apply judge a fresh
	// registry from them before asking again. Pruned to want's names, so a
	// name the chart dropped is not carried for ever.
	held    map[string]User
	missing map[string]bool

	// agents is the account each of this company's seats posts as, by user
	// id, read when the seats are identified and before the people are
	// first asked about.
	agents map[string]string

	// anchor is the account each name held when the company last stated it:
	// the first registered for it since the last apply, or the one it held
	// at that apply. Kept while the name holds nobody, so a name that comes
	// back on another account is seen to have moved.
	anchor map[string]string

	// reported is every person's standing as last registered, logged and
	// reported, in want's order — every person the registry it was
	// registered into does not withhold.
	reported []standing
}

// absence is why a name the chart gives a person holds no account that is
// registered for them.
type absence string

const (
	// noAccount is a name the server answered without: nobody holds it, or
	// nobody the bots may see.
	noAccount absence = "no_account"

	// agentAccount is the account one of this company's own seats posts as.
	agentAccount absence = "agent_account"

	// botAccount is a name a BOT holds that no seat here posts as.
	botAccount absence = "bot_account"

	// sharedAccount is an account another person's name holds too.
	sharedAccount absence = "shared_account"
)

// move is a name now holding another account than the company last stated.
type move struct {
	from, to string
}

// standing is what is known about one person.
type standing struct {
	Person
	// id is the account registered for them, empty when there is none.
	id string
	// why is why there is none, empty when id is set or the server has not
	// answered for the name yet.
	why absence
	// with names who else holds the account: the agent seat's handle for
	// agentAccount, the other people's for sharedAccount.
	with string
	// moved is set while the name holds another account than its anchor.
	moved move
}

// standings judges every person reg does not withhold from what the server
// last said. Pure: no field changes.
func (p *people) standings(reg *notify.Registry) []standing {
	want := reachable(reg, p.want)
	out := make([]standing, 0, len(want))
	claims := map[string][]string{}
	for _, person := range want {
		s := standing{Person: person}
		user, held := p.held[person.Username]
		switch {
		case p.missing[person.Username]:
			s.why = noAccount
		case !held:
			// Not answered yet, which is not "nobody holds it".
		case p.agents[user.ID] != "":
			s.why, s.with = agentAccount, p.agents[user.ID]
		case user.IsBot:
			s.why = botAccount
		default:
			s.id = user.ID
			claims[user.ID] = append(claims[user.ID], person.Handle)
		}
		out = append(out, s)
	}
	for i := range out {
		s := &out[i]
		if others := slices.DeleteFunc(slices.Clone(claims[s.id]), func(h string) bool {
			return h == s.Handle
		}); s.id != "" && len(others) > 0 {
			s.id, s.why, s.with = "", sharedAccount, strings.Join(others, ", ")
			continue
		}
		if from := p.anchor[s.Username]; s.id != "" && from != "" && from != s.id {
			s.moved = move{from: from, to: s.id}
		}
	}
	return out
}

// reachable is the people reg does not withhold, in their order. A nil
// registry withholds nobody, having nothing to register into.
func reachable(reg *notify.Registry, people []Person) []Person {
	if reg == nil {
		return people
	}
	return slices.DeleteFunc(slices.Clone(people), func(person Person) bool {
		_, withheld := reg.Withholding(person.Handle)
		return withheld
	})
}

// target is the registry this transport writes into now, or nil. Read
// outside the transport's mutex, because the engine's live registry is behind
// a lock of the engine's own.
func (t *Transport) target() *notify.Registry {
	t.mu.Lock()
	lookup := t.registry
	t.mu.Unlock()
	if lookup == nil {
		return nil
	}
	return lookup()
}

// prune drops every fact about a name the chart no longer gives anybody.
func (p *people) prune() {
	current := map[string]bool{}
	for _, person := range p.want {
		current[person.Username] = true
	}
	stale := func(name string) bool { return !current[name] }
	maps.DeleteFunc(p.held, func(name string, _ User) bool { return stale(name) })
	maps.DeleteFunc(p.missing, func(name string, _ bool) bool { return stale(name) })
	maps.DeleteFunc(p.anchor, func(name, _ string) bool { return stale(name) })
}

// pokePeople asks the refresh loop to ask the server again now. Never
// blocking: one ask pending is as good as many, since it reads the people as
// they are when it runs.
func (t *Transport) pokePeople() {
	select {
	case t.peoplePoke <- struct{}{}:
	default:
	}
}

// startPeople runs the refresh loop until [Transport.Stop].
//
// Every ask after the one at connect runs here, on this one goroutine, so two
// answers never race to write their facts in the opposite order to the one
// they were asked in.
func (t *Transport) startPeople(ctx context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	t.mu.Lock()
	t.peopleStop, t.peopleDone = cancel, done
	t.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(t.peopleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-t.peoplePoke:
			}
			t.resolvePeople(ctx, t.clients())
		}
	}()
}

// stopPeople ends the refresh loop and waits for it.
func (t *Transport) stopPeople() {
	t.mu.Lock()
	cancel, done := t.peopleStop, t.peopleDone
	t.peopleStop, t.peopleDone = nil, nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// clients is every running seat's client, by handle, for an ask to try in
// turn.
func (t *Transport) clients() []*Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Client, 0, len(t.seats))
	for _, handle := range slices.Sorted(maps.Keys(t.seats)) {
		if c := t.seats[handle].client; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// resolvePeople asks the server which account holds each person's name, and
// registers what it is answered with.
//
// AS ANY SEAT, because what a bot may see is what the transport hears: the
// first client whose token the server takes answers, and the next is tried
// only when the server refused that one ([refusesToken]) — any other failure
// is the server's, and the next token would meet it again.
func (t *Transport) resolvePeople(ctx context.Context, clients []*Client) {
	t.mu.Lock()
	want := slices.Clone(t.people.want)
	t.mu.Unlock()
	if len(want) == 0 {
		return
	}
	names := make([]string, 0, len(want))
	for _, person := range want {
		if !slices.Contains(names, person.Username) {
			names = append(names, person.Username)
		}
	}

	var (
		users []User
		err   = fmt.Errorf("mattermost: no seat is connected to ask with")
	)
	for _, c := range clients {
		users, err = c.UsersByUsernames(ctx, names)
		if !refusesToken(err) {
			break
		}
	}
	if err != nil {
		log.WarnContext(ctx, "mattermost_people_unread", "people", len(names),
			"error", err.Error(),
			"detail", "which account each person's username names could not be "+
				"read, so what the last answer said stands and the server is "+
				"asked again on the next refresh")
		return
	}
	found := make(map[string]User, len(users))
	for _, u := range users {
		if name := NormalizeUsername(u.Username); name != "" && u.ID != "" {
			found[name] = u
		}
	}
	t.mu.Lock()
	for _, name := range names {
		if user, ok := found[name]; ok {
			t.people.held[name] = user
			delete(t.people.missing, name)
		} else {
			delete(t.people.held, name)
			t.people.missing[name] = true
		}
	}
	t.mu.Unlock()
	t.publishPeople()
}

// publishPeople judges every person from what is known, registers the result
// and says what moved.
//
// NO I/O, so an apply can call it on the way past. Held whole under
// [Transport.registering], from the judgement to the last word said, so an
// apply and an answer each publish what they read and the later one always
// publishes last.
func (t *Transport) publishPeople() {
	t.registering.Lock()
	defer t.registering.Unlock()
	// ONE REGISTRY for the judgement and the writes, so a person is judged
	// by the reading of the directory the registry they land in answers for.
	reg := t.target()
	t.mu.Lock()
	t.people.prune()
	now := t.people.standings(reg)
	for _, s := range now {
		if s.id != "" && t.people.anchor[s.Username] == "" {
			t.people.anchor[s.Username] = s.id
		}
	}
	before := t.people.reported
	t.people.reported = now
	onChange := t.onPeople
	t.mu.Unlock()

	if reg != nil {
		registerPeople(reg, now)
	}
	sayPeople(before, now)
	if onChange != nil && !slices.EqualFunc(peopleFindings(before), peopleFindings(now),
		integration.Finding.Same) {
		onChange()
	}
}

// registerPeople writes each person's standing into a registry: the account
// they hold, or nothing. A person the registry withholds is not among them,
// and holds nothing there to withdraw: a registry is built from one reading,
// and this is the only writer of a person's id in it.
//
// EVERY WITHDRAWAL BEFORE ANY REGISTRATION, as the human reconcile does, so
// an account moving from one person to another in one answer is a withdraw
// and a register rather than a collision with itself. The only writer of a
// person's id in this namespace is this transport, so the id the registry
// holds for their handle is the one to take back.
func registerPeople(reg *notify.Registry, people []standing) {
	for _, s := range people {
		if held := reg.ExternalID(Backend, s.Handle); held != "" && held != s.id {
			reg.Unregister(Backend, held, s.Handle)
		}
	}
	for _, s := range people {
		if s.id == "" {
			continue
		}
		if err := reg.Register(Backend, s.id, s.Handle); err != nil {
			log.Warn("mattermost_person_not_registered", "handle", s.Handle,
				"username", s.Username, "user_id", s.id, "error", err.Error())
		}
	}
}

// sayPeople logs what changed between two publications.
//
// SAID ON THE TRANSITION, never on every answer: an ask a minute that
// repeated a missing person on each one would bury the line that matters
// under fifty-nine copies of it an hour.
func sayPeople(before, now []standing) {
	if slices.Equal(before, now) {
		return
	}
	was := make(map[string]standing, len(before))
	for _, s := range before {
		was[s.Handle] = s
	}
	found, unknown := 0, 0
	for _, s := range now {
		prev := was[s.Handle]
		switch {
		case s.why != "":
			unknown++
			if s.why != prev.why || s.with != prev.with || s.Username != prev.Username {
				log.Warn("mattermost_person_unknown", "handle", s.Handle,
					"username", s.Username, "reason", string(s.why),
					"detail", s.sentence())
			}
		case s.id != "":
			found++
			if prev.why != "" {
				log.Info("mattermost_person_found", "handle", s.Handle,
					"username", s.Username, "user_id", s.id)
			}
		}
		if s.moved != (move{}) && s.moved != prev.moved {
			log.Warn("mattermost_person_account_changed", "handle", s.Handle,
				"username", s.Username, "user_id", s.moved.to,
				"previous_user_id", s.moved.from, "detail", s.movedSentence())
		}
	}
	log.Info("mattermost_people_resolved", "people", len(now), "found", found,
		"unknown", unknown)
}

// sentence is what is wrong with a person's name, for the log line and the
// finding alike, so the two never describe one person two ways.
func (s standing) sentence() string {
	switch s.why {
	case agentAccount:
		return fmt.Sprintf("%s's contact.mattermost_user_id is %q, the account the "+
			"agent seat %s posts as, so it is not registered for them: what that "+
			"agent posts would reach its colleagues as %s's word", s.Handle,
			s.Username, s.with, s.Handle)
	case botAccount:
		return fmt.Sprintf("%s's contact.mattermost_user_id is %q, which is a "+
			"bot's account, so it is not registered for them: what it posts "+
			"would reach the agents as %s's word", s.Handle, s.Username, s.Handle)
	case sharedAccount:
		return fmt.Sprintf("%s's contact.mattermost_user_id is %q, and it names the "+
			"same Mattermost account as the contact of %s: one account is not two "+
			"people, so it is registered for neither, and what it posts reaches "+
			"the agents from a stranger", s.Handle, s.Username, s.with)
	default:
		return fmt.Sprintf("%s's contact.mattermost_user_id is %q, and no Mattermost "+
			"account by that name is visible to this company's bots, so every post %s "+
			"writes reaches the agents from a stranger and a mention of the name "+
			"renders as plain text", s.Handle, s.Username, s.Handle)
	}
}

// movedSentence is what a name moving to another account means.
func (s standing) movedSentence() string {
	return fmt.Sprintf("%s's contact.mattermost_user_id is %q, which named the "+
		"Mattermost account %s when the company was last applied and names %s "+
		"now: every post that account makes reaches the agents as %s's, with "+
		"their authority", s.Handle, s.Username, s.moved.from, s.moved.to, s.Handle)
}

// PeopleFindings is what the transport last published about the people, in
// the integration vocabulary: an [integration.FindingContactUnknown] per
// person no account is registered for, and an
// [integration.FindingContactMoved] per person whose name has moved — among
// the people the registry it published into does not withhold.
//
// FROM THE ANSWER THE TRANSPORT ACTS ON, never a second ask: the reconcile
// pass that reports it runs on one node with an administrator's token, which
// sees accounts a bot cannot, so asking there would report a person as known
// whose posts this transport attributes to nobody. Nothing before the first
// answer, because "not asked yet" is not "nobody holds the name".
func (t *Transport) PeopleFindings() []integration.Finding {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	reported := t.people.reported
	t.mu.Unlock()
	return peopleFindings(reported)
}

// peopleFindings is the findings a publication reports.
func peopleFindings(people []standing) []integration.Finding {
	var out []integration.Finding
	for _, s := range people {
		if s.why != "" {
			remedy := "Write the username their Mattermost profile shows — a renamed " +
				"account keeps its posts and loses its old name — or have them join " +
				"the server. If the account exists, check that the bots' role still " +
				"holds view_members: without it a bot sees only the people it shares " +
				"a team or channel with"
			switch s.why {
			case agentAccount, botAccount:
				remedy = "Write the username of the person's own account, not an agent's"
			case sharedAccount:
				remedy = "Give each of these seats the username of their own account. " +
					"The duplicate check compares the contact as written, so two " +
					"${VAR} references that resolve to one name pass it"
			}
			out = append(out, integration.Finding{
				Kind:    integration.FindingContactUnknown,
				Subject: s.Handle,
				Detail:  s.sentence(),
				Remedy:  remedy,
			})
		}
		if s.moved != (move{}) {
			out = append(out, integration.Finding{
				Kind:    integration.FindingContactMoved,
				Subject: s.Handle,
				Detail:  s.movedSentence(),
				Remedy: "If that account is not theirs, write the username their " +
					"profile shows now. If it is — the account was made again — " +
					"POST /config/reload, which re-applies the company unchanged; " +
					"any apply clears this",
			})
		}
	}
	return out
}
