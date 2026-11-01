/**
 * The tool catalogue: one row of `GET /tools` and the `tools` push, and of
 * the built-in groups `mcp_servers_status` sends.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS (`ToolRow`, which `CatalogueRow`
 * builds for both answers): held there by
 * `TestTheCatalogueIsWhatTheToolScreensRead` in both directions, the hint
 * words, the groups and the gates included.
 */

/**
 * One tool's behavioural hints, as the engine recorded them AT REGISTRATION.
 *
 * EVERY HINT IS A WORD, never a bool, and the third word is the point: "the
 * server did not advertise this" and "the server said no" are different facts,
 * and an absent hint arriving as `false` would read as a positive denial, so
 * a fresh MCP server's unannotated tools would look like proven reads on the
 * one screen an operator audits them on. All three values are truthy, so a
 * careless `if (!ann.read_only)` cannot mean "not read-only" either.
 *
 * A hint this build does not know renders as ITSELF, for the reason the event
 * registry gives: a rolling upgrade puts a newer node's value on an older
 * node's screen.
 */
export type ToolHint = "yes" | "no" | "unknown";

export interface ToolAnnotations {
  read_only: ToolHint;
  destructive: ToolHint;
  idempotent: ToolHint;
  open_world: ToolHint;
}

/** The shelf of the built-in catalogue a tool is filed on, in the order the engine lists them. */
type BuiltinGroupKey =
  "work" | "projects" | "knowledge" | "chat" | "artifacts" | "colleagues" | "memory" | "discovery";

/**
 * Who may call a built-in: what its authority row asks of the caller, named
 * by the engine and never decided here. Holding a tool is not being allowed
 * to call it. `everybody`: any agent. `self`: the agent, about its own
 * notes, turns and skills. `worklet`, `doclet`, `chatlet`, `artifacts`: an
 * agent that uses that app. `worklet_asked`: one that uses Worklet, or one an
 * open ask on the item names. `project_lead`: the lead of the project the
 * work or file is in. `artifact_viewer`, `artifact_editor`, `artifact_owner`:
 * the least role on the one artifact the call names (an editor also authors
 * artifacts).
 */
type BuiltinGate =
  | "everybody"
  | "self"
  | "worklet"
  | "worklet_asked"
  | "project_lead"
  | "doclet"
  | "chatlet"
  | "artifacts"
  | "artifact_viewer"
  | "artifact_editor"
  | "artifact_owner";

export interface ToolRow {
  name: string;
  description: string;
  /** `builtin` or `mcp:<server>`: the two-value origin grammar. */
  source: string;
  /** A human-readable name the server advertised, when it advertised one. */
  title?: string;
  annotations: ToolAnnotations;
  /**
   * WHERE calling this puts something in front of somebody outside the turn,
   * empty for a tool that reaches nobody. The registry's own predicate, not
   * "was this served by MCP": a proven read-only MCP tool delivers nowhere,
   * and Worklet's comment tool delivers although it is a builtin.
   */
  delivers: string;
  /**
   * The JSON Schema the model is offered, ABSENT when the tool takes no
   * arguments. Absent and `{}` are different: only the first means this build
   * did not send one.
   */
  input_schema?: Record<string, unknown>;
  /** The built-in catalogue's shelf this tool is filed on; "" for one it does not file (an MCP server's). */
  group: BuiltinGroupKey | "";
  /** Who may call it; "" for a tool the catalogue does not file, whose calls no authority row decides. */
  gate: BuiltinGate | "";
}
