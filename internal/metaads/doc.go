// Package metaads is the Meta Ads integration: every operation of Meta's
// Marketing API v27.0 an ads integration reaches, classed by the coverage
// matrix its reference page publishes, reached by the engine for the agents as
// one system user of the company's own Meta app (ADR-0170). Organic Pages and
// Instagram publishing, WhatsApp, Commerce and Rights Manager are other
// products, left out ([specgen] lists each by its domain).
//
// # Tokens
//
// The company pastes three tokens of one Employee system user: a read token
// holding no permission that writes, an operate token (ads_management) for
// changes, and optionally an admin token (business_management) used only for an
// account change an administrator allowed. Each call goes through the least
// privileged token holding every permission its operation needs, and a call
// made at once never through the admin token. The bearer travels in the
// Authorization header and every call carries appsecret_proof; no token, app
// secret or proof reaches a log, an error, a record or an answer, and an
// answer's paging links are dropped, since Meta writes the token into them.
//
// An expiring token is renewed at fifty days of its sixty, once a day until one
// renewal holds, under the fleet lease refresh/meta_ads. The renewed token is
// sealed in the estate beside the SHA-256 of the pasted token it descends from,
// and used only while the document still names that paste: a new paste wins,
// and a cleared one takes its renewal with it.
//
// # Classes
//
// A write is classed by its operation's field rules against its target as read
// just before (growth.ClassifyIn): an update field no rule names counts as
// spend; a pause, a lower, a rename and a label are operate; archiving and
// deleting are admin, which Meta cannot undo. A lowering the engine cannot prove
// from the two values (narrower targeting, a stricter rule) keeps the raised
// class. A state the engine reads lowers a write as the matrix says: a create
// made PAUSED, a creative swapped on a paused ad, an audience no active ad set
// uses, a rule that only pauses, a test event, a draft Instant Experience, an
// unpublished Page post. A dry run (execution_options validate_only) is read.
//
// # Scope
//
// A seat names its ad accounts. Every object a request names, in its path, its
// parameters and every batched request, is held to them: an ad account by its
// ID, an account's object by the account_id Meta answers for it, read from the
// object itself, and anything no ad account owns (a Page, a pixel, a catalog,
// the business) only by a seat given business_resources. A list that can name
// another account's objects is answered with the seat's alone, and a field
// expansion, which could follow an edge out of the seat's accounts, is refused.
//
// # Spend
//
// Spend passes internal/spendguard at the ask and again at the apply under the
// ad account's lease. Daily capacity sums, over campaigns whose own status is
// ACTIVE and that have not ended, the campaign's daily budget where it holds
// the budget and otherwise its ACTIVE ad sets' daily budgets, a lifetime budget
// counted as its remainder over its remaining days, rounded up. Meta writes
// budgets in the account currency's offset units (cents, or whole units for the
// eleven currencies Meta offsets by one), read exactly into micros.
//
// # Rate limits
//
// Meta says each ad account's business use case usage on every answer. Agents'
// reads stop at nine tenths of any limit, which is kept for approved acts and
// the pass, and wait while Meta asks for time to regain access; only a read is
// retried. An ad set's budget changes at most four times an hour, counted from
// the access log before a person is asked.
package metaads
