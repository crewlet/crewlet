# Frozen insights catalogues

One file per insight record schema: `<schema>.json`, a verbatim copy of
`../insights-catalogue.json` as that schema was first consented to. The relay
vendors each one and counts a record against the schema it names, and
`TestAPublishedClassShapeIsImmutable` (in `internal/insights`) holds the live
catalogue to the frozen copy of its own schema, so a class cannot change its facts
or buckets without a schema bump, which turns the changed classes off until an
administrator accepts them again. A bump adds its own file here.
