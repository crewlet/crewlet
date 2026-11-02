/**
 * A tool's arguments, read off the JSON Schema the model is offered: each
 * property by name, its type as the schema states it, whether it is
 * required, and its description.
 *
 * READ, NEVER COMPLETED. A type the schema does not state is "" and is drawn
 * as its absence rather than guessed; a list of types (`["string", "null"]`)
 * is drawn whole. A tool with no schema is null, which is not a tool that
 * takes nothing (`[]`): only the second is something the engine said.
 */

/** One argument as the schema declares it. */
export interface Argument {
  name: string;
  type: string;
  required: boolean;
  description: string;
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

export function argumentsOf(schema: Record<string, unknown> | null | undefined): Argument[] | null {
  if (!schema) return null;
  const props = schema["properties"];
  if (!isRecord(props)) return [];
  const required = Array.isArray(schema["required"]) ? (schema["required"] as unknown[]) : [];
  return Object.entries(props).map(([name, raw]) => {
    const field = isRecord(raw) ? raw : {};
    const type = field["type"];
    return {
      name,
      type: typeof type === "string" ? type : Array.isArray(type) ? type.join(" | ") : "",
      required: required.includes(name),
      description: typeof field["description"] === "string" ? field["description"] : "",
    };
  });
}
