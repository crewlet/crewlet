# ADR-0042: A kind is native or framed, and one table decides it

- **Status:** accepted
- **Authority:** `internal/artifacts`
- **Enforced-by:** `internal/artifacts.TestEveryKindAnswersEveryFacet`, `internal/artifacts.TestOnlyAFramedKindRunsAPage`, `internal/artifacts/manifest.TestEveryEntryRuleCanBeMet`, `internal/artifacts/manifest.TestAnEntryInfersExactlyOneKind`, `internal/artifactupload.TestADeckDeclaresNoCapability`, `internal/api/artifactsapi.TestTheDashboardDrawsEachKindAsTheKindTableSays`, and the dashboard's `KIND_VIEWS` and `KIND_WORDS`, records over every kind that fail the build when a kind is added and not drawn
- **Cost-when-tried:** before this record, what an artifact's kind decided was spelled in a switch on its name in eleven places (the manifest's entry check, the agent tools' default entry, the artifact origin's serving decision, the frame route, the source route, the runtime contract, the dark-mode warning, the search text, the CLI's inference, the dashboard's viewer and uilet's card). Adding the second native kind meant finding every one; a switch nobody found treated the new kind as whatever its default branch said, which for the serving decision is the difference between drawing a file and running it.
- **Tag-status:** unreleased

## The decision

**Every kind is native or framed, and one table says which, with every other
rule decided by kind.** The table is `internal/artifacts/kind.go`; each format
is its own package (`internal/artifacts/slideformat`,
`internal/artifacts/docindex`). A row of `artifacts.Kinds` answers, for one
kind:

- **render**: `framed` runs the version's own files as a page, only on the
  credential-less artifact origin ([ADR-0035](0035-an-artifact-is-served-from-an-origin-that-holds-no-credential.md)).
  `native` carries no author code: the dashboard draws it from a closed format
  with its own components, and the engine answers its content as data (a
  deck's canonical tree, a doc's tabs), never as markup to render;
- **entry**: the media types the version may open on anywhere, and the
  indexes told apart by their path (`deck.json`, `doc.json`), since an index
  is a file of a common type and is never inferred from JSON alone;
- **format**: the closed format a version's files are held to at publish,
  read whole, carried files included, and refused finding by finding; one
  package per format reads it on publish and on read;
- **store**: whether the datastore is declared by a page, is the content
  itself (a sheet), or does not exist;
- **search**: whether the gallery indexes the entry's text or what the format
  extracts from every file.

What follows from it, everywhere: a native kind declares no capabilities and
pins no runtime, because there is no page to grant them to; only a framed
kind is linted as a page, framed, ticketed or served on the artifact origin;
the agent tools list the kinds and their one-clause purposes from the table;
the CLI infers a kind from its entry by the table; and the generated contract
carries the table's public half to the dashboard and to uilet's `ArtifactKind`.

**A kind enters the table only in the change that makes it work end to end.**
A kind this build does not serve is refused at create, so no build publishes
a kind it cannot open.

**A kind is fixed at create.** No publish changes it: versions, compare,
covers and comments all assume one kind across an artifact's history, and
turning a doc into a deck is a new artifact made from its content.

**A gallery cover is drawn, never captured.** Each native kind's cover comes
from the version's outline (`GET /artifacts/{id}/versions/{n}/outline`), a
small answer cached by version, drawn by the same renderer the kind is read
with: a deck's cover slide, a doc's opening. A sheet's cover is its first rows,
read as the viewer. A framed kind keeps its glyph.

## Where a comment is anchored, by kind

Comments land later on the subject this build reserves
(`artifactsdomain.KindComment`). Their anchor is fixed here, now, because an
anchor shape added after a tag is a record format change. Every anchor names
the version it was made on, and a publish re-anchors it by finding its own
content again rather than by position alone.

| Kind | Anchor |
|---|---|
| `html` | A name the page declares (`crewlet.comments.anchor(name)`), else a point in the frame as fractions of its width and height, else the whole version |
| `doc` | The tab's path, the heading's id as the dashboard's renderer gives it (`h-` and its words), and the quoted text with its byte offset in the tab at that version |
| `slides` | The slide's id and the element's path of child indexes from its `<section>` (`0/2/1`), so a comment survives an edit elsewhere on the slide |
| `design` | The artboard's file and a point in the artboard's own pixels |
| `dashboard` | The widget's id and the datum: the dataset, the field and the row's key |
| `sheet` | The cell: the collection, the document and the field, the same triple presence already names |

## Why the obvious alternatives are wrong

**A switch per surface, kept honest by review**, is what was there. It had
already diverged: the CLI inferred a document from any `.md` while the
manifest admitted only the entry's type, and the frame route and the serving
decision each decided framing on their own.

**A type as an app**, a viewer bundle per kind served on the artifact origin
the way a hosted page type is, would run every kind as author-adjacent code,
lose covers without screenshots, and put present mode, compare, print and
comments inside a frame instead of the dashboard that already has them. A
closed format drawn natively needs no execution surface at all.

**Screenshot covers** need a headless browser rendering author pages on the
server, a new code-execution surface with network reach, and bake one
viewer's view of live data into an image every other viewer sees.

## What this does not decide

It does not decide **a format's contents**: each format's package is its own
authority (`docs/reference/slide-format.md`, the doc format in
`docs/guides/artifact-authoring.md`). It does not decide **who may read or
write** an artifact of any kind, which stays
[ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md)'s. It does
not decide **how a dashboard's live data is read** or **how a design's
artboards are served**, which their own records take when those kinds land.
