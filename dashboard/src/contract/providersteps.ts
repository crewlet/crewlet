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
 * first — while the export is the stored document as it stands. The node is
 * stopped first because the export reads the node's store, which one process
 * holds at a time, and the import then takes the offline route, and the node
 * publishes the revision when it starts again.
 *
 * THE SIGN-IN RUNS ON THE STOPPED NODE AND THE CHECK ON THE RUNNING ONE. The
 * builder's node has run, so its own secret rows are not the company's whole
 * store: the fleet's is the broker's, which only a running engine serves. A
 * login is let through there because what it writes is new — `-capture-token`
 * stores a headless token in this node's own rows under the CLI's own token
 * variable, which an entry naming no `auth.token` reads, so the entry needs
 * nothing but its model, and the node carries the row onto the fleet's store
 * as it starts. `crewlet llm doctor` reads the company's stored values, and
 * every command that does is refused on a stopped node that has run, where
 * its rows would answer for a store they are not. So the check is the last
 * step, beside the running node, reading the values through it — the token
 * the login stored among them — which is what the engine runs on. Every value
 * it reads that way is a reveal, which takes admin and a person present, so
 * `CREWLET_API_TOKEN` holds one of `crewlet.yaml`'s `api.auth.tokens`, one
 * that carries `admin`: a machine token is refused one.
 *
 * HELD by `cmd/crewlet.TestTheFirstModelStepsRunOnACompanyOnlyTheStoreHolds`,
 * which walks these lines in order from the state a create leaves — the store
 * holding the company, no company file beside `crewlet.yaml`, a node that has
 * run — running every `crewlet config` command, loading what every `crewlet
 * llm` command loads through the store its run reaches, and standing a node
 * up at `crewlet run` that holds what the stopped node's rows held. A step
 * that reads a file no earlier step wrote, or that the stopped node refuses,
 * fails it.
 */
export const PROVIDER_STEPS = `# Stop crewlet run first (Ctrl+C), and run these beside its
# crewlet.yaml: the export reads the company out of the store,
# which a running node holds.

# 1. Write the running company to a file
crewlet config export > company.yaml

# 2. Add the model to it
providers:
  llm:
    default:
      model: sonnet

# 3. Sign its CLI in to your subscription
crewlet llm login default -capture-token

# 4. Import the file, and start the node again
crewlet config import company.yaml
crewlet run

# 5. In a second terminal, with CREWLET_API_TOKEN set to one of
# crewlet.yaml's api.auth tokens, check it end to end through
# the running node
crewlet llm doctor default`;
