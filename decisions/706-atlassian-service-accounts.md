# d-706 — Atlassian mints after all, on Cloud, through an unscoped key

Status: **decided, implemented.**
Implementation: `internal/atlassian`, `cmd/crewlet/atlassian.go`,
`integrations.atlassian` · Corrects a claim
[`d-703`](703-unserved-integrations.md) and two package docs made in good
faith · Cited from `internal/atlassian`'s package doc and from
`internal/jira/reconcile.go`.

## The claim this reverses

`internal/jira/reconcile.go` opened with a refusal, and `cmd/crewlet/jira.go`
repeated it:

> Jira cannot: a Cloud API token is issued by the person it belongs to at
> Atlassian's own account site, and a Data Center personal access token can
> only be minted for the calling user. A run that pretended otherwise would
> print instructions dressed as actions.

It was written about a **user** account, and about a user account it is
exactly right. It was then applied to the whole vendor, and the consequence
shipped in four more places: the example config told founders there was
NOTHING TO PROVISION, `choosing-your-stack.md` put Atlassian in the by-hand
column, the authoring skill made it an invariant, and every Crewlet company
created one Atlassian account per agent by hand and pasted one token per
agent into a variable.

A **service account** is a different object with a different API. With an
organization API key, Atlassian's `account-management` admin service creates
one, mints its API token, and grants it a product licence. So the by-hand
step was never necessary on Cloud, and the refusal cost every operator the
same afternoon.

## What is true, precisely

| | Cloud | Data Center |
|---|---|---|
| Create an account for an agent | **Yes** — a service account, through the organization admin API | No. There is no organization admin API |
| Mint that account's credential | **Yes** — the admin console's own token route accepts an organization key | No. A PAT can only be minted for the calling user |
| Give it a product licence | **Yes** — the service-account invite route | n/a |
| Place it in a project or a space | **No.** Refused to an API token; a Forge app on a paid plan is the only thing that may | No |

So `crewlet atlassian provision` is Cloud-only and says so by name, and
`crewlet jira provision` keeps the Data Center duties — the seat-identity
report, the project/lead agreement check, and the inbound webhook.

## Three routes, and two of them are not in the reference

- `POST /admin/account-management/v1/orgs/{org}/service-accounts` — creates
  the account. Atlassian's published reference documents service accounts
  under a **different service**, `api-access`, which serves only `/count`
  and answers the collection with *"Request failed to match any route"*.
  `account-management` is what the admin console itself calls.
- `POST /users/{atlassianId}/manage/api-tokens` — mints the credential.
  Atlassian's reference documents its token endpoints as **read and revoke
  only**. This is the route the console mints through, and it accepts an
  organization key.
- `POST .../service-accounts/invite` — grants the product licence. It is the
  only route that works: a service account is not a directory user, so group
  membership answers `USER_NOT_FOUND`, and the role-assignment endpoints are
  read-only.

Two of those three were read off the admin console's own traffic. **That is
a real risk and it is taken deliberately**, because the alternative is asking
an operator to create and paste one credential per agent, for ever. What it
costs is honesty: every refusal is a NAMED error rather than a bare status,
the docs page says plainly that these are console-derived, and a withdrawal
shows up as a refusal an operator can read rather than a half-provisioned
company. The httptest fake pins the exact request shape of all three, so a
change in what Crewlet SENDS fails in CI even though a change in what
Atlassian ACCEPTS cannot.

Three more vendor behaviours are load-bearing and none of them is guessable:

- **The organization key must be created WITHOUT SCOPES.** `account-management`
  refuses a scoped key with 403 whatever scopes it holds — the same answer as
  no permission at all. It is the wall every first run hits, so both the 403
  and the 404-on-a-read map to one sentinel whose message names the fix.
- **The product ARI is `jira-software`, not `jira`.** The plain one is
  ACCEPTED and grants nothing: a run that reports success and agents that can
  do nothing, with no error anywhere.
- **The mint's expiry field is `expiry`, not `expiresAt`.** The name
  Atlassian uses elsewhere fails with `INVALID_EXPIRY`, which reads as though
  no expiry had been sent at all.

## The reconcile holds no state

The obvious design is the one a SaaS control plane needs: a row per agent
carrying the account id, the licence set, the token's scopes and when the
access was last verified, so a poll costs nothing. This has none of that, and
it is the same call [`d-201`](201-coordination-contract.md) makes about what
belongs in a shared store: a remembered claim is the one thing that can
DISAGREE with the vendor, and the disagreement is the expensive failure.

Every stored field has a vendor-side equivalent:

| A row would remember | This asks |
|---|---|
| the account id | the organization's own listing, joined on the display name and on what the recorded credential says it is |
| the credential | `TokenSink.Value` — the same three-valued read GitLab's reconcile makes |
| the licence set | whether the agent's own credential can call that product |
| a propagation deadline | whether THIS run granted the licence |

The scope set is the interesting one, because it cannot be read back at all —
`ListTokens` returns an id and a label and nothing else. So drift is detected
**behaviourally**: the credential is exercised against every product the seat
is enabled for, and a seat that has gained Confluence has a Jira-only token
whose Confluence identity check is refused, so it is re-minted with the
scopes the seat has now. That tests the thing rather than a memory of it.

What it costs is one organization listing plus one identity call per enabled
product per seat, once, on a command an operator typed. What it buys is a
provisioner that needs no database, works on a stopped node, and cannot be
wrong about what the vendor holds.

## The block names the organization, not a third site

`integrations.jira` and `integrations.confluence` each name a SITE. The
organization key, the organization id and the account-naming prefix are
organization-level and cover both products at once, which is why they are a
third block rather than a `provisioning:` sub-block on each of the other two.

Two sub-blocks would duplicate `org_id` and invent a disagreement between
them that does not exist today. A single merged `atlassian:` block replacing
both would be a rename across a dozen consumers, and old stored revisions
would keep decoding while old YAML files stopped parsing.

So the new block carries **only** what is genuinely the organization's, and
the cloud id a licence is granted on still comes from the product's own
block — which is also how an organization running the two products on two
sites works correctly rather than by accident.

**There is no `api_key` field**, and that is the rule
[`d-203`](203-secrets-follow-the-config-path.md) already states: the
organization key is the OPERATOR's credential, not the company's. It carries
the right to create billable accounts across the whole organization, and the
secret store is replicated to every node holding the keyring. It arrives from
`-admin-token`, then `$ATLASSIAN_ORG_API_KEY`, then `$ATLASSIAN_ADMIN_TOKEN`
— the environment alone, exactly as GitLab's and Mattermost's admin tokens do.

## Placement is reported, never faked

Adding an account to a Jira project role or a Confluence space permission is
refused to an API token. The run therefore grants the org-level **licence**
and then reads back, as the agent, what access it actually ended up with —
reporting **missing** (the operator's to grant, with the exact settings URL
and whether the project is team- or company-managed), **excess** (a forbidden
permission the site's own scheme attached, which Crewlet did not grant and
cannot revoke), and **pending** (a licence granted this run; Atlassian
applies one asynchronously and has taken minutes).

The rejected alternative was a reserved field and a TODO for the Forge route.
A field the engine never reads is exactly the silence
[`d-703`](703-unserved-integrations.md)'s surviving rule exists to end.

The Confluence half of that read has one trap worth recording: Confluence has
no `mypermissions`, so the space's own grant list is read and filtered to the
agent — and the filter has to cover grants made to a GROUP the agent belongs
to, because almost every real grant is. Matching only the account id reports
an agent that works perfectly as having no access at all, and the operator's
next move is to grant permissions it already had.

## What it cost to find out

Two things, and neither was in scope when this started.

**The seat-credential grammar existed twice.** `internal/jira/provision.go`
exported it with a comment saying a second copy is how the two come to look
under different keys — and `internal/engine/confluence.go` held that second
copy, already drifted: no `Authorization` key, no `Bearer ` strip, and it
chose the `mcp_env` block by NAME ORDER rather than by which block holds a
token. A provisioner would have been the third. It is now one grammar in
`internal/atlassian`, which both products and the provisioner read.

**Confluence seat identities were never registered.** `confluence.Backend`
appeared in no production `Register` call, so the wiki's party namespace was
permanently empty for agent seats: a page mentioning an agent resolved to
nobody, [`d-704`](704-confluence-page-subscriptions.md)'s subscription ledger
was never written, an agent was never suppressed as the actor of its own
edit, and every page event fell through to the space lead. The parser was
correct the whole time; it was asking a registry nothing had ever written to.

The fix is cheap precisely because one account id serves both products — but
it is resolved PER PRODUCT anyway, because on Data Center the two identity
endpoints answer different things (Jira's `name`, Confluence's `userKey`) and
registering one under the other's namespace is the misroute
`internal/engine/atlassian.go` exists to prevent.

## What this does not decide

Whether Crewlet should ship a Forge app of its own. That would make placement
possible and would make Cloud webhooks Crewlet's rather than the operator's —
and it is a distribution and review commitment, not an engineering one.
`integrations.forge_app_id` already carries the identity such an app would
verify against.
