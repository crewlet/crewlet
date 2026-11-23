// Package metaads is the Meta Ads integration: every operation of Meta's
// Marketing API v27.0 an ads integration reaches, classed by the coverage
// matrix its reference page publishes, reached by the engine for the agents as
// one system user of the company's own Meta app (ADR-0170), or as an agent's
// own system user where it keeps one (ADR-0179). Organic Pages and
// Instagram publishing, WhatsApp, Commerce and Rights Manager are other
// products, left out ([specgen] lists each by its domain).
//
// # Tokens
//
// The company pastes three tokens of one Employee system user: a read token
// holding no permission that writes, an operate token (ads_management) for
// changes, and optionally an admin token (business_management) used only for an
// account change an administrator allowed (and optionally its Admin system
// user's, which makes agents' own: "Seat identities"). Each call goes through the least
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
// # Seat identities
//
// A seat set to `identity: own` signs its calls as its own Employee system
// user of the company's business, so Meta's activity history names it
// (ADR-0179). Crewlet makes it with the business's Admin system user's token
// (admin_system_user_token, used for nothing else): named by the shared slug
// (crewlet-<handle>), given the seat's ad accounts with its level's tasks
// (View performance; Manage campaigns from Operate; Manage ad account at
// Administer), the app installed, and one sixty-day token minted with the
// level's permissions within what the company's tokens prove the app holds.
// Without that token a person makes it in Business settings and hands in its
// token. Either is checked before it is kept and on every pass: the company's
// app (which Meta installs only on its own business's system users), an
// Employee, never an Admin, permissions matching the level (business_management
// only at Administer), and every one of the seat's ad accounts at its tasks.
// The token is sealed at META_ADS_TOKEN_<HANDLE> and reaches no box.
//
// The seat's own token signs every call the seat makes, with no slot choice:
// it holds only its level's permissions and reaches only its ad accounts. The
// engine's own reads that hold a call to its policy (an object's account, a
// target's state, an account's capacity) stay the company's: Meta records no
// read, and they must see past the seat's reach to refuse well. A seat whose
// own identity Meta refused is refused every call, never the company's token
// in its place; one that keeps none yet, or whose business holds Meta's
// system user cap (10 at Full access, 1 at Limited, error 3949), calls as the
// company's system user and reads Needs you.
//
// Each pass, before the check, a made system user is brought back to the
// seat's level and ad accounts if either changed, and a token within ten days
// of its end is renewed (minted anew, or exchanged where no admin token is
// held); the old token is revoked once the new one is sealed, and a former
// system user's token Crewlet could not revoke stays sealed, revoked again
// each pass until Meta refuses it. Turning the seat off or back to the
// company's revokes its tokens and takes its ad accounts; a token Crewlet
// cannot revoke keeps the identity until it can. The system user stays, since
// Meta deletes none, and is reused if it returns.
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
