/**
 * The task an agent is asked to build an HTML page in: its title and its body,
 * composed from what the person wrote, and shown to them exactly as it is sent.
 *
 * PURE, so the page draws the very text it files and a test reads it.
 */

import type { ArtifactCapability } from "~/protocol/index.ts";
import { CAPABILITY_CHOICES } from "./words.ts";

/** What the person asked for. */
export interface PageBrief {
  title: string;
  brief: string;
  capabilities: readonly ArtifactCapability[];
}

/** How many characters of the brief name the task when the page has no title. */
const NAMING_CHARS = 60;

/** The page's name as the task says it: its title, else the brief's first words. */
export function pageName(b: PageBrief): string {
  const title = b.title.trim();
  if (title) return title;
  const first = b.brief.trim().split("\n")[0]!.trim();
  if (first.length <= NAMING_CHARS) return first;
  const cut = first.slice(0, NAMING_CHARS);
  return `${cut.slice(0, Math.max(cut.lastIndexOf(" "), 1)).trimEnd()}…`;
}

/** The task's title. */
export function taskTitle(b: PageBrief): string {
  const name = pageName(b);
  return name ? `Build the HTML page “${name}”` : "Build an HTML page";
}

/**
 * The task's body: the brief as written, what the page must be able to do in
 * the create page's words with the engine's capability names, and how it is
 * delivered.
 */
export function taskBody(b: PageBrief): string {
  const parts = [b.brief.trim()];
  const caps = CAPABILITY_CHOICES.filter((c) => b.capabilities.includes(c.capability));
  if (caps.length > 0) {
    parts.push(
      [
        "What the page can do:",
        ...caps.map((c) => `- ${c.label} (the \`${c.capability}\` capability)`),
      ].join("\n"),
    );
  } else {
    parts.push("The page needs no capabilities: it keeps nothing and knows no one.");
  }
  const title = b.title.trim();
  parts.push(
    `Publish it as an HTML artifact${title ? ` titled “${title}”` : ""}, declaring only the capabilities above, and share it with me as an editor.`,
  );
  return parts.filter(Boolean).join("\n\n");
}
