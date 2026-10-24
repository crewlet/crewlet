# ADR-0048: The adopted brand is the company's public face, snapshotted by an administrator and served by its bytes

- **Status:** accepted
- **Authority:** `internal/artifactsdomain`
- **Enforced-by:** `internal/artifactsdomain.TestTheAdoptedBrandOutlivesItsKitAndIgnoresItsDrafts`, `internal/artifactsdomain.TestAnUnreadableBrandIsNeverAdopted`, `internal/artifactsdomain.TestAnAdoptionNamesOnlyTheFreshCopiesItsBrandServes`, `internal/artifactsdomain.TestASupersededAdoptionIsServedWhileAVersionPinsIt`, `internal/artifactsdomain.TestASupersededAdoptionGoesWithTheLastVersionThatPinsIt`, `internal/artifactsdomain.TestAFramedVersionPinsTheStylesheetAdoptedWhenItIsPublished`, `internal/artifactupload.TestAnAdoptionCopiesEveryFileAndGeneratesTheStylesheet`, `internal/artifactserve.TestTheBrandRouteServesOnlyAdoptedBytes`, `internal/artifactserve.TestTheArtifactListenerServesOnlyTheServingRoutes`, `internal/artifacts/brandformat.TestNoValueLeavesItsDeclaration`, `internal/artifacts/svgsafe.TestNothingThatRunsLoadsOrExpandsIsStored`, `internal/api/artifactsapi.TestTheAdoptedBrandIsEverybodysAndADraftIsNot`, `internal/api/artifactsapi.TestAdoptingIsAnAdministratorsAndRecordsTheOverride`
- **Cost-when-tried:** the first design put the brand in one artifact at a fixed id whose editors published what every page showed. A purge of that artifact leaves a deletion marker that refuses its id for ever, so the company could never have a brand again; and any editor of it, an agent included, restyled every page the company publishes with no administrator involved.
- **Tag-status:** unreleased

## The decision

A brand **kit** is an ordinary artifact of the native kind `brand`, held to a
closed format (`internal/artifacts/brandformat`), which anybody with the
artifacts app may draft. The company's **brand** is an **adoption**: one record
on one subject of the artifacts log (`brand`, no id), written only by an
administrator who is a person, at a recent proof of identity
(`artifacts.brand.adopt`, `artifacts.brand.clear`), naming one version of one
kit. Four rules follow, and they bind the domain, the upload pipeline, the API,
the artifact origin and the agent tools, which is why they are a record:

1. **An adoption snapshots, never points.** It records the version's canonical
   `brand.json`, a stylesheet generated from it once, and a fresh copy of every
   logo and face it serves, each an object the adoption's own record uploaded
   under one reservation. A later publish to the kit, its trash, its retention
   and its purge change nothing a page is served until somebody adopts again;
   the apply refuses only a version gone by then.
2. **What is adopted is everybody's.** Every person, every page, every agent
   under every requester ceiling (`get_company_brand`) and the artifact origin's
   public route read it. A draft kit is not the public face: whether a newer
   version exists is told only to a reader who may open the kit.
3. **It is served by its bytes, from an allowlist.** The artifact listener
   serves `/_crewlet/brand/<sha256>.<ext>` for a hash an adoption names, as
   that file's own type, public and immutable for a year, keyed with the host;
   anything else is an empty `404`, and the object store is never asked by
   hash. The API origin serves the same files as every artifact file
   (ADR-0035's second rule).
4. **A framed page pins, a drawn kind follows, a deck copies.** A framed
   version records the stylesheet adopted when it is published, and the origin
   links it at the page's opening when it serves the version, leaving the
   author's bytes as they are; a page that says `data-crewlet-brand="off"` on
   its `<html>` element is served without it. An adoption's files stay served
   while it is current or a retained version pins it. A doc and a dashboard
   follow the adopted brand live, and a deck copies the brand into its own
   files when it is created.

The stylesheet defines only the artifact kit's `--brand-*` input slots on
`:root` and an `@font-face` per uploaded face, re-checking every value it
writes; an SVG logo is stored as `internal/artifacts/svgsafe` re-serialises it;
the brand's voice reaches a model only through the tool, fenced as
company-adopted text naming both who wrote it (the adopted version's author)
and who adopted it. The adoption also labels the version it adopts where the
label is free, as provenance: the snapshot never depends on it.

## Why the obvious alternatives are wrong

**A record beside the charter, in the company document**, is where an
administrator's company-wide setting usually lives. Logos and faces are bytes
no signed config record carries, no reference table names a config revision's
objects, a managed document refuses every person's write (ADR-0030), and the
charter's write path is being rebuilt on another branch.

**An adoption that points at the kit's version** instead of copying it is
cheaper by a few megabytes and breaks three ways: retention retires the version
after a hundred newer drafts, the kit's owner can trash it, an administrator can
purge it. Naming the version's own objects instead of copying them would keep
the bytes alive but make the adoption the first record that names an object
another artifact's row names, outside the one carry ADR-0036 admits.

**Live restyling of framed pages**, the stylesheet always the current one,
restyles a page nobody reviewed under the new brand: an author who wrote white
text on the accent has unreadable text the day the accent turns pale, and no
version records who changed what the page shows. The pin is a column the
retention of the brand's files reads, so it costs one indexed row per version.

**Serving any object by its hash** would make the public route an oracle for
every private file whose digest somebody learnt.

## What this does not decide

It does not decide **off-brand linting**, several brands per company, a
re-theme of an existing deck, or a "use the current brand" action on an old
page; each is a later change on top of these rules. It does not decide how the
dashboard draws a doc or a dashboard in the brand's tokens, which is the
dashboard's.

## Amendments to earlier records

[ADR-0035](0035-an-artifact-is-served-from-an-origin-that-holds-no-credential.md):
the artifact listener's route list gains the adopted brand's files
(`GET /_crewlet/brand/{file}`), held by the same enforcing test, and the origin
places one more element at a page's opening, built from a hash alone.
