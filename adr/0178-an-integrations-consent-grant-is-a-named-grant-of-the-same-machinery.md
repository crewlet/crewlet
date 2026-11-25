# ADR-0178: An integration's consent grant is a named grant of the same machinery

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:**
  - `internal/engine.TestAConsentSignInAsksForItsScopesOfflineAndSealsTheGrant`
  - `internal/engine.TestAConsentWithoutARefreshTokenOrWithAnotherScopeIsRefused`
  - `internal/engine.TestAConsentRenewsOnceOnA401AndKeepsAReturnedRefreshToken`
  - `internal/engine.TestAnInvalidGrantAsksGoogleNoMoreUntilASignIn`
  - `internal/engine.TestSigningInAgainRevokesNothingAndKeepsNoOldToken`
  - `internal/engine.TestADisconnectRevokesOnlyWhenNoOtherGrantSharesTheProject`
  - `internal/engine.TestAConsentNeedsAPublicAddressAndItsIntegrationOn`
  - `internal/engine.TestAConsentGrantGoesOnlyWithTheClientItWasMadeAs`
  - `internal/engine.TestAnUnreadGrantIsNotOneNobodyHolds`
  - `internal/api/setupapi.TestAConsentGestureIsRefusedACallerWithoutSecretsWrite`
  - `internal/fleetsecrets.TestAnOAuthGrantIsUnreachableFromTheOperatorsView`
  - `internal/googleauth.TestAClientsProjectIsItsIDsNumericPrefix`
  - `internal/googleauth.TestAGrantIsKeptOnlyAsExactlyTheScopesAsked`
  - `internal/googleauth.TestARefusalAboutAWeekAfterTheSignInReadsAsAnAppInTesting`
  - `internal/api/configapi.TestADisconnectGivesUpTheIntegrationsConsentGrant`
- **Cost-when-tried:** not tried. AdSense and AdMob refuse service accounts, so the company reaches them only through a person's consent; without an engine-held grant, a person's token would have to be pasted, unrenewed, or put in an agent's box.
- **Tag-status:** unreleased

## The decision

**One grant implementation, many families.** ADR-0075's sealing, renewal lease, signed sign-in state, callback, revocation and refusal record serve a *named* grant of a family (`coord.GrantFamily`): an MCP server's (`mcp`) and an integration's consent grant (`integration`). Each family keeps its own records: its sealed row `<owner>/<name>/refresh` in `fleetsecrets.Estate`, its public half, its sign-in record and its renewal lease, so an MCP server and an integration of one name never share a grant, and MCP's classes (`mcp_oauth`, `mcp_sign_in`, `mcp_oauth_renewal`) are unchanged.

**An integration's grant is `integration/<kind>`.** It is sealed at `integration/<kind>/refresh` under the estate owner `integration`, which no `${VAR}`, listing, reveal or snapshot reaches. Its public half (`integration_grant`) holds who signed in on Crewlet and when, the scopes granted, the accounts its product last reached and the vendor's redacted words for the renewal it last refused; its sign-in (`integration_sign_in`) holds the sealed PKCE `S256` verifier; one node renews at a time under `integration_grant_renewal`.

**The company's own client, its scopes, offline.** A consent product (`googleauth.Consent`) names the scopes its seats' levels need now (reading alone, or changes too) and reads the company's own OAuth client from its block (`oauth.client_id`, and `oauth.client_secret` as a `${VAR}` that several consent blocks may share). An administrator at a fresh step-up starts the sign-in (a `consent` setup requirement); the engine sends the browser to Google's authorization endpoint with `access_type=offline`, `prompt=consent`, `include_granted_scopes=false` and exactly those scopes, so granular consent can never grant part of them, and no `resource`. It keeps the grant only if the exchange carries a refresh token and grants exactly those scopes. A company that raises a seat to change settings signs in again for the wider set. Any other grant is refused and kept nowhere, and revoked at Google only where that ends no live grant (this product's own, or another's with a client in the same project); otherwise the person is told to remove Crewlet at myaccount.google.com. Google sends no device sign-in for these scopes, so a company without `api.external_url` cannot sign in, and is told why.

**Renewal and refusal are ADR-0075's.** A refresh token Google returns replaces the sealed one at the version read; a 401 is one renewal and one retry; `invalid_grant` is recorded on the grant, reported as `credential_rejected`, and no node asks Google again until somebody signs in. A refusal six and a half to seven and a half days after the sign-in adds that the OAuth app is most likely in Testing.

**Signing in again never revokes; giving up revokes only alone.** Google's revocation ends every grant the person made to the client's Cloud project, so revoking the replaced token would end the one just sealed: the new token replaces it and the old is held nowhere. A sign-out or a disconnect deletes the sealed row, the record and any sign-in, and revokes at Google only when no other live consent grant of the company has a client in the same project (the client ID's numeric prefix); otherwise it says how to end the access at Google.

## Why the obvious alternatives are wrong

**A copy of the MCP grant code for integrations** would be two implementations of the same security rules, which drift: a fix to one renewal path would leave the other spending refresh tokens twice.

**Reusing MCP's classes with an `integration/` server name** would put two families in one record set, so an MCP listing, alarm or removal would read integration grants, and a rolling upgrade would hand integration grants to nodes that do not know them.

**Revoking on every sign-out** would end a second product's grant whenever it shares the client's Cloud project, which Google documents and a person cannot see.

## What this does not decide

Per-seat consent grants, a vendor other than Google, and which products take a consent grant: each product's own change registers it.
