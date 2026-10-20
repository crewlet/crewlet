/**
 * Create flow: what a "Make something new" tile on the Artifacts gallery
 * looks like, where pressing one lands (straight in that kind's create or
 * edit experience), and a gallery of smaller cards. Today and five
 * directions, each in its own folder (`variants/<id>/variant.ts`), sharing the
 * create canvases, the copy and the commit paths in `shared/`.
 *
 * THE DIRECTIONS ARE BUILT AGAINST UILET BRANCH `proto/create-flow`, which
 * adds the primitives they draw (`ArtifactKindMark`, the tile appearances,
 * the card's tile size, `ArtifactTemplateCard`, `PromptStarters`,
 * `DottedGround`, the title input, `ChoiceList`'s leading picture and wide
 * side sheets). Build that branch and install its pack into
 * `node_modules/@crewlethq/ui` before checking or building this topic, as the
 * typecheck reads the installed package.
 */

import { defineTopic } from "../../harness/defineTopic.ts";
import { addAgent, addPerson, compose, scenario } from "../../fixtures/scenarios.ts";
import { noArtifacts, TEAM, withArtifacts } from "./fixtures/estate.ts";
import studio from "./variants/studio/variant.ts";
import composer from "./variants/composer/variant.ts";
import crew from "./variants/crew/variant.ts";
import templates from "./variants/templates/variant.ts";
import quick from "./variants/quick/variant.ts";

/** Priya designs and Jonas runs finance: two more people who own and share artifacts. */
const people = compose(
  addPerson({ name: "Priya Shah", handle: "priya", unit: TEAM }),
  addPerson({ name: "Jonas Weber", handle: "jonas" }),
);

/**
 * Two specialists beside the PM and the engineer, so asking for a design or
 * a dashboard has an agent suited to it to suggest: a designer and an
 * analyst, each drawn as their own character.
 */
const specialists = compose(
  addAgent({
    name: "Crewlet-Design",
    handle: "design",
    unit: TEAM,
    goal: "Design the product's screens and states, in the house style.",
    character: "prismlet",
    color: "purple",
  }),
  addAgent({
    name: "Crewlet-Data",
    handle: "data",
    unit: TEAM,
    goal: "Answer questions with data: metrics, dashboards and analysis.",
    character: "dashlet",
    color: "cyan",
  }),
);

export default defineTopic({
  id: "create-flow",
  title: "Create Flow Proposals",
  start: "#/artifacts",
  scenarios: [
    scenario("month", "A month in", compose(people, specialists, withArtifacts)),
    scenario("first-day", "First day, nothing made", compose(people, specialists, noArtifacts)),
  ],
  variants: [
    { id: "today", label: "Today", summary: "The dashboard as it ships." },
    studio,
    composer,
    crew,
    templates,
    quick,
  ],
  static: {
    frames: "./frames",
    "frames/_crewlet/runtime/1.0.0": "../../../../static/artifacts/runtime/1.0.0",
    "frames/_crewlet/kit/0.20.0": "../../../node_modules/@crewlethq/artifact-kit/dist",
    // THE TOKENS WITHOUT THE FACES: a frame's opaque origin loads no font a
    // static host serves without CORS, so a page draws in the system face.
    "frames/_crewlet/tokens": "../../../node_modules/@crewlethq/tokens/dist/css",
  },
});
