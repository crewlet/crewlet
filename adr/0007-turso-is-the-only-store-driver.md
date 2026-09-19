# ADR-0007 — Turso is the only store driver, and it bounds the release matrix

- **Status:** accepted
- **Authority:** `internal/store`
- **Enforced-by:** `internal/store.TestTursoIsTheOnlyDriverInTheBinary`, and `internal/store/platform.go`, which turns an unsupported build into a compile error
- **Measured:** the embedded database engine is a ~20 MB native library, extracted at run time into a per-user cache. Upstream embeds it for linux and darwin on amd64/arm64 and windows/amd64, and for nothing else.
- **Cost-when-tried:** the second driver was `modernc.org/sqlite`, kept as an escape hatch behind `store.driver` and `CREWLET_STORE_DRIVER`. It has no vector distance functions, so an operator who flipped the variable kept every table and lost their agents' recall, with nothing saying so. Building the linux artefact `-static` to avoid the dynamic link segfaults on its first query — measured, and `ci.yml`'s `cross` job asserts the artefact stays dynamic.
- **Tag-status:** unreleased

## The decision

One driver: `turso.tech/database/tursogo`. There is no dialect intersection to
write inside, so the engine uses what Turso has. Both the `store.driver` field
and the `CREWLET_STORE_DRIVER` variable are retired, and a Tier A file that
still sets the field is refused with a message naming the change rather than
reported as a misspelling.

The consequences reach past `internal/store`, which is why this is a record
rather than a package doc:

- **The release matrix is the driver's matrix.** linux and darwin on amd64 and
  arm64. There is no Windows target, because the release once published
  windows/amd64 and windows/arm64 and the second had no library at all — it
  started, then failed at its first query.
- **The linux binary is not static, despite `CGO_ENABLED=0`.** purego declares
  its `dlopen` imports with `//go:cgo_import_dynamic`, so the artefact is
  dynamically linked against glibc and does not run on musl or `scratch`. A
  static program has no dynamic loader and cannot `dlopen` at all.
- **The engine ships its own lexical search.** Turso has no fts5 and no `fts`
  index method, so `internal/textindex` is the analyzer and the BM25 arithmetic
  the knowledge search is built on. The alternative was refusing knowledge
  search on the only driver this build ships, or embedding a search library
  with its own index format, file and backup story on every node.

## Why the obvious alternative is wrong

The obvious alternative is to keep a second certified driver as an escape
hatch. The escape hatch turned out not to be one: the two drivers were not
interchangeable for a database with rows in it, and the difference was silent.
A user who took the hatch kept a working binary, a complete schema and an
agent whose memory returned nothing.

"Pure Go" is not "self-contained", and conflating them is how the Windows and
musl targets shipped. `-tags musl` picks the embedded `.so`, not the binary's
own linkage, so it does not produce an Alpine build either.

## What this does not decide

It does not decide the file layout: the node keeps two of them, which is
[ADR-0004](0004-a-node-keeps-two-database-files.md).

Nothing about the data moves. Turso is SQLite-compatible in its file format,
so an existing store opens untouched and any SQLite-compatible client still
reads the file for forensics.
