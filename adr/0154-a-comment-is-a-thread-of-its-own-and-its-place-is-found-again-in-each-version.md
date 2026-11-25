# ADR-0154: A comment is a thread of its own, and its place is found again in each version

- **Status:** accepted
- **Authority:** `internal/artifacts/commentanchor`
- **Enforced-by:**
  - `internal/artifacts/commentanchor.TestAPlaceIsFoundByItsContentNotItsPosition`
  - `internal/artifacts/commentanchor.TestAnAnchorThatIsGoneIsOutdatedNeverDropped`
  - `internal/artifacts/commentanchor.TestEveryKindAnswersWhereItsCommentsMayPoint`
  - `internal/artifactsdomain.TestACommentRidesItsThreadsSubjectNeverTheArtifacts`
  - `internal/artifactsdomain.TestAThreadKeepsTheAnchorAndVersionItWasMadeOn`
- **Cost-when-tried:** ADR-0042 fixed each kind's anchor and said a publish re-anchors a comment "by finding its own content again". Doing that AT PUBLISH means one write per open thread after every version, a window in which the head is published and its threads still point at the version before, and a retention sweep that deletes the version a thread was made on before anything moved it. Riding the artifact's subject, as a page's comments ride the page's, makes every remark contend with every publish.
- **Tag-status:** unreleased

## The decision

**A thread is its own subject** in the artifacts log (`comment`, id
`<artifact>.<thread>`): its opening remark, replies, edits, removals, resolve
and reopen contend only with each other, and the artifact's deletion gate
covers it as it covers an asset.

**A thread stores the anchor it was made with and the version it was made on,
and nothing else about where it is.** Its place on any version is FOUND, at
read, by looking for the anchor's content in that version's anchor index,
which `commentanchor` computes from the version's files and which is cached by
version because a version is immutable. A publish therefore re-anchors every
thread at once with no write; a thread whose content is not found is answered
`outdated` with the version it was made on, kept, listed and still answerable.

**What each kind's anchor is found by** is ADR-0042's table, with one
refinement: an `html` anchor names the page it is on (the HTML file's path in
the version), since a site of several pages holds a name or a point per page.
The page declares a name with `data-crewlet-anchor`, which
`crewlet.comments.anchor(name, element)` sets, so the frame and the index read
one declaration. This build offers `html`, `slides`, `design` and the whole
version; `doc`, `dashboard` and `sheet` keep ADR-0042's shapes reserved and
take whole-version comments, the anchor being a union keyed by its kind, so a
later kind is an added case rather than a changed record.

## Why the obvious alternatives are wrong

**Re-anchoring at publish** is the cost above: N writes, a stale window and a
dependence on the old version still being kept.

**Storing a pixel position** for every kind loses the thread at the first
reflow, which is why ADR-0042 anchors by content.

## What this does not decide

Who may comment (ADR-0034's ladder, at `commenter`), how a comment wakes its
agent (ADR-0012: derived from the record on the artifacts log's feed), and how
a page reports where its anchors are drawn (`docs/reference/artifact-runtime.md`).
