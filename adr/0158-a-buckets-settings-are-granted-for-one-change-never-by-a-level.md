# ADR-0158: A bucket's settings are granted for one change, never by a level

- **Status:** accepted
- **Authority:** `internal/scaleway`
- **Enforced-by:**
  - `internal/scaleway.TestTheNeverGrantedListIsPinned`
  - `internal/scaleway.TestABucketsSettingsAreGrantedForOneChange`
  - `internal/scaleway.TestABucketGrantRidesThePolicyForItsHoursAndLeaves`
  - `internal/scaleway.TestTheScalewayPageStatesWhatIsNeverGranted`
  - `internal/seataccess.TestAGrantForOneChangeIsNeverPermanentNorPastItsBound`
  - `internal/agent/builtin.TestAOneChangePermissionIsAskedForItsTargetAlone`
  - `internal/capability.TestASeatIsToldTheGrantsItsCredentialHolds`
- **Cost-when-tried:** an agent found two buckets whose object listing was public and could not close them, since no level or grant held `PutBucketAcl`; the person it asked to run the call by hand saw it fail silently, and the buckets stayed public.
- **Tag-status:** unreleased

## The decision

**ObjectStorageBucketsWrite leaves Scaleway's never-granted list as a grant
for one change.** No level holds it. A seat holds it only as a grant a person
approves (ADR-0150), for at most four hours and one by default, never for
good, asked for with the bucket it is for (`target`), and the person is told
what else the set allows. The rest of Object Storage stays never granted:
object contents (ObjectStorageFullAccess, ObjectStorageReadOnly,
ObjectStorageObjectsRead, ObjectStorageObjectsWrite) and bucket policies
(ObjectStorageBucketPolicyFullAccess).

"For one change" is a mark on the vendor's catalogue entry
(`seataccess.GrantEntry.MaxHours`, with `Note` for what else it allows), read
the same way by a save (`CheckGrants`, and `CheckGrant` on every pass),
`request_access` and its decision, the Agents tab and "Your access". The grant
keeps the request's target, and `seat_grant_changed` names it.

## Why the obvious alternatives are wrong

**Keeping it never granted.** Only `PutBucketAcl` closes a public listing, so
an agent that finds one hands the change back to a person, which is the work
the agent holds the key for.

**Granting it like any other set, or in a level.** It is all of a bucket's
settings: lifecycle rules that expire objects, and ACLs that let anyone write
into a bucket. Held for good, or by every agent on a level, it is a standing
path to the object contents the never-granted list keeps closed. Hours bound
it, and one approval per change puts a person's eyes on each use.

**ObjectStorageBucketPolicyFullAccess instead.** A bucket policy can hand any
principal, or everyone, the bucket's objects, and shuts out every principal
it does not name. It is wider than the ACL, not narrower.

## What this does not decide

What the grant was used for: Scaleway's Audit Trail does not cover Object
Storage and buckets keep no access log, so the trail records the grant, never
its calls. One bucket as a grant's reach: Scaleway scopes a set to projects,
so the target is a record, not a bound. Which other sets are granted for one
change: a vendor marks one in its catalogue on this record's reasoning.
