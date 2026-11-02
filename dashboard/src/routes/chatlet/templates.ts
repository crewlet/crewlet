/**
 * The rooms a person can start from a template.
 *
 * A TEMPLATE IS TWO WORDS AND A NAME: the topic and the description it sets,
 * in one `update_channel` that names it, so the room's transcript says "set
 * this room up as a Project room" once rather than a topic line and a
 * description line. The texts are the screen's own presets: the engine records
 * the name a patch states and holds both texts to the room's caps, which these
 * are written well inside (`templates.test.ts`).
 */

export interface RoomTemplate {
  id: "project" | "incident" | "release" | "standup";
  /** What the template is called, and what the transcript line names. */
  name: string;
  /** The room's topic, one line. */
  topic: string;
  /** The room's description, which agents in the room read as context. */
  purpose: string;
  /** What the choice says about it, under its name. */
  blurb: string;
}

export const ROOM_TEMPLATES: readonly [RoomTemplate, ...RoomTemplate[]] = [
  {
    id: "project",
    name: "Project room",
    topic: "Plan, build and ship. Decisions and status live here.",
    purpose:
      "Where the people and agents on this project plan the work, post progress and settle decisions. Link the Worklet tasks you talk about.",
    blurb: "Plans, progress and decisions for one piece of work.",
  },
  {
    id: "incident",
    name: "Incident room",
    topic: "Live incident. Status every 30 minutes until resolved.",
    purpose:
      "One place for an incident: what is broken, who is on it, what has been tried, and the timeline for the write-up afterwards.",
    blurb: "Status, owners and a timeline while something is broken.",
  },
  {
    id: "release",
    name: "Release room",
    topic: "Builds, checks and store submissions for the next release.",
    purpose:
      "Release coordination: builds as they land, QA sign-off, store submissions and release notes.",
    blurb: "Builds, sign-off and notes on the way to a release.",
  },
  {
    id: "standup",
    name: "Team standup",
    topic: "Daily standup. Yesterday, today, blockers.",
    purpose:
      "Each person and agent posts what they did, what is next and what blocks them. Agents post theirs on a schedule.",
    blurb: "A daily check-in that agents post to on a schedule.",
  },
];

/** The template an id names; the first for an id this build does not list. */
export function templateNamed(id: string): RoomTemplate {
  return ROOM_TEMPLATES.find((entry) => entry.id === id) ?? ROOM_TEMPLATES[0];
}
