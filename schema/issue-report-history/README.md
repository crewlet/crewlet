# Frozen issue report catalogues

One file per report schema a reporting tag has published: `<schema>.json`, a
verbatim copy of `../issue-report-catalogue.json` at that tag. It is the record of
what an older release sends. The relay vendors each one and holds that release's
reports to it, and `TestAPublishedClassShapeIsImmutable` holds the live catalogue
to it, so a class this schema published cannot change shape under a release already
running. [`RELEASING.md`](../../RELEASING.md) has the step that writes it.
