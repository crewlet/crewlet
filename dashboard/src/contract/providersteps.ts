/**
 * The terminal steps that give a company its first model, as the builder
 * shows them: on the card after a create, and under the caution a company
 * with no `providers.llm` raises. No dashboard screen adds a model, and every
 * model runs through a coding CLI that is signed in on the node's host, so
 * this is the one part of starting a company the dashboard hands back.
 *
 * THEY START FROM THE STORE, because that is where a company the builder made
 * is: `PUT /config` wrote it, and no file holds it. `crewlet llm` reads the
 * entry it signs in from a company FILE, and `crewlet config import` writes a
 * file back, so the first step writes the running company to one. That step
 * is `crewlet config export` and not `GET /config?format=yaml`: the read is
 * redacted, and `crewlet llm` refuses a company file still carrying a mask
 * (`config.LoadCompanyToRun` with no revision to restore one from, since the
 * command writes none) — the import would restore it, but the sign-in comes
 * first — while the export is the stored document as it stands. The node is stopped
 * first because the export reads the node's store, which one process holds at
 * a time — `crewlet llm` itself runs beside a running node, reading the stored
 * values through it — and the import then takes the offline route, and the
 * node publishes the revision when it starts again.
 *
 * `-capture-token` stores a headless token in the secret store under the CLI's
 * own token variable, which an entry naming no `auth.token` reads, so the
 * entry needs nothing but its model. With the node stopped it reaches the
 * fleet's store through the node's own broker — except on a member of a
 * cluster, which cannot reach it alone and names a running member instead.
 *
 * HELD by `cmd/crewlet.TestTheFirstModelStepsRunOnACompanyOnlyTheStoreHolds`,
 * which walks these lines in order from the state a create leaves — the store
 * holding the company, no company file beside `crewlet.yaml` — running every
 * `crewlet config` command and loading what every `crewlet llm` command loads.
 * A step that reads a file no earlier step wrote fails it.
 */
export const PROVIDER_STEPS = `# Stop crewlet run first (Ctrl+C), and run these beside its
# crewlet.yaml: the export reads the company out of the store,
# which a running node holds.

# 1. Write the running company to a file
crewlet config export > company.yaml

# 2. Add the model to it, and the sandbox every seat's executor
#    runs in: here this machine, as your user
providers:
  llm:
    default:
      model: sonnet
  sandbox:
    local: {}
    default_run_in: direct

# 3. Sign its CLI in to your subscription, and check it end to end
#    (on a member of a cluster, add -api with a running member's URL
#    and a Tier A token in CREWLET_API_TOKEN: a member alone cannot
#    reach the fleet's store)
crewlet llm login default -capture-token
crewlet llm doctor default

# 4. Import the file, and start the node again
crewlet config import company.yaml
crewlet run`;
