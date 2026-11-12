# ADR-0130: A read refused a part keeps it, and a voice denies only what it said exists

- **Status:** accepted
- **Authority:** `internal/atlas/collect`
- **Enforced-by:** `internal/atlas/collect.TestARefusedPartIsCarriedAndRetiresNothing`, `internal/atlas/collect.TestAVoiceDeniesOnlyWhatItSaidExists`, `internal/atlas.TestAMonitorsStateIsASignalOnWhatItWatches`, `internal/datadog.TestTheAtlasReadsMonitorsServicesHostsAndChecks`, `internal/datadog.TestAnAlertingMonitorIsTheHealthOfWhatItWatches`, `internal/datadog.TestARefusedListingIsAFindingAndRetiresNothing`, `internal/datadog.TestADeletedMonitorRetiresNothingItWatched`, `internal/datadog.TestNoCredentialShapeReachesTheAtlasFromDatadog`, `internal/datadog.TestAListingThatFailsFailsTheRead`, `internal/datadog.TestADatadogCheckWarnsWhatTheAtlasCannotRead`
- **Cost-when-tried:** not tried. ADR-0129's one source, Cloudflare, reads under a token whose every Read group the credential check already requires, and claims every entry it names, so neither question arose until a vendor with a permission per listing, whose entries point at other sources' entries, was read.
- **Tag-status:** unreleased

## The decision

**A source reads a vendor in parts, and a part the vendor refuses is a finding, never a fault.** A vendor puts each listing behind a permission of its own, and the credential a company gave the engine for another purpose may hold some and not others. A read reports each listing it was refused beside its claims, with what to grant and which of the voice's claims that listing made. The read is still complete: the duty carries every such claim exactly as the log holds it, so a refusal withdraws nothing, counts no miss, advances no miss an entry already had, and retires nothing, however long it lasts. The source's standing names the refused parts, and a later read that is whole clears them. Every other failure of a listing still fails the read whole, as ADR-0129 says.

**A voice denies only what it said exists.** A source claims an entry only where the entry lives at its vendor, and otherwise names it: a monitor lives at Datadog, and the service, the server and the address it watches do not. An entry the voice only related or described is released by the two complete misses a day apart that would retire it: its slots are withdrawn, and no denial is recorded. The fold lets an integration's newer denial stand against another integration's older claim, so a denial built for an entry the voice never claimed would retire a name the DNS provider still lists the day somebody deletes a monitor.

**A monitor's state is a fact, and the health of what it watches is read off it.** A monitor's state and the instant it began are claims in the integration's voice, recorded when they change like every other. The signal a state raises, on the monitor and on every entry a `monitored_by` relation joins to it, is computed when the atlas is read and stored nowhere, so it cannot outlive the state, the monitor or the relation.

**Datadog is read by exact keys.** A monitor watches the service its `service:` tags name and the host its `host:` tags name, and a running Synthetic test's monitor watches the address the test requests. A monitor's query is never read: it is the vendor's own grammar, with negations and wildcards a reader would have to interpret, and an interpreted scope is a guessed relation.

## Why the obvious alternative is wrong

**Failing the read on a refused listing** makes one missing permission an outage of the whole source: a company whose key reads monitors and not hosts would see neither, and the first lasting refusal would leave every entry of the source unconfirmed for good.

**Reading what is permitted and diffing as usual** treats the refusal as an empty listing. Every entry an earlier read of that listing recorded is missed, then retired a day later, because somebody changed a role.

**Requiring every permission at the credential check** refuses a key pair that provisions agents' identities correctly because it cannot read a catalogue the atlas would like. The atlas is optional reading; the integration's own work is not.

**Claiming every entry a source names** is simpler and is what lets one source retire another's entry. An integration's claim that an entry exists is also a promise to deny it when it stops being listed.

**Copying a monitor's state onto the entries it watches**, as a fact on each, multiplies every state change by the entries watched and leaves a stale alert on an entry whose relation was withdrawn.

## What this does not decide

Whether a source reads more under an identity made for the atlas. Whether an alert delivery brings a monitor's read forward, which the event-driven sources decide. How a refused part is shown, which the dashboard decides.
