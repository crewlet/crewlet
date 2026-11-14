# ADR-0145: A permission the atlas could use is a suggestion, never a refusal, and an optional part nothing was read from is no finding

- **Status:** accepted
- **Authority:** `internal/atlas/collect`
- **Enforced-by:** `internal/atlas/collect.TestAnOptionalPartNothingWasReadFromIsNoFinding`, `internal/cloudflare.TestTheAtlasReadsWorkersPagesBucketsAndTunnelsWhereTheTokenMay`, `internal/cloudflare.TestATokenWithoutTheOptionalPermissionsIsWholeAndSuggested`, `internal/cloudflare.TestAnOptionalPermissionTakenAwayKeepsWhatWasReadAndSaysSo`, `internal/cloudflare.TestAnOptionalListingIsReadWholeOrNotAtAll`, `internal/cloudflare.TestNoCredentialShapeReachesTheAtlasFromCloudflare`, `internal/cloudflare.TestTheProvisionerStepsListTheBuildersNames`
- **Cost-when-tried:** not tried here, and seen next door. Cloudflare's connect check refuses a token that lacks any group in its provisioner list, so adding a group the atlas would like to that list would have refused, on the next check, every token that manages its agents correctly. ADR-0130's refused part is the other half: it marks a source partly read, which for a permission no company was ever asked for would mark every company's Cloudflare source so, from the first read.
- **Tag-status:** unreleased

## The decision

**A permission only the atlas would use is optional, and its absence is said as a suggestion.** Some of what a vendor holds sits behind a permission the company's credential was never asked to carry: at Cloudflare, the read groups for Workers scripts, Pages, R2 and Tunnels on the provisioner token. Such a permission is in no list a credential check requires. A credential without it works, is said to work, and is told in one line what it could add to see more in the atlas. The line is a fact beside the verdict of the connect check and one advisory on a ready integration from the pass, both generated from one table. Nothing an integration does for its agents waits on it.

**No agent is ever granted it.** The optional groups are a table of their own, apart from the levels. A level grants what its own list names, so a provisioner that holds an optional group mints no token with it.

**An optional part is tried, and a refusal of it is carried like any other.** The read asks each optional listing and reads what answers. A `403` leaves that part unread, with the suggestion as its detail, and every claim an earlier read recorded from it stands (ADR-0130). Any other failure fails the read, as for every listing.

**The standing names an optional part only where the voice holds an entry from it.** A part marked optional is a finding on the source's standing only while the log holds a live claim that an entry of that part exists, in the source's voice. That is a permission somebody took away: what was read is kept and the standing says what to add back. Where the voice holds no such entry, nothing was ever read from the part, nothing was lost, and the source reads as whole. Only an entry counts: a fact one part shares with another (what an account is used for) would name a part nothing was read from.

**The optional reads decode a name.** Each of the four listings yields a name or an id: a script, a project and a tunnel are workloads that run on the account, and a bucket belongs to it. A Pages project's deployment configuration, where its environment variables are, is never decoded; a script's content, settings and secrets, a tunnel's token and configuration, and an object are never asked for.

## Why the obvious alternative is wrong

**Adding the groups to the provisioner's own list** makes the connect check refuse, and the pass report a short grant on, a token that does everything the integration exists for. It would also hand the groups to the level tables' reasoning, where "the token holds everything an agent can be given" is the promise.

**Reporting a never-held permission as a refused part** marks the source partly read for every company on its first read, for a permission nobody asked them for, and trains a reader to ignore the mark that means something was lost.

**Reading the token's own policy first and skipping what it lacks** costs the calls it saves, and trusts a policy read the token may be refused, where the listing's own answer is the truth.

**Dropping an optional part's entries when its permission goes** retires what a company's atlas recorded the day somebody narrows a token, which ADR-0130 refuses for every other part.

**A setting that switches the optional reads on** asks a person to state in the engine what the token at the vendor already states, and the two would disagree.

## What this does not decide

Which vendors have optional permissions: each source marks its own parts. Whether a vendor offers the atlas an identity of its own instead, which is that vendor's decision record. How a suggestion is drawn on the dashboard's atlas screens, which read the standing as it is. That each optional read group covers more than the listing made with it (a script's code, a project's plain environment, a bucket's objects): the engine calls none of those, and the integration's page says so.
