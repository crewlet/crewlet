# ADR-0075: An MCP server's OAuth grant is the company's, sealed in the estate and renewed by one holder

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestASignInsStateIsSignedAndSpentOnce`, `internal/engine.TestTwoRenewersCannotStrandAToken`, `internal/engine.TestASignOutRevokesAtTheVendorAndDeletesTheGrant`, `internal/engine.TestARefusedRenewalAsksForASignInAndTheVendorNoMore`, `internal/engine.TestASignInComingBackFromAnotherIssuerIsRefused`, `internal/mcp/oauth.TestOnlyPKCES256IsEverSentAndAServerWithoutItIsRefused`, `internal/mcp/oauth.TestAnEndpointOnAnotherOriginIsReachedOnlyThroughTheGuard`, `internal/fleetsecrets.TestAnMCPGrantIsUnreachableFromTheOperatorsView`, `internal/mcp.TestARefusedTokenIsRenewedOnceAndTheRequestRetriedOnce`, `internal/engine.TestAGrantGoesOnlyToTheAddressItWasIssuedFor`, `internal/engine.TestAServiceIdentityIsIssuedItsTokenAtUseAndKeepsNothing`, `internal/engine.TestARefusedServiceIdentityIsHeldUntilItsWaitOrNewCredentials`, `internal/engine.TestAServiceIdentitysTokenEndpointIsDialledOnlyThroughTheGuard`, `internal/mcp/oauth.TestAServiceIdentitysSecretGoesOnlyByAMethodTheServerOffers`
- **Cost-when-tried:** a remote MCP server that signs in with OAuth (Linear, Notion, Sentry, Atlassian, Cloudflare, Stripe) could only be reached by pasting a personal access token into a header, where a vendor offers one at all: a token tied to one person, never renewed, and readable back from the secret store by any administrator.
- **Tag-status:** unreleased

## The decision

**One grant per server, the company's.** A server whose entry carries
`oauth` is signed in to once, by an administrator who stepped up, through the
authorization code grant with PKCE `S256` (and nothing weaker), the server
named as the resource (RFC 8707), the client registered by the engine's
published client metadata document, by dynamic registration, or as a client the
company registered itself. Every agent the server reaches uses that grant. A
per-agent server, a local server and a box's `sandbox.mcp.servers` cannot carry
one.

**The grant lives in three places, each for who has to agree on it.**

- The refresh token, and the client a registration issued, are SEALED in the
  secret store's engine namespace, `mcp/<server>/refresh`
  (`fleetsecrets.Estate`): a name no `${VAR}` reaches, which no operator
  surface lists, reveals, sets or deletes, and no `${VAR}` snapshot opens. It
  is rewritten only at the version a renewal read (`Estate.Swap`).
- The public half (who signed in, when, the issuer and client, the scopes,
  when it runs out, and the vendor's words for a renewal it refused for good)
  is a coordination record per server (`mcp_oauth`), which every node's answer
  and alarm read. No token is in it.
- The access token is issued at use and held in each node's memory alone,
  sent per request to the server's own origin (`mcp.Spec.Bearer`), never
  stored, logged, answered or audited.

**A grant goes with its resource, not its server's name.** It records the
resource it was issued for, and a node hands a server its grant only while the
server's configured address is that resource (`mcp.ResourceServes`); a
configuration write that moves the address holds the server back for a sign-in
at the new one.

**One renewer at a time, across the fleet.** A renewal spends the refresh
token, which a vendor rotates and treats as stolen if presented twice. So a
node renews while it holds the server's renewal lease (`mcp_oauth_renewal`),
reading the sealed grant inside it and writing the rotated token back at the
version it read. The lease keeps two nodes from spending one token; the
version keeps a renewer whose lease lapsed from writing over the one that took
over. A 401 to a token is one renewal and one retry, and a token issued within
the last thirty seconds is not renewed again. A refusal the vendor gives for
good is recorded on the grant, and no node asks the vendor again until
somebody signs in.

**A sign-in is a state the fleet signed, spent once.** The person's browser
carries the sign-in's id signed under the fleet keyring and nothing else; the
PKCE verifier, the client and the endpoints are sealed on the sign-in's
coordination record (`mcp_sign_in`), which lapses with it. The callback
(`GET /webhooks/mcp-oauth`) is unauthenticated and trusts only that signature;
it spends the sign-in by moving it on at the version it read, checks the
issuer the code came from where the authorization server names itself (RFC
9207), and refuses a grant without a refresh token, since nothing could renew
it on the nodes that run the server.

**Every address a vendor's metadata names is dialled through
`mcp.ServerNamedClient`:** on the server's own origin as the server's address
is dialled (the operator chose it, private or not, ADR-0072), and anywhere else
over https through the public-address guard, following no redirect, bounded in
time and size.

**Or a service identity, with nothing kept.** A server whose `oauth.grant` is
`client_credentials` is signed in to by nobody: the engine signs in as the
company's own client, with the id the configuration names and the secret its
`${VAR}` resolves to, whenever the server needs a token, through the same
token source a person's grant is sent by (one path to the server, renewed ahead
of its end and once on a 401). Each node is issued its own access token and
holds it in memory alone; there is no refresh token, no grant record, no
sealed row and no renewal lease, because nothing shared is spent: a node's
request is whole in itself. A failure is held for the restart schedule's wait,
so a refused credential costs the vendor one request a wait, not one a
request; a new id, secret or scope list is a new source, asked at once. The
secret is sent only to the token endpoint the server's metadata names, through
`mcp.ServerNamedClient`, by HTTP basic authentication where the authorization
server offers it and in the body only where that is all it offers, and never
where it offers neither; a vendor's refusal is quoted bounded and redacted,
with every value the request carried withheld.

**Giving the grant up revokes it.** A sign-out, and a removal of the server,
revoke the refresh token at the vendor where it publishes a revocation endpoint
(RFC 7009), delete the sealed row and the public record, and re-activate the
revision so every node stops the server.

## Why the obvious alternatives are wrong

**The grant as a `${VAR}` in the operator's store** would make the refresh
token readable back through `secrets.reveal`, and resolvable into any child
process an `mcp_env` names.

**Every node renewing on its own** spends one refresh token on several nodes at
once, and a vendor that rotates tokens revokes the whole grant the second time
it sees one.

**One node renewing and handing access tokens to the rest** would carry tokens
over the broker, which signs and does not encrypt (ADR-0031), or put them in a
coordination record any peer reads.

**A static access token written into the server's headers at start** runs out
within the hour, and renewing it would restart the server on every node.

**A lease for a service identity's tokens** would serialise requests that spend
nothing the fleet shares, adding a store round trip, and a failure whenever the
store blinks, to every token. **Sharing one service token across the fleet**
would carry it over the broker or keep it in a record, the same as handing a
person's grant's access tokens around.

## What this does not decide

Per-agent grants, the device authorization grant, enterprise-managed sign-in
(ID-JAG) and a service identity that proves itself by a signed assertion
(`private_key_jwt`) rather than a secret: each needs a control the page does
not have.
