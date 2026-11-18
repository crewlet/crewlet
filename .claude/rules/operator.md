---
paths:
  - "{schema,docs,cmd/crewlet}/**"
  - "internal/{api,config,coord,engine,estate,eventfan,executorkeep,hostbox,jira,jsapi,jsinflight,jsprovision,mcp,objstore,observe,org,presence,queue,sandbox,seataccess,statelog,store,confluence,atlassianhost,artifacts,envref}/**"
  - "internal/providers/llm/harness/**"
  - "{Dockerfile,.goreleaser.yaml,Makefile,.github/workflows/main-image.yml}"
  - ".claude/worktrees/*/{schema,docs,cmd/crewlet}/**"
  - ".claude/worktrees/*/internal/{api,config,coord,engine,estate,eventfan,executorkeep,hostbox,jira,jsapi,jsinflight,jsprovision,mcp,objstore,observe,org,presence,queue,sandbox,seataccess,statelog,store,confluence,atlassianhost,artifacts,envref}/**"
  - ".claude/worktrees/*/internal/providers/llm/harness/**"
  - ".claude/worktrees/*/{Dockerfile,.goreleaser.yaml,Makefile,.github/workflows/main-image.yml}"
---

# crewlet-operator — Update It in the Same Change
`crewlet-operator` runs Crewlet companies from `crewlet.ai` custom resources, consuming this engine's contract: it renders both config tiers, calls the API, copies a few run-time decisions out of the source and runs the image. Why a change it has not followed turns its next run red (its ADR-0002), and what your summary says either way, is in `CLAUDE.md`. The name always means that repository, never a person or `internal/api/operator`.

What needs one — anything it renders, calls, copies, runs or links:
- **A document it renders** — a field, enum value, default, rule or refusal of Tier B or Tier A, and so of `schema/`
- **A credential position** — a field tagged `secret:"true"`, each mapped to a reference field on one of its kinds (a test holds the two lists to each other)
- **A route or an answer it calls** — `/health`, `/ready`, `/fleet`, `/agents`, `/config`, `/secrets` and the pause tools of `/operator/mcp`: path, method, status, error envelope, the fields it reads, the paths a validation error names, who may call it
- **A decision it copies** — the engine functions its `internal/render/mirror_test.go` fingerprints by file and name, and the harness profiles (`harnessprofile`)
- **How the engine runs in a pod** — `crewlet run -config`, the `CREWLET_*` variables, ports, probes, the drain and its grace, and where the engine writes: it runs non-root on a read-only root filesystem, with only a data volume, `/tmp` and the home directory writable
- **The image and the tag** — `Dockerfile`, `.goreleaser.yaml`, `make image-context`, `main-image.yml` (its path and name: its release verifies the signature against them), and any `v*` tag
- **A page or heading it links** — `https://github.com/crewlet/crewlet/blob/main/docs/<page>.md#<heading>`; its docs gate fails when one goes

Rules:
- **Look in its tree before you commit, not in your memory** — grep it for what you change and for the engine file you edit. No hit means nothing is needed
- **Make the change there; a note is not the change** — on a branch of it, by its own conventions (its `CLAUDE.md`, `make check`, `git commit -s`). If it cannot be reached, say exactly what it needs in your summary and the pull request
- **Prove it against this change, not `main`** — `CREWLET_VERSION` for a commit, `CREWLET_IMG` for an image built with `make image-context` (its `CONTRIBUTING.md`, "The engine suite"). Say what you could not run
- **Deliver on this change's terms** — push and open a pull request there only when you do here, each naming the other; land the pair back to back, this one first
- **Follow `main` plainly** — its API is `v1alpha1`: the companion change is a rename or reshape with no alias, both-spellings reader or engine-version switch, unless it has shipped a tag carrying the surface

`CONTRIBUTING.md` ("Changes `crewlet-operator` has to follow") states the same rule for contributors; change both together.
