# ADR-0179: A seat may hold its own identity at a growth vendor; the company's is the fallback, never a silent one

- **Status:** accepted
- **Authority:** `internal/seataccess`
- **Enforced-by:**
  - `internal/seataccess.TestTheResolverNeverFallsBackSilently`
  - `internal/seataccess.TestTheRowNeedsYouUntilTheSeatsOwnIdentityWorks`
  - `internal/seataccess.TestASeatIdentityIsCheckedBeforeItIsKeptAndOnEveryPass`
  - `internal/seatname.TestEveryNameKeepsItsVendorsRule`
- **Cost-when-tried:** not tried. Every growth vendor that offers a seat its own identity would record one actor, the company, for every agent's change, and revoking one agent at the vendor would mean revoking every agent.
- **Tag-status:** unreleased

## The decision

**One setting, one record, one resolver, for every lane.** A growth seat block whose vendor offers a seat its own identity takes `identity: own | company` (the lane's default where unset; writing it is an administrator's, as a level is). The ledger keeps, per seat and lane, the identity the seat holds (the vendor's ID, its name, how it was made, where its credential is sealed), what the last check read back, the last attempt that was not kept, and an identity a replacement superseded. Each growth product asks `seataccess.ResolveSeat` who signs a call: the seat's own where it keeps one that works, the company's where the seat is set to it or keeps none yet, and a refusal (`*seataccess.IdentityRefusal`) where the vendor refused the seat's own. **Never the company's in its place**: otherwise revoking one agent's key at the vendor would quietly hand it the company's.

**Checked before it is kept, and on every pass.** An identity a person hands in, adds at the product, or asks Crewlet to make is checked first (alive, the company's team, the role its level needs, the reach its scope needs) and kept only when ready; one that fails is kept nowhere and the agent's box says why. Every pass reads each kept identity back; a refusal there stops the seat's calls until a person gives it a working one or sets it back to the company's. A seat turned off or back to the company's gives its identity up: the vendor undoes what Crewlet can undo, the sealed credential is cleared, and what only a person can revoke is asked of them until the vendor refuses it.

**The row needs a person until the seat's own works.** On a lane whose vendor offers a seat its own identity, the company's identity is never In sync: the row reads Needs you until the seat's own is verified, and Refused where the vendor refused it. Only a vendor that offers none (Apple Ads, AdSense, AdMob) reads In sync on the company's.

**One name.** Every name Crewlet suggests or creates for a seat's identity comes from `seatname.For`: `crewlet-<handle>`, lowercase letters, digits and hyphens (underscores only where a vendor refuses hyphens), held to each vendor's characters and length, hashed where the vendor's names are shared beyond one company.

**Enforcement stays Crewlet's.** The vendor role is a ceiling under Crewlet's floor: every call is still held to the seat's level, scope, operation class and the company's spend caps (ADR-0170), and an approved act is sent as the seat's own identity (ADR-0171). This amends ADR-0170's "the vendors cannot give one identity per agent": they can, and Crewlet keeps enforcing the scope a vendor's identity cannot narrow. It answers ADR-0178's open per-seat question for every Google product but AdSense and AdMob, whose per-seat identity needs a Google Account per agent.

## Why the obvious alternatives are wrong

**Falling back to the company's identity when a seat's own is refused** would turn a revoked agent into one acting as the company, the opposite of what the revoke asked for.

**One identity mechanism per vendor** would word, check and resolve each one its own way, so the Agents tab and Ask Crewlet would describe an agent's identity differently per vendor, and a fix to one check would miss the others.

**Keeping an identity first and checking later** would let an Admin key or another team's key sign calls until the next pass.

## What this does not decide

Which vendors offer a seat its own identity, and how each makes and checks one: each vendor's lane says so in its `seataccess.IdentitySpec` and implements `seataccess.IdentityMaker`.
