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
`crewlet/crewlet-operator` (public, MIT) is the Kubernetes operator that runs Crewlet companies from `crewlet.ai` custom resources. It CONSUMES this engine's contract — it renders both config tiers, calls the API, copies a few run-time decisions out of the source and runs the image — and its suites run this repository's `main` as `ghcr.io/crewlet/crewlet:main` publishes it, never a pin (its ADR-0002). An engine change it has not followed turns its next run red, so when a change here needs support there, that support is part of the change. (`crewlet-operator` always names that repository — never a person, and never `internal/api/operator`.)

What needs one — anything it renders, calls, copies, runs or links:
- **A document it renders** — a field, enum value, default, rule or refusal of Tier B or Tier A, and so of `schema/`
- **A credential position** — a field tagged `secret:"true"`, each mapped to a reference field on one of its kinds (a test holds the two lists to each other)
- **A route or an answer it calls** — `/health`, `/ready`, `/fleet`, `/agents`, `/config`, `/secrets` and the pause tools of `/operator/mcp`: the path, method, status, error envelope, the fields it reads, the paths a validation error names, who may call it
- **A decision it copies** — the engine functions its `internal/render/mirror_test.go` fingerprints by file and name, and the harness profiles (`harnessprofile`)
- **How the engine runs in a pod** — `crewlet run -config`, the `CREWLET_*` variables, the ports, the probes, the drain and its grace, and where the engine writes: it runs non-root on a read-only root filesystem with only a data volume, `/tmp` and the home directory writable
- **The image and the tag** — the `Dockerfile`, `.goreleaser.yaml`, `make image-context` and `main-image.yml` (its path and name, which its release verifies the signature against), and any `v*` tag
- **A page or heading it links** — `https://github.com/crewlet/crewlet/blob/main/docs/<page>.md#<heading>`; its docs gate fails when one goes

Rules:
- **Look before you commit, in its tree rather than your memory** — grep it for what you are changing and for the engine file you are editing. No hit is the answer
- **Make the change there; a note is not the change** — on a branch of it, by its own conventions (its `CLAUDE.md`, `make check`, `git commit -s`). If it cannot be reached, say exactly what it needs in your summary and the pull request
- **Prove it against this change, not against `main`** — `CREWLET_VERSION` for a commit, `CREWLET_IMG` for an image built with `make image-context` (its `CONTRIBUTING.md`, "The engine suite"). Say what you could not run
- **Deliver on this change's terms** — push and open a pull request there only when you do here, each naming the other, and land the pair back to back, this one first
- **Follow `main` plainly** — its API is `v1alpha1`: the companion change is a rename or a reshape, with no alias, both-spellings reader or engine-version switch, unless it has shipped a tag carrying the surface
- **Say what you did, or why nothing was needed** — the summary and the pull request description link the companion change, or say in one line why there is none (`crewlet-operator: none — internal to the turn loop`). Most changes need none and must not get one

`CONTRIBUTING.md` ("Changes `crewlet-operator` has to follow") states the same rule for contributors; change both together.
