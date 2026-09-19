/**
 * Reading a tool's behavioural hints.
 *
 * PURE FUNCTIONS OVER VALUES, for the reason `lib/spend.ts` gives: a rule
 * exercised only by rendering a component is a rule nobody re-measures, and
 * every rule here decides what an operator is told a tool can do.
 *
 * THE THIRD VALUE IS THE WHOLE POINT. The engine's own hints are tri-state
 * because "the server did not advertise this" and "the server said no" are
 * different facts, and this file may never collapse them: a fresh MCP server
 * annotates nothing, so treating unknown as a denial would present its every
 * tool as a proven read on the one screen an operator audits it on.
 */

import type { Tone } from "~/ui/primitives.tsx";
import type { ToolAnnotations, ToolHint, ToolRow } from "~/protocol/types.ts";

/** What a tool is allowed to do, as one word for a chip. */
export type Capability = "reads" | "writes" | "destroys" | "unknown";

/**
 * The strongest thing the hints POSITIVELY assert.
 *
 * Positively is the operative word and it is the engine's own rule (see
 * `Registry.KnownReads`): an unannotated tool is not a known read. The order
 * is worst-first, because the question an operator asks this column is "what
 * is the most this could do", and a tool that is both a write and
 * irreversible has to answer "destroys".
 */
export function capabilityOf(ann: ToolAnnotations | undefined): Capability {
  if (!ann) return "unknown";
  if (ann.destructive === "yes") return "destroys";
  if (ann.read_only === "no") return "writes";
  if (ann.read_only === "yes") return "reads";
  return "unknown";
}

/**
 * The tone a capability is drawn in — worse means louder.
 *
 * `unknown` gets NO tone rather than a quiet one. It is not a mild version of
 * "reads": it is the absence of a claim, and colouring it as if it were a
 * reading is the collapse this whole file exists to prevent.
 */
export function capabilityTone(c: Capability): Tone | undefined {
  switch (c) {
    case "destroys":
      return "critical";
    case "writes":
      return "caution";
    case "reads":
      return "positive";
    default:
      return undefined;
  }
}

/**
 * The sentence under a tool's name: what its hints actually say.
 *
 * Every unadvertised hint is NAMED rather than skipped, because "nobody said"
 * is the fact an operator acts on — it is what tells them to read the server's
 * own documentation before granting a seat this tool.
 */
export function hintSentence(ann: ToolAnnotations | undefined): string {
  if (!ann) return "this build sent no hints for this tool";
  const said: string[] = [];
  const silent: string[] = [];
  const note = (hint: ToolHint, yes: string, no: string, name: string) => {
    if (hint === "yes") said.push(yes);
    else if (hint === "no") said.push(no);
    else silent.push(name);
  };
  note(ann.read_only, "reads only", "can write", "read-only");
  note(ann.destructive, "can be irreversible", "is reversible", "destructive");
  note(ann.idempotent, "repeats safely", "repeats with effect", "idempotent");
  note(ann.open_world, "reaches outside the company", "stays inside the company", "open-world");
  if (!said.length) return "nothing was advertised about what this tool does";
  const tail = silent.length ? `; nothing said about ${silent.join(", ")}` : "";
  return said.join(", ") + tail;
}

/**
 * How many of a tool's four hints carry an assertion either way.
 *
 * The column an operator sorts on when auditing a new server: a row at 0 of 4
 * is a tool the engine can say nothing about, and a table sorted by it puts
 * every one of them together.
 */
export function hintsAdvertised(ann: ToolAnnotations | undefined): number {
  if (!ann) return 0;
  return [ann.read_only, ann.destructive, ann.idempotent, ann.open_world].filter(
    (h) => h === "yes" || h === "no",
  ).length;
}

/** The named arguments a tool's schema declares, in the schema's own order. */
export function schemaFields(tool: ToolRow): { name: string; type: string; required: boolean }[] {
  const schema = tool.input_schema;
  if (!schema) return [];
  const props = schema["properties"];
  if (!props || typeof props !== "object") return [];
  const required = Array.isArray(schema["required"]) ? (schema["required"] as unknown[]) : [];
  return Object.entries(props as Record<string, unknown>).map(([name, raw]) => {
    const field = (raw ?? {}) as Record<string, unknown>;
    // `type` is a string in almost every schema and a list in a few
    // (`["string", "null"]`). Both render; anything else is reported as
    // its own absence rather than as "object", which would be a claim.
    const type = field["type"];
    return {
      name,
      type: typeof type === "string" ? type : Array.isArray(type) ? type.join(" | ") : "",
      required: required.includes(name),
    };
  });
}
