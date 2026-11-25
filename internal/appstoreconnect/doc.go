// Package appstoreconnect is the App Store Connect integration: every
// operation of Apple's App Store Connect API, reached by the engine for each
// agent through that agent's own team key (ADR-0179). The company names its
// teams' issuer IDs and holds no key of any role.
//
// # Keys and tokens
//
// The engine signs a token per request ([Client]), ES256, living ten minutes
// from a minute back, scoped to that one request, so a token that reaches a
// log repeats only the call it was made for.
//
// # Every call is an agent's own
//
// Each agent signs every call, and every read the engine makes for one (which
// team, the team's apps, placement, state, the target an approval binds), with
// its own team key (ADR-0179): a person makes it at Apple at the role the
// agent's level needs ([LevelRole]: Marketing to read, or Sales for an agent
// that reads sales; App Manager to operate or administer), never Admin, and
// hands in its .p8 file, with the team's vendor number for a Sales key. Apple
// has no call that names a key's role, so the check, after one read every role
// may make that tells a dead or foreign key from a narrow one, lists the
// team's apps with the key itself (which also names them for the agents' app
// chooser) and reads the role from five reads whose roles Apple publishes and
// no team-level program gates. Only Apple's role denial (403 FORBIDDEN_ERROR)
// says a role may not; another 403, a 404 or a 409 says nothing of the role.
// The role is every one the known answers allow: several are accepted only
// when each fits the level. A refusal (an unsigned agreement, a revoked key)
// fails the check at the step Apple refused, in Apple's plain terms. The key
// is kept only once the check finds it at a fitting role in the team of the
// agent's apps, and is read back every pass. An agent with no
// working key makes no call; a call its key's role may not make, and an act
// only Admin may make, are refused before Apple is asked. Apple lets no key be
// revoked by API, so a replaced or given-up key stays sealed as retiring until
// a pass finds Apple refusing it.
//
// # Rate budget
//
// Apple limits each key per rolling hour and says what is left on every
// answer. Agents' reads stop at a tenth of a key's limit, which is kept for
// approved acts and the pass. A read waits out a 429 within httpx's cap; a
// write is never sent twice.
//
// # Scope
//
// A seat names its apps. The resource types that belong to exactly one app
// are generated from Apple's document (specgen topology): a type with a
// to-one relationship to an app or to an owned container, or one an owned
// parent reads below it that the team cannot list and that lists no such
// parents back. Types reached many-to-many (beta testers, users, devices) or
// listed team-wide (territories, categories, repositories) are not owned.
//
// Every owned resource a call names, in its path, its query's ID filters and
// its write body's data and relationships, is placed in one of the seat's
// apps before anything is sent; any other resource it names needs the seat's
// team_resources. Placement reads up the resource's to-one links to its app
// where its instance read can include one, which is exact; it searches down
// from the seat's apps through owned types alone only where none exists. An
// owned resource whose links are all empty is in no app, and the team's.
// Every placement read goes through the calling agent's own key.
//
// The cache of placements is filled only from placements and from answers
// rooted at the seat's app through owned hops, never from an answer rooted at
// a resource no app owns, which may hold another app's. Every answer loses
// the apps outside the seat's apps and the links to them. A read not rooted at
// the seat's app whose answer holds an owned resource is refused whole for a
// seat whose apps are not all its team's: placing each resource would read
// Apple once per resource, and a partial answer would misstate the list.
//
// # Every step on the access log
//
// Every record the product writes is one the access log accepts: a published
// word, free text and a value the log refuses in the clear go as their digest
// and length, and a field the log may not name is left out and counted. A
// held call records its allow before it acts, and acts only when that allow
// landed as its one answer; every way an allowed call ends records one
// outcome. Everything done under an act's lease is cut at growth.ApplyBound,
// which the lease outlives; a write cut there is an unknown outcome. A request
// that never left, and an act whose write never did, fail as their cause did,
// never as an unknown outcome.
package appstoreconnect
