# Repo Ownership

This page is the source of truth for which agent owns which repo.
Per-role GitLab PATs are scoped to these allowlists at the token level
(the engine has no per-role repo-allowlist config field).

## Ownership matrix

| Repo | Owner | Co-owners / reviewers | What's in scope |
|---|---|---|---|
| `nimbus-hq/nimbuscore` | SWE | CTO (architecture review), AI Systems Engineer (endpoint design input for framework primitives) | The Go control-plane HTTP/REST service. Workspaces, clusters, nodes, regions, join tokens, kubeconfig delivery. Swagger spec lives here. |
| `nimbus-hq/nimbusk0s` | SWE | CTO | The k0s worker Docker image and its entrypoint. Nodes join clusters via this image. |
| `nimbus-hq/nimbuscode` | SWE | CTO | Terraform for deploying Nimbus's own infra (cloud accounts, DNS, the hosted control plane). Operational, not a product surface. |
| `nimbus-hq/console` | Frontend SWE | PM (UX review), DevRel (screenshots for docs), CTO | The authenticated React management dashboard. AI Services pages (Models, Jobs, Datasets, Serving) land here in Phase 2. |
| `nimbus-hq/website` | Frontend SWE | PM (positioning), DevRel (launch posts), CTO | The public-facing React marketing site. |
| `nimbus-hq/<framework>` *(repo name TBD)* | AI Systems Engineer | CTO (architecture review), DevRel (docs + examples), SWE (control-plane integration) | Phase-2 Python framework. SDK, CLI, runtime, scheduler, worker protocol, nimbuscore-side glue. |
| `nimbus-hq/docs` *(TBD)* | DevRel | AI Systems Engineer (framework accuracy), SWE (API accuracy), Frontend SWE (console screenshots), PM (positioning) | The developer-facing docs site. Stack TBD via DevRel's ADR. |

## Cross-repo work — when does it happen?

| Story type | Repos touched | Coordination |
|---|---|---|
| Framework needs new nimbuscore endpoint | `<framework>` + `nimbuscore` | AI Sys Eng files an ADR + a SWE story; framework-side story is blocked until the endpoint ships. |
| Console gets new AI-services page | `console` + `nimbuscore` | Frontend SWE files a SWE story for any missing endpoint; UI story is blocked on it. Frontend SWE regenerates the TS client after the endpoint ships. |
| New framework feature → new docs | `<framework>` + `docs` | AI Sys Eng writes the ADR; DevRel drafts the docs page within 48 hours of ADR approval; AI Sys Eng technical-reviews before merge. |
| Launch blog post for a framework release | `docs` + `website` | DevRel writes the post in `docs`; Frontend SWE features it on the marketing site if it's a major release. |

## Anti-patterns

- **Crossing repos without a ticket.** If you're about to `git push` to
  a repo you don't own, stop. File a story against the owner instead.
- **Working around a missing endpoint.** AI Sys Eng calling the cluster
  directly to bypass nimbuscore is forbidden — nimbuscore is the single
  source of truth for cluster operations.
- **Front-running the API contract.** Frontend SWE building a UI
  against a hypothetical endpoint, hoping it lands the same way. Wait
  for the endpoint to ship, regenerate the client, then build the UI.
- **Sneaking docs into engineering MRs.** Docstrings and one-line
  example tweaks are fine in engineering MRs; full doc pages go through
  DevRel in the docs repo.

## When a new repo is needed

1. File an ADR page with the proposed name, stack, owner, and
   one-line purpose.
2. CTO and CEO sign off (CEO for the public-facing implication; CTO for
   the technical fit).
3. Owner creates the repo under the `nimbus-hq` group with the standard
   MR template, branch protection, and CI scaffold.
4. Update this `Repo Ownership` page in the same week.
5. Update `examples/nimbus.company.yaml` if a role's GitLab token needs
   broadened scope.

## TODO before Phase-2 kickoff

- [ ] AI Systems Engineer: propose the framework repo name + create
      `nimbus-hq/<framework>`.
- [ ] DevRel: ADR for docs stack + create `nimbus-hq/docs`.
- [ ] Founder / org admin: rotate per-role GitLab PATs so each role's
      token is scoped to only the repos in this matrix.
