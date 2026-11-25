# ADR-0170: A growth integration's credential stays in the engine

- **Status:** accepted
- **Authority:** `internal/growth`
- **Enforced-by:**
  - `internal/config.TestAGrowthCredentialIsNamedAtItsOwnFieldAlone`
  - `internal/config.TestAVendorsOwnServerIsRefusedBesideItsProduct`
  - `internal/agent/builtin.TestAGrowthCallIsMadeOnlyAtTheLevelItsClassNeeds`
- **Tag-status:** unreleased

## The decision

**No credential of a marketing and growth integration (ad, analytics, app
store and publisher products) reaches an agent's box, a tool server or any
field but its own.** Agents reach those vendors through engine tools alone,
each built from its product's catalog (`growth.Catalog`), which hold every
call to the seat's lane level and scope, the class of the operation and, for
spend, the company's caps. The company document refuses the variable a growth
block's credential field names at any other position, found by walking the
whole document, and refuses a vendor's own MCP server for a product while its
block is on. This departs from ADR-0102 for these lanes alone.

## Why the obvious alternative is wrong

Handing an agent the vendor's token, as ADR-0102 hands a seat its own cloud
credential, would make every rule above advisory: a token in a box spends,
publishes and changes users without the engine seeing the call, and a vendor
role spans every account it was added to, so even a read token reads accounts
outside the seat's scope, and earnings are financial. The vendors cannot give
one identity per agent either (one App Store Connect team key reaches every
app; Meta allows one regular system user per business on its first tier).

## What this does not decide

Who answers a spend, public or admin act (ADR-0171), how a product signs in to
its vendor, or which accounts a seat may name; each product's package doc and
docs page say those.
