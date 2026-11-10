# Operating insights

An opted-in engine can share how its company's agents work, as small typed weekly records, so the project learns which patterns are common across companies. It is separate from [engine issue reports](engine-issue-reports.md): that consent covers the engine's own defects and never usage, and this one covers usage patterns alone ([ADR-0119](../../adr/0119-operating-insights-are-typed-weekly-records-under-a-consent-of-their-own.md)).

## Turning it on

It is an administrator's setting, off by default:

```yaml
diagnostics:
  insights:
    share: review   # off (the default), review or auto
    accepted: 1     # the catalogue schema you accepted
    always: []      # classes sent without review, in review mode
```

- **review** keeps each eligible record in Settings › Diagnostics beside the advisor's evidence for it, until an administrator sends it or declines it (`POST /diagnostics/insights/decisions`). A class listed in `always` skips the wait.
- **auto** sends every eligible record on its own.
- **off** means off: every record that waits is forgotten, on every node, and nothing is kept or sent until it is on again, across restarts.

`accepted` must name the schema of the catalogue this build carries. A build whose catalogue is newer shares nothing, every class alike and not only the ones that changed, until an administrator accepts it again.

In the dashboard, **Settings › Diagnostics › Usage insights to Crewlet** writes the same setting: **Don't share**, **Review each record first** or **Send automatically**, and choosing to share accepts the catalogue this build carries. When the catalogue is newer than the one accepted, the page says nothing is shared and offers to accept it. In review mode, **Waiting for your review** lists each record with **Send**, **Don't send** and **Always send this pattern**, which adds the class to `always` and sends that record. **Usage insights kept** lists every record with its status, and each opens its exact bytes; a sample record of every class shows what one would send.

## What is sent

One record per class per ISO week, for the patterns the [advisor](advisor.md) finds in the company's own records. A class is eligible once its finding holds on two days in a row of one week, and each record leaves at its own random time within a day of being sent. A record is the class, the ISO week, the release tag (or `main` for any other public build) and at most four facts, each a bucket of a share or a count:

```json
{"schema":1,"class":"wake.noop_share","week":"2030-W02","version":"v1.4.0","facts":{"seats":"1","share":"35-49"}}
```

The full catalogue, with every class, fact and bucket, is [`schema/insights-catalogue.json`](../../schema/insights-catalogue.json), and `GET /diagnostics/insights` answers it with every record the company keeps.

## What is never sent

A name of a role, seat, person, unit, server or host; an item key or title; any text; a raw count or duration; the commit; an instant finer than the ISO week; anything about the topology or the configuration. The record type has no field that could hold one, and a development build never sends.

## Where it goes

Records are posted to `https://reports.crewlet.ai/v1/insights`, a constant the company document cannot change, and only from a public build: a development build's server sends nothing, even a record another server scheduled. Each record keeps the relay's last answer (its status code), and a send that never settled reads as failed after an hour. The relay keeps no per-record row and no client address: it counts records per week, class, fact tuple and version, and reads any count under 3 as "under 3".
