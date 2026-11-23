// Package appstoreconnect is the App Store Connect integration: every
// operation of Apple's App Store Connect API, reached by the engine for the
// agents through the company's team keys, one per Apple role (ADR-0170).
//
// # Keys and tokens
//
// A team key is pasted once per role and sealed; the engine signs a token per
// request ([Client]), ES256, living ten minutes from a minute back, scoped to
// that one request, so a token that reaches a log repeats only the call it was
// made for. Each call goes through the least-privileged connected key whose
// role may make it, and a refused key is never retried with another.
//
// # A seat's own key
//
// A seat set to `identity: own` signs its calls with its own team key
// (ADR-0179): a person makes it at Apple at the role the seat's level needs
// ([LevelRole]: Marketing to read, App Manager to operate or administer),
// never Admin, and hands in its .p8 file. Apple has no call that names a
// key's role, so the check reads it from seven list reads whose roles the
// operation table records, after one read every role may make that tells a
// dead or foreign key from a narrow one. The key is kept only once the check
// finds it at a fitting role in the team of the seat's apps, and is read back
// every pass. A read its role may not make (a sales report) goes through the
// company's least-privileged key, which the access log records as the
// company's; an Admin-only act an administrator allowed goes through the
// company's Admin key; every other change goes through the seat's key or is
// refused, and a seat whose key Apple refuses has every call refused. Apple
// lets no key be revoked by API, so a replaced or given-up key stays sealed as
// retiring until a pass finds Apple refusing it.
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
// owned resource whose links are all empty is in no app, and the team's. No
// placement read goes through the Admin key.
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
// which the lease outlives; a write cut there is an unknown outcome.
package appstoreconnect
