/**
 * The MCP server wizard's draft as the Dashboard Assistant reads it: what is
 * being added or edited, field by field, in the wizard's words, so a question
 * about it ("is this the right address for Linear?") is answered about this
 * draft.
 *
 * READ, NEVER SET. Every field changes through the wizard's own step, where
 * the engine tests the server before anything is stored; a change the
 * assistant made would skip that. And no value is here: a header or
 * variable is named, never what was typed for it, which stays on the page
 * until it is sealed.
 */

import type { DraftFieldSpec } from "~/app/assistant/draft.ts";
import { argsOf, rowsOf, valuesOf, type Draft } from "./draft.ts";
import { SIGN_IN } from "../words.ts";

/** What the wizard tells the assistant it can do with the draft. */
export const SERVER_DRAFT_HELP =
  "This is an MCP server being set up in the MCP & tools wizard, not saved. Read it to answer questions about it; the person changes it on the wizard's own steps, where the engine tests the server before anything is stored. Values typed for its headers or variables are never shown here.";

/** The draft's fields, each read in the wizard's words. */
export const SERVER_DRAFT_FIELDS: readonly DraftFieldSpec<Draft>[] = [
  { path: "name", label: "Name", read: (d) => d.name },
  { path: "description", label: "What it is for", read: (d) => d.description },
  {
    path: "source",
    label: "Made from",
    read: (d) =>
      d.catalogue
        ? `${d.catalogue.title}${d.catalogue.community ? " (a community listing nobody here reviewed)" : " (the curated catalogue)"}`
        : d.source === "json"
          ? "A pasted configuration"
          : "An address",
  },
  {
    path: "connection",
    label: "Connects",
    read: (d) =>
      d.transport === "http"
        ? `Remote, at ${d.url}`
        : `Local, as ${[d.command, ...argsOf(d.args)].join(" ")}`,
  },
  { path: "tool_prefix", label: "Tool prefix", read: (d) => d.toolPrefix || "None" },
  { path: "sign_in", label: "Sign-in", read: (d) => SIGN_IN[d.signIn].label },
  {
    path: "keys",
    label: "Headers or variables",
    read: (d) => {
      const typed = new Set(Object.keys(valuesOf(d)));
      const rows = rowsOf(d).filter((r) => r.key.trim());
      return rows.length > 0
        ? rows
            .map(
              (r) =>
                `${r.key.trim()}: ${typed.has(r.key.trim()) ? "a new value, sealed when saved" : r.stored ? "stored" : "no value yet"}`,
            )
            .join("\n")
        : "None";
    },
  },
  {
    path: "instances",
    label: "Instances",
    read: (d) => (d.shared ? "One for the company" : "One per agent"),
  },
];
