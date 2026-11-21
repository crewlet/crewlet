// Package growth is the vocabulary every marketing and growth integration
// shares: ad, analytics, app store and publisher products. It is a leaf of
// types and rules; the products' packages, the engine tools in agent/builtin
// and the spend guard (internal/spendguard) act on them.
//
// # Six classes
//
// Every vendor operation and every engine tool is in exactly one [Class]:
//   - read reads, and nobody is asked;
//   - operate is a reversible change that neither starts or raises spend nor
//     publishes (a pause, a lowered budget or bid, a draft edit): nobody is
//     asked, the access log records it, and the Growth page lists it;
//   - public publishes to customers or the world, or retracts what the
//     integration published;
//   - spend starts or raises money going out;
//   - admin manages the vendor account itself (users, access, keys, billing
//     setup, policies) and makes every irreversible change that is neither
//     spend nor public, such as deleting a campaign;
//   - never is not offered, and its catalog row says why: the vendor's terms
//     forbid it, or it cannot be done.
//
// Public, spend and admin are held calls (ADR-0171): the engine builds the
// exact vendor operation, checks it (and the spend guard for spend), and
// holds it for a person holding admin, at a fresh step-up, who answers Allow
// once or Deny. None is ever delegated ([delegated.NeverDelegated]). A raise
// and an enable are spend; a pause and a lower are operate, because a brake
// must not wait on a step-up and its undo is a decision. [Classify] applies
// that rule to a generic update field by field.
//
// A seat's [Level] bounds what its agent may call or ask: read; operate,
// which also asks public and spend; administer, which also asks admin.
//
// # Every capability
//
// A product's [Catalog] lists every operation of its vendor's API with its
// class, the vendor roles that may call it and the OAuth scopes or
// permissions it needs, and every role the vendor gives an identity: the
// coverage matrix its docs page publishes. The engine reads the role each
// connection actually holds and refuses an operation that role cannot make
// ([Permits]), naming the roles that can. A product offers curated tools for
// its high-value acts and one generic tool per class over the rest of its
// catalog, so every method ships without hundreds of tools.
//
// # Credentials stay in the engine
//
// No growth credential reaches an agent's box (ADR-0170): every call passes
// through the engine's tools, which hold it to the seat's level and scope,
// the classes and the caps. internal/config refuses a growth credential's
// variable named at any other field, and a vendor's own MCP server beside its
// product.
//
// # Money
//
// Money is int64 micros of the account's currency, never a float, and figures
// in two currencies are never summed. Each adapter converts with its vendor's
// own currency table; this package parses and formats decimal amounts
// ([ParseAmount], [FormatAmount]) and converts minor units given the digits
// that table says ([FromMinor], [ToMinor]).
//
// # Days and reports
//
// An account's day is its own zone's, never the company's clock or the
// host's: a vendor reports, budgets and caps by it ([Range.Resolve]). A
// report answer ([Report]) is paged to at most [ReportMaxRows] and says when
// it is truncated, its zone, the last complete day and why it is empty.
//
// # Metrics
//
// [Metric] is the closed, append-only list of figures the growth domain keeps
// per account and day, one name per meaning: an advertiser's spend is never a
// publisher's earnings. Each declares its [Scale] and how it aggregates
// ([Aggregate]): ratios are never stored, but computed from their parts.
package growth
