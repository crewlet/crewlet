# ADR-0129: An integration's read records what changed and retires what two reads missed

- **Status:** accepted
- **Authority:** `internal/atlas/collect`
- **Enforced-by:** `internal/atlas/collect.TestAnUnchangedReadRecordsNothing`, `internal/atlas/collect.TestAChangedEntryIsRecordedWholeAndAtOnce`, `internal/atlas/collect.TestRetirementTakesTwoCompleteMissesADayApart`, `internal/atlas/collect.TestARetiredEntryIsNeitherMissedNorRetiredAgain`, `internal/atlas/collect.TestAFailedReadRecordsAndRetiresNothing`, `internal/atlas/collect.TestASourceIsReadWhenDueAndAnUnchangedReadWritesNothing`, `internal/atlas/collect.TestNoCredentialShapeReachesARead`, `internal/atlasdomain.TestAReadRecordsWhatChangedAndWithdrawsTheRestOnEveryNode`, `internal/atlasdomain.TestAReadWithdrawsOnlyItsOwnVoice`, `internal/atlasdomain.TestOnlyTheEngineRecordsAnIntegrationsRead`, `internal/atlasdomain.TestAReadsRetirementIsTheEntrysDenialAndALaterReadRestoresIt`, `internal/atlas.TestAnIntegrationDeniesOnlyTheEntryItself`, `internal/cloudflare.TestTheAtlasReadsEveryNameAndCertificate`, `internal/cloudflare.TestNoCredentialShapeReachesTheAtlasFromCloudflare`, `internal/cloudflare.TestAPartialReadFailsWhole`, `internal/cloudflare.TestOnePassListsTheZonesOnce`, `internal/redact/redacttest.TestEveryShapeIsAKnownCredential`
- **Cost-when-tried:** not tried. The nearest earlier shape is the integration reconcile loop, whose contract forbids a write on a converged pass (`internal/integration`, certified by `integrationtest`): reading inventory inside it would have made every unchanged pass either a write or a lie about what it read.
- **Tag-status:** unreleased

## The decision

**An integration's read of the atlas is its own fleet duty**, `atlas-collect`, claimed per tick like the reconcile loop and re-claimed before every source, on the company's `atlas.refresh_interval_seconds` (an hour by default, never under five minutes). It reads each connected integration's vendor whole, under the access the company's own credential already holds, and is never told what changed. The reconcile loop's cadence is tuned to catching access somebody revoked, its failures are findings about seats, and its contract forbids a write on a converged pass; an inventory read has a different cost, a different failure meaning and a different cadence, so it runs apart, and where the two read one listing (Cloudflare's zones) the duty repeats one cheap call an hour.

**A read records only what changed.** The duty reads every live claim the atlas log holds in the integration's voice, diffs the read against it, and publishes one `read` record per entry whose claims moved: the claims on it that are new or changed, and the slots of the voice's earlier claims the read no longer holds, which the log withdraws into the entry's history. An unchanged read writes nothing, so an hourly read of thousands of names costs the log nothing an hour. An entry the read lists is recorded whole and at once: a slot the read no longer holds on it is a change the vendor reported, never a skipped row.

**Retirement takes two complete reads a day apart.** A complete read that does not list an entry the voice holds is one miss, remembered in the source's standing with the instant of that read. The entry is retired, with one denied exists claim and every other slot of the voice withdrawn, only by a later complete read at least a day after the first miss that misses it too. A read that failed or was cut short is no read: it records nothing, retires nothing and forgets no miss. A read that lists the entry again clears its miss, and a read that lists an entry it retired restores it with one record. An integration denies nothing but an entry's own existence; a person's claim on the same slot outranks it, and the disagreement is shown.

**Each source's standing is the fleet's, apart from the integration statuses.** The duty keeps per source, in the coordination store, its last attempt and whether it completed, its last complete read and that read's digest, the calls and withheld values it cost, what it recorded, and the entries it has stopped seeing since when. The whole company needs the current value and only the current value: a node taking the duty over reads when each source was last read whole, and the dashboard reads the same row wherever it serves. It is a bucket of its own because the integration statuses' reader forgets every key the document does not declare as a surface.

**Every value a source reports is typed, and a credential has nowhere to go.** A source maps a vendor's answer into the vocabulary's grammars through one builder, which refuses a value or a key shaped like a credential and counts what it refused by where it sat, never by what it was. A source reads no field nobody mapped: Cloudflare's TXT records, comments and tags are never decoded. A corpus of every shape the redaction pass knows, planted in every field a vendor's answer carries, reaches no read.

## Why the obvious alternative is wrong

**Reading inside the reconcile loop** costs no extra call for the lists both read, and it breaks the loop's contract that a converged pass writes nothing, ties inventory to a cadence chosen for revoked access, and mixes two failure meanings on one status card. One repeated call an hour is cheaper than that.

**Recording every read**, as a presence record per entry, is what makes a vendor's listing the atlas's freshness. At an hourly cadence a few thousand DNS records would be a few thousand replicated writes an hour on every data node for nothing, and the log would hold a day of identical rows for every fact.

**Retiring on one miss** removes what a vendor's offset-paginated listing skipped while the list changed under the read, and what a vendor's own view lagged on for an hour. Two complete misses a day apart make a removal a fact the vendor repeated.

**Retiring on a failed or partial read** reads an outage at the vendor as a deletion at the company. A read that did not finish is no read.

## What this does not decide

Which vendors beyond Cloudflare are read, and what each reads, which each source decides with its own tests. Whether a source reads more under an identity made for the atlas, which a later record decides. How a source's standing is shown, which the dashboard decides.
