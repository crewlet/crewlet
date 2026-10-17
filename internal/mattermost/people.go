package mattermost

import (
	"context"
	"fmt"
	"maps"
	"slices"
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
// reported, and so is one a BOT holds: registered, every post that bot made
// would reach the agents as the person's. And a request that FAILED changes
// nothing, because a server mid-restart says nothing about who anybody is,
// and treating it as "nobody" would make every person a stranger for as long
// as the outage lasted.
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
// withheld seat reported, because a name that holds nobody is nothing the
// operator owes a fix for while its owner is not to be reached at all. The name
// is still ASKED about, so the registry that reinstates them is filled from the
// last answer at once rather than after the next request.

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

	// accounts is the user id the server last answered for each name, and
	// absent why it holds no person under the others it was asked about.
	// Facts about the server, not about the org — which is what lets an
	// apply fill a fresh registry from them before asking again. Pruned to
	// want's names on every answer, so a name the chart dropped is not
	// carried for ever.
	accounts map[string]string
	absent   map[string]absence
}

// absence is why a name the chart gives a person holds no person.
type absence string

const (
	// noAccount is a name the server answered without: nobody holds it, or
	// nobody the bots may see.
	noAccount absence = "no_account"

	// botAccount is a name a BOT holds. Never registered for the person,
	// because what it would attribute to them is that bot's every post —
	// an agent's words presented to its colleagues as a person's, with the
	// person's authority behind them.
	botAccount absence = "bot_account"
)

// unknownPerson is a person whose name holds no person, and why.
type unknownPerson struct {
	Person
	why absence
}

// unknown is the people reg does not withhold whose name the last answer said
// holds no person, in want's order.
func (p *people) unknown(reg *notify.Registry) []unknownPerson {
	var out []unknownPerson
	for _, person := range reachable(reg, p.want) {
		if why := p.absent[person.Username]; why != "" {
			out = append(out, unknownPerson{Person: person, why: why})
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
	t.settlePeople(ctx, names, found)
}

// settlePeople records one answer, registers it, and says what moved.
func (t *Transport) settlePeople(ctx context.Context, asked []string, found map[string]User) {
	reg := t.target()
	t.mu.Lock()
	before, wasAbsent := maps.Clone(t.people.accounts), maps.Clone(t.people.absent)
	unknownBefore := t.people.unknown(reg)
	if t.people.accounts == nil {
		t.people.accounts, t.people.absent = map[string]string{}, map[string]absence{}
	}
	for _, name := range asked {
		user, ok := found[name]
		switch {
		case !ok:
			delete(t.people.accounts, name)
			t.people.absent[name] = noAccount
		case user.IsBot:
			delete(t.people.accounts, name)
			t.people.absent[name] = botAccount
		default:
			t.people.accounts[name] = user.ID
			delete(t.people.absent, name)
		}
	}
	current := map[string]bool{}
	for _, person := range t.people.want {
		current[person.Username] = true
	}
	maps.DeleteFunc(t.people.accounts, func(name, _ string) bool { return !current[name] })
	maps.DeleteFunc(t.people.absent, func(name string, _ absence) bool { return !current[name] })
	// SAID ONLY OF THE PEOPLE THE REGISTRY ROUTES: a withheld seat is
	// asked about and never reported (see the package notes above).
	want := reachable(reg, t.people.want)
	now, nowAbsent := maps.Clone(t.people.accounts), maps.Clone(t.people.absent)
	unknownAfter := t.people.unknown(reg)
	onChange := t.onPeople
	t.mu.Unlock()

	t.registerPeople()

	// SAID ON THE TRANSITION, never on every answer: an ask a minute that
	// repeated a missing person on each one would bury the line that
	// matters under fifty-nine copies of it an hour.
	if !maps.Equal(before, now) || !maps.Equal(wasAbsent, nowAbsent) {
		found := 0
		for _, person := range want {
			if now[person.Username] != "" {
				found++
			}
		}
		log.InfoContext(ctx, "mattermost_people_resolved", "people", len(want),
			"found", found, "unknown", len(unknownAfter))
	}
	for _, person := range want {
		name := person.Username
		switch why := nowAbsent[name]; {
		case why != "" && why != wasAbsent[name]:
			log.WarnContext(ctx, "mattermost_person_unknown",
				"handle", person.Handle, "username", name, "reason", string(why),
				"detail", why.sentence(person))
		case now[name] != "" && wasAbsent[name] != "":
			log.InfoContext(ctx, "mattermost_person_found",
				"handle", person.Handle, "username", name, "user_id", now[name])
		case now[name] != "" && before[name] != "" && before[name] != now[name]:
			// THE NAME NOW BELONGS TO ANOTHER ACCOUNT: the person it
			// named renamed themselves and somebody took the name, or
			// the account was deleted and made again. The chart says
			// the name, so the name's holder is who this seat is — but
			// it is a change of who a colleague is, and said as one.
			log.WarnContext(ctx, "mattermost_person_account_changed",
				"handle", person.Handle, "username", name,
				"user_id", now[name], "previous_user_id", before[name])
		}
	}
	if onChange != nil && !slices.Equal(unknownBefore, unknownAfter) {
		onChange()
	}
}

// sentence is what is wrong with a person's name, for the log line and the
// finding alike, so the two never describe one person two ways.
func (why absence) sentence(person Person) string {
	if why == botAccount {
		return fmt.Sprintf("%s's contact.mattermost_user_id is %q, which is a "+
			"bot's account, so it is not registered for them: what it posts "+
			"would reach the agents as %s's word", person.Handle, person.Username,
			person.Handle)
	}
	return fmt.Sprintf("%s's contact.mattermost_user_id is %q, and no Mattermost "+
		"account by that name is visible to this company's bots, so every post %s "+
		"writes reaches the agents from a stranger and a mention of the name "+
		"renders as plain text", person.Handle, person.Username, person.Handle)
}

// registerPeople writes what is known about each person into the live
// registry: the account their name holds, or nothing.
//
// NO I/O, so an apply can call it on the way past. A person with no known
// account has whatever id this transport registered for them withdrawn — the
// only writer of a person's id in this namespace is this transport, so the id
// the registry holds for their handle is the one to take back. A person the
// registry withholds is one with no account to register, by the directory's
// word rather than the server's.
func (t *Transport) registerPeople() {
	t.registering.Lock()
	defer t.registering.Unlock()
	t.mu.Lock()
	lookup := t.registry
	want := slices.Clone(t.people.want)
	accounts := maps.Clone(t.people.accounts)
	t.mu.Unlock()
	if lookup == nil {
		return
	}
	reg := lookup()
	if reg == nil {
		return
	}
	for _, person := range want {
		id := accounts[person.Username]
		if _, withheld := reg.Withholding(person.Handle); withheld {
			id = ""
		}
		if id == "" {
			if held := reg.ExternalID(Backend, person.Handle); held != "" {
				reg.Unregister(Backend, held, person.Handle)
			}
			continue
		}
		if err := reg.Register(Backend, id, person.Handle); err != nil {
			log.Warn("mattermost_person_not_registered", "handle", person.Handle,
				"username", person.Username, "user_id", id, "error", err.Error())
		}
	}
}

// PeopleFindings is what the last answer said about the people, in the
// integration vocabulary: one [integration.FindingContactUnknown] per person
// whose name the server answered without, among those the registry this
// transport writes into does not withhold.
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
	reg := t.target()
	t.mu.Lock()
	unknown := t.people.unknown(reg)
	t.mu.Unlock()
	out := make([]integration.Finding, 0, len(unknown))
	for _, person := range unknown {
		remedy := "Write the username their Mattermost profile shows — a renamed " +
			"account keeps its posts and loses its old name — or have them join " +
			"the server. If the account exists, check that the bots' role still " +
			"holds view_members: without it a bot sees only the people it shares " +
			"a team or channel with"
		if person.why == botAccount {
			remedy = "Write the username of the person's own account, not an agent's"
		}
		out = append(out, integration.Finding{
			Kind:    integration.FindingContactUnknown,
			Subject: person.Handle,
			Detail:  person.why.sentence(person.Person),
			Remedy:  remedy,
		})
	}
	return out
}
