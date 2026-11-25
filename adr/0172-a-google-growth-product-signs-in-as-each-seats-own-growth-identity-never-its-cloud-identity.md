# ADR-0172: A Google growth product signs in as each seat's own growth identity, never its cloud identity

- **Status:** accepted
- **Authority:** `internal/googleauth`
- **Enforced-by:**
  - `internal/googleauth.TestTheExchangeIsCloudPlatformAndOnlyTheImpersonationCarriesProductScopes`
  - `internal/googleauth.TestAnIssuedTokenIsKeyedByItsScopeSet`
  - `internal/googleauth.TestEveryIdentityIDFitsGooglesRuleAndNeverCollides`
  - `internal/googlecloud.TestASeatsGrowthIdentityIsItsOwnAccountRecordedByUniqueID`
  - `internal/googlecloud.TestAGrowthIdentityFollowsTheOROfTheSeatsLanes`
  - `internal/googlecloud.TestAGrowthIdentityIsNeverAdoptedNorReenabledUnasked`
  - `internal/googlecloud.TestARecordedGrowthIdentityIsNotAnOrphan`
  - `internal/googlecloud.TestAnAccountMadeByAPassCutShortIsRecordedNeverOrphaned`
  - `internal/googlecloud.TestATurnOffCutShortIsFinishedNotReportedAsAPersons`
  - `internal/googlecloud.TestTheCloudShellBlockMakesTheCompanysOwnReaderForItsGrowthProducts`
  - `internal/engine.TestAGrowthTokenIsMintedOnlyForAUsableRecordedIdentityWithItsScopes`
  - `internal/engine.TestGoogleCloudsLaneNeverCountsTowardTheGrowthIdentity`
- **Cost-when-tried:** not tried. Google's growth products (Analytics, Search Console, Ads, Play) accept a service account added as a user, but the account a seat holds for Google Cloud puts its `cloud-platform` token in the agent's box (ADR-0102).
- **Tag-status:** unreleased

## The decision

**Every Google token comes from one package.** `internal/googleauth` owns the company's Google identity reached by federation, impersonation, the token caches and the one call path to Google. A token is keyed by the account and the sorted scopes it carries; the STS exchange is always `cloud-platform`, and only the impersonation names a product's scopes.

**Each seat has a second account for the growth products, its growth identity.** `crewlet-g-<handle>-<hash>` in the identity project, hashed over `growth/<company>/<handle>`, so it never collides with the seat's Google Cloud account. Its tokens are minted inside the engine, with the product's own scopes, and never leave it: no growth token is a `${VAR}` or reaches a box. A person adds its email to each product at the role the seat may act with; the vendor's role is the ceiling and the engine's scope and classes the floor.

**The identity's owner is the core, not a lane.** Its record is a coordination class of its own (`google_identity`, keyed by handle), holding Google's unique ID, because the seat access ledger is keyed by one integration and a lane's sync would disable an account another lane still uses. Only the Google identity pass writes it, with Google Cloud's company pass, before every Google growth lane's pass: on while any growth lane of the seat is on, disabled when the last turns off or the seat is removed, and kept, so its roles in the products survive should the seat return. The record is written before each step at Google it names (the intent to make an account by its email, then its unique ID; a disable before Google is asked), so a pass cut short leaves an account Crewlet knows is its own. It is never adopted by name; one a person turned off at Google is never turned back on unasked. The Google Cloud lane counts for nothing toward it, so turning Google Cloud off for a seat leaves its growth identity working.

**The company reads its figures as its own reader, one per company.** `crewlet-gr-<company>-<hash>`, hashed over `reader/<company>`, made by the Cloud Shell block when a growth product is on, which lets only this company's federated subject act as it. Two companies may share an identity project, so a reader with a fixed name would let one company read the other's spend and earnings. The engine impersonates it with the federated token directly.

## Why the obvious alternatives are wrong

**One account per seat for both** would rest "no growth token in a box" on Google refusing a `cloud-platform` token at each product's API, which no page states, while the seat's box holds that token.

**A ledger row per growth lane for the same account** would let one lane's off disable the account the others still use.

**A refresh token per seat from a person's consent** would hold a long-lived secret per seat where an hour-long impersonated token serves; consent is kept for the products that refuse service accounts (AdSense, AdMob).

## What this does not decide

The products' own levels, tools and decisions, each in its package and the growth framework; the consent grants of the products that refuse service accounts.
