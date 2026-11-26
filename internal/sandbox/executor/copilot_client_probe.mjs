// The stand-in of the Copilot SDK's main module (`@microsoft/copilot-sdk`) the
// suite installs beside copilot_steer_probe.mjs: the second client the notes
// extension opens to delete a launch attempt Copilot refused. It holds the
// extension to the contract Copilot 1.0.114 kept (measured): a client is made
// with mode "coding-agent" and `_internalConnection: {kind: "parent-process"}`,
// which start() refuses otherwise as 1.0.114 does (row R19g); its rpc answers
// only once started ("Client is not connected", row R17a); and
// `rpc.sessions.bulkDelete({sessionIds})` deletes those sessions (rows R17d,
// R19w). Reports, one JSON object a line on stdout: `client` (the options),
// `bulkDelete` (its arguments).

const report = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")

export class CopilotClient {
  constructor(options) {
    this.options = options
    this.started = false
    report({ op: "client", input: options ?? null })
    this.rpc = {
      sessions: {
        bulkDelete: async (args) => {
          if (!this.started) throw new Error("Client is not connected")
          report({ op: "bulkDelete", input: args ?? null })
          return { freedBytes: Object.fromEntries((args?.sessionIds ?? []).map((id) => [id, 0])) }
        },
      },
    }
  }

  async start() {
    const connection = this.options?._internalConnection
    if (this.options?.mode !== "coding-agent" || connection?.kind !== "parent-process") {
      throw new Error(
        "Could not resolve @microsoft/copilot-sdk-linux-x64. Reinstall @microsoft/copilot-sdk so its platform package is installed.",
      )
    }
    this.started = true
  }
}
