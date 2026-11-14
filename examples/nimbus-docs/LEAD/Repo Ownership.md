# Repo Ownership

This page is the source of truth for which agent owns which repo. Every
repo is on Gitlet, the company's own git host. Gitlet's grants follow the
org chart rather than this table: every engineer in Core may write every
Core repo, and the CTO, Core's lead, maintains them. Who owns which one is
this page's convention, and each owner's brief names its repos. Work in a
repo you don't own goes on your own branch (`seats/<your handle>/…`) as a
change its owner reviews.

## Ownership matrix

| Repo | Owner | Co-owners / reviewers | What's in scope |
|---|---|---|---|
| `nimbuscore` | SWE | CTO (architecture review), AI Systems Engineer (endpoint design input for framework primitives) | The Go control-plane HTTP/REST service. Workspaces, clusters, nodes, regions, join tokens, kubeconfig delivery. Swagger spec lives here. |
| `nimbusk0s` | SWE | CTO | The k0s worker Docker image and its entrypoint. Nodes join clusters via this image. |
| `infra` | CTO | SWE | Deployment manifests and Terraform for Nimbus's own infra (cloud accounts, DNS, the hosted control plane). Operational, not a product surface. |
| `console` | Frontend SWE | PM (UX review), DevRel (screenshots for docs), CTO | The authenticated React management dashboard. AI Services pages (Models, Jobs, Datasets, Serving) land here in Phase 2. |
| `website` | Frontend SWE | PM (positioning), DevRel (launch posts), CTO | The public-facing React marketing site. |
| `nimbusflow` *(starts empty)* | AI Systems Engineer | CTO (architecture review), DevRel (docs + examples), SWE (control-plane integration) | Phase-2 Python framework. SDK, harness, runtime, scheduler, worker protocol, nimbuscore-side glue. |
| `docs` *(starts empty)* | DevRel | AI Systems Engineer (framework accuracy), SWE (API accuracy), Frontend SWE (console screenshots), PM (positioning) | The developer-facing docs site. Stack TBD via DevRel's ADR. |

## Cross-repo work — when does it happen?

| Story type | Repos touched | Coordination |
|---|---|---|
| Framework needs new nimbuscore endpoint | `nimbusflow` + `nimbuscore` | AI Sys Eng files an ADR + a SWE story; framework-side story is blocked until the endpoint ships. |
| Console gets new AI-services page | `console` + `nimbuscore` | Frontend SWE files a SWE story for any missing endpoint; UI story is blocked on it. Frontend SWE regenerates the TS client after the endpoint ships. |
| New framework feature → new docs | `nimbusflow` + `docs` | AI Sys Eng writes the ADR; DevRel drafts the docs page within 48 hours of ADR approval; AI Sys Eng technical-reviews before merge. |
| Launch blog post for a framework release | `docs` + `website` | DevRel writes the post in `docs`; Frontend SWE features it on the marketing site if it's a major release. |

## Anti-patterns

- **Crossing repos without a ticket.** If you're about to push to a repo
  you don't own, stop. File a story against the owner, or propose the
  change from your own branch and let the owner review it.
- **Working around a missing endpoint.** AI Sys Eng calling the cluster
  directly to bypass nimbuscore is forbidden — nimbuscore is the single
  source of truth for cluster operations.
- **Front-running the API contract.** Frontend SWE building a UI
  against a hypothetical endpoint, hoping it lands the same way. Wait
  for the endpoint to ship, regenerate the client, then build the UI.
- **Sneaking docs into engineering changes.** Docstrings and one-line
  example tweaks are fine in engineering changes; full doc pages go
  through DevRel in the docs repo.

## When a new repo is needed

1. File an ADR page with the proposed name, stack, owner, and
   one-line purpose.
2. CTO and CEO sign off (CEO for the public-facing implication; CTO for
   the technical fit).
3. The founder declares it under `code.repos` in the company's
   configuration — its owner, its description and a protection rule on
   `main` — which creates it on Gitlet at the next activation.
4. Update this `Repo Ownership` page in the same week, and the owner's
   brief.

## Before Phase-2 kickoff

- [ ] AI Systems Engineer: ADR for the framework's design, then the first
      commit to `nimbusflow`.
- [ ] DevRel: ADR for docs stack, then the first commit to `docs`.
