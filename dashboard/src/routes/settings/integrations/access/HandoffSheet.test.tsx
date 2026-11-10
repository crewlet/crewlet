/**
 * The Set up sheet, where a person makes an agent's robot at Expo, makes its
 * token there and pastes it here; or, where the token is the agent's whole
 * identity (DigitalOcean), makes the token with the level's scopes and pastes it.
 *
 * The invariants: a new agent walks three steps in order, each naming what to
 * press at Expo and where; a token revoked or rotated starts at its token; the
 * field is masked and keeps saved logins out; Check and save is held until
 * there is a value; the engine's refusal stands beside the field and keeps
 * what was pasted; an accepted token is cleared and the engine's sentence
 * shown; and a role changed at Expo is fixed there, then checked again.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { SetupVerdict } from "~/protocol/index.ts";
import { HandoffSheet } from "./HandoffSheet.tsx";
import {
  DIGITALOCEAN_LANE,
  DO_TOKENS_PAGE,
  EXPO_LANE,
  digitaloceanAnswer,
  digitaloceanWaitingAnswer,
  expoAnswer,
  expoWaitingAnswer,
  seat,
} from "./fixtures.ts";
import { handoffOf, rotateHandoffOf, type Handoff, type HandoffStep, type Lane } from "./logic.ts";

afterEach(cleanup);

const PM = seat("pm", "PM", {});
const TOKENS_PAGE = "https://expo.dev/accounts/acme/settings/access-tokens";

/** PM's handoff as the tab reads it off the engine's wait. */
function handoff(step: HandoffStep, over?: Parameters<typeof expoWaitingAnswer>[1]): Handoff {
  return handoffOf(EXPO_LANE, expoWaitingAnswer(step, over).seats[0]!.lanes.expo!)!;
}

/** PM's DigitalOcean handoff as the tab reads it off the engine's wait. */
function doHandoff(
  step: "create" | "mint",
  over?: Parameters<typeof digitaloceanWaitingAnswer>[1],
): Handoff {
  return handoffOf(
    DIGITALOCEAN_LANE,
    digitaloceanWaitingAnswer(step, over).seats[0]!.lanes.digitalocean!,
  )!;
}

function draw(
  handoff: Handoff,
  onPaste: (token: string) => Promise<SetupVerdict> = async () => ({
    outcome: "works",
    summary: "Works.",
  }),
  lane: Lane = EXPO_LANE,
) {
  const onClose = vi.fn();
  const onCheck = vi.fn();
  const paste = vi.fn(onPaste);
  render(
    <LayerHost>
      <HandoffSheet
        seat={PM}
        lane={lane}
        handoff={handoff}
        onClose={onClose}
        onPaste={paste}
        onCheck={onCheck}
        refusal={(err) => ({ said: err instanceof Error ? err.message : String(err) })}
      />
    </LayerHost>,
  );
  return { onClose, onCheck, paste };
}

const sheet = () => screen.getByRole("dialog");
const press = (name: string) => fireEvent.click(within(sheet()).getByRole("button", { name }));
const steps = (vendor = "Expo") =>
  within(within(sheet()).getByRole("list", { name: `Setting up PM on ${vendor}` }))
    .getAllByRole("listitem")
    .map((li) => li.textContent ?? "");

// A NEW AGENT WALKS THREE STEPS, each saying what to press at Expo: the robot,
// named for the agent and at the role chosen here; its token, shown once; and
// the paste. Mutation: start a new agent at the token and the first step is gone.
test("a new agent's sheet walks the robot, its token and the paste, in order", () => {
  draw(handoff("create"));
  expect(screen.getByText("Set up PM on Expo")).toBeTruthy();
  const walked = steps();
  expect(walked).toHaveLength(3);
  ["Create the robot on Expo", "Create its token", "Paste it here"].forEach((label, i) =>
    expect(walked[i]).toContain(label),
  );

  const robot = within(sheet()).getByRole("region", { name: "Create the robot on Expo" });
  expect(within(robot).getByText(/choose Add robot/)).toBeTruthy();
  expect(within(robot).getByText("crewlet-acme-pm")).toBeTruthy();
  expect(within(robot).getByRole("button", { name: "Copy the robot's name" })).toBeTruthy();
  expect(within(robot).getByText("Runner")).toBeTruthy();
  expect(
    within(robot).getByText("View all projects and perform low-risk actions on them"),
  ).toBeTruthy();
  expect(within(robot).getByText("Never choose Admin: agents never hold it.")).toBeTruthy();
  const open = within(robot).getByRole("link", { name: /Open Expo/ });
  expect(open.getAttribute("href")).toBe(TOKENS_PAGE);
  expect(open.getAttribute("target")).toBe("_blank");

  press("Continue");
  const token = within(sheet()).getByRole("region", { name: "Create its token" });
  expect(token.textContent).toContain("On the row of crewlet-acme-pm, choose Create token.");
  expect(token.textContent).toContain("Expo shows the token's value once");

  press("Continue");
  expect(within(sheet()).getByLabelText("Token")).toBeTruthy();
  expect(within(sheet()).getByRole("button", { name: "Back" })).toBeTruthy();
});

// THE PASTE IS A SECRET: masked, and no browser or password manager offers a
// saved login into it. Mutation: drop type=password and every attribute goes.
test("the token field is masked and keeps saved logins out", () => {
  draw(handoff("mint"));
  press("Continue");
  const field = within(sheet()).getByLabelText("Token") as HTMLInputElement;
  expect(field.type).toBe("password");
  expect(field.getAttribute("autocomplete")).toBe("new-password");
  expect(field.hasAttribute("data-1p-ignore")).toBe(true);
  expect(field.getAttribute("data-lpignore")).toBe("true");
});

// A TOKEN REVOKED OR ROTATED STARTS AT THE TOKEN: the robot already exists, so
// the sheet never asks for it again. Mutation: always walk from the robot.
test("a new token for an existing robot starts at its token", () => {
  draw(handoff("mint"));
  expect(screen.getByText("New token for PM")).toBeTruthy();
  expect(steps()).toHaveLength(2);
  expect(within(sheet()).getByRole("region", { name: "Create a new token" })).toBeTruthy();
  expect(within(sheet()).queryByText(/choose Add robot/)).toBeNull();
});

// NOTHING IS SENT WITHOUT A VALUE. Mutation: drop the hold and an empty paste
// reaches the engine.
test("Check and save is held until a token is pasted", () => {
  const { paste } = draw(handoff("mint"));
  press("Continue");
  const save = within(sheet()).getByRole("button", { name: "Check and save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(save);
  expect(paste).not.toHaveBeenCalled();
});

// A REFUSAL IS THE ENGINE'S SENTENCE BESIDE THE FIELD, named by it to a screen
// reader, with what was pasted kept so it can be read again. Typing clears it.
// Mutation: drop the FormField's error and the sentence is nowhere.
test("a refusal stands beside the field and keeps the value", async () => {
  const refusal =
    "This robot is a Developer on acme, and you chose Runner. Change its role with Edit robot at Expo, or choose Developer here.";
  const { paste } = draw(handoff("mint"), async () => {
    throw new Error(refusal);
  });
  press("Continue");
  const field = within(sheet()).getByLabelText("Token") as HTMLInputElement;
  fireEvent.change(field, { target: { value: "robot-token-for-tests" } });
  press("Check and save");
  await waitFor(() => expect(within(sheet()).getByText(refusal)).toBeTruthy());
  expect(paste).toHaveBeenCalledWith("robot-token-for-tests");
  expect(field.getAttribute("aria-invalid")).toBe("true");
  const said = within(sheet()).getByText(refusal);
  expect(field.getAttribute("aria-describedby")?.split(" ")).toContain(said.id);
  expect(field.value).toBe("robot-token-for-tests");
  fireEvent.change(field, { target: { value: "robot-token-for-tests-2" } });
  expect(within(sheet()).queryByText(refusal)).toBeNull();
});

// A REFUSAL'S REMEDY IS ITS OWN ACTION, and goes with the refusal when the value
// changes. Mutation: join them on the field's error and the callout is gone.
test("a refused token says what was wrong at the field and what to do below it", async () => {
  render(
    <LayerHost>
      <HandoffSheet
        seat={PM}
        lane={EXPO_LANE}
        handoff={handoff("mint")}
        onClose={() => {}}
        onPaste={async () => {
          throw new Error("refused");
        }}
        onCheck={() => {}}
        refusal={() => ({
          said: "This robot holds a Developer.",
          action: "Change its role at Expo.",
        })}
      />
    </LayerHost>,
  );
  press("Continue");
  const field = within(sheet()).getByLabelText("Token");
  fireEvent.change(field, { target: { value: "robot-token-for-tests" } });
  press("Check and save");
  const action = await within(sheet()).findByText("Change its role at Expo.");
  expect(action.closest(".crewlet-callout--danger")).toBeTruthy();
  const said = within(sheet()).getByText("This robot holds a Developer.");
  expect(field.getAttribute("aria-describedby")?.split(" ")).toContain(said.id);
  fireEvent.change(field, { target: { value: "robot-token-for-tests-2" } });
  expect(within(sheet()).queryByText("Change its role at Expo.")).toBeNull();
});

// AN ACCEPTED TOKEN IS GONE FROM THE SHEET, and the engine says what it
// checked and kept, the way a Test says it. Mutation: keep the field after
// success and the value stays, or draw only the summary and the facts go.
test("an accepted token is cleared and the engine's verdict is shown", async () => {
  const { onClose } = draw(handoff("mint"), async () => ({
    outcome: "works",
    summary: "Works. The token is sealed for this agent.",
    facts: [
      { label: "Robot", value: "crewlet-acme-pm" },
      { label: "Sealed as", value: "EXPO_TOKEN_PM" },
    ],
  }));
  press("Continue");
  fireEvent.change(within(sheet()).getByLabelText("Token"), {
    target: { value: "robot-token-for-tests" },
  });
  fireEvent.submit(within(sheet()).getByLabelText("Token").closest("form")!);
  await waitFor(() => expect(within(sheet()).getByText(/^Works\./)).toBeTruthy());
  expect(within(sheet()).getByText("crewlet-acme-pm").closest("dd")).toBeTruthy();
  expect(within(sheet()).getByText("EXPO_TOKEN_PM")).toBeTruthy();
  expect(within(sheet()).queryByLabelText("Token")).toBeNull();
  expect(document.body.innerHTML).not.toContain("robot-token-for-tests");
  press("Done");
  expect(onClose).toHaveBeenCalled();
});

// A ROLE CHANGED AT EXPO IS FIXED THERE: both roles, where to change it, and
// the check that reads it back. Mutation: drop the current role and the panel
// cannot say what is wrong.
test("a role changed at Expo shows both roles and checks again when fixed", () => {
  const { onCheck, onClose } = draw(handoff("rerole"));
  expect(screen.getByText("PM's role on Expo")).toBeTruthy();
  expect(within(sheet()).getByText("Developer")).toBeTruthy();
  expect(within(sheet()).getByText("Runner")).toBeTruthy();
  expect(within(sheet()).getByText(/choose Edit robot and set its role to Runner/)).toBeTruthy();
  expect(
    within(sheet())
      .getByRole("link", { name: /Open Expo/ })
      .getAttribute("href"),
  ).toBe(TOKENS_PAGE);
  press("Done, check again");
  expect(onCheck).toHaveBeenCalled();
  expect(onClose).toHaveBeenCalled();
});

// WITHOUT THE ROBOT'S NAME, the first step says the engine's own sentence and
// offers nothing to copy, rather than a name this screen made up.
test("a robot the engine did not name is asked for in the engine's words", () => {
  draw(handoff("create", { named: false }));
  const robot = within(sheet()).getByRole("region", { name: "Create the robot on Expo" });
  expect(
    within(robot).getByText("Add a robot named crewlet-acme-pm with the Runner role at Expo"),
  ).toBeTruthy();
  expect(within(robot).queryByRole("button", { name: /^Copy/ })).toBeNull();
});

// A ROTATE STARTS AT THE PASTE, its token step one step back for a person who
// has not made the new token yet, opening the lane's own page at the vendor,
// since a rotate waits on nothing. Mutation: start a rotate at the token.
test("a rotate starts at the paste, with how to make the token one step back", () => {
  const qa = expoAnswer().seats.find((s) => s.handle === "qa")!.lanes.expo!;
  draw(rotateHandoffOf(EXPO_LANE, qa)!);
  expect(screen.getByText("New token for PM")).toBeTruthy();
  expect(within(sheet()).getByLabelText("Token")).toBeTruthy();
  press("Back");
  const token = within(sheet()).getByRole("region", { name: "Create a new token" });
  expect(token.textContent).toContain("On the row of crewlet-acme-qa, choose Create token.");
  expect(
    within(token)
      .getByRole("link", { name: /Open Expo/ })
      .getAttribute("href"),
  ).toBe(TOKENS_PAGE);
});

// NO PAGE, NO LINK: where neither the wait nor the lane names the vendor's
// page, the step says where in words and draws no Open Expo to nowhere.
test("a step with no page at the vendor draws no Open Expo", () => {
  const qa = expoAnswer().seats.find((s) => s.handle === "qa")!.lanes.expo!;
  draw(rotateHandoffOf({ ...EXPO_LANE, action_url: undefined }, qa)!);
  press("Back");
  const token = within(sheet()).getByRole("region", { name: "Create a new token" });
  expect(within(token).queryByText(/Open Expo/)).toBeNull();
});

// A DIGITALOCEAN TOKEN IS THE AGENT'S WHOLE IDENTITY, so a new agent walks two
// steps: the token, made on DigitalOcean's form in the company's team with the
// name the engine sent and the level's scopes, then the paste. Mutation: walk
// an account step wherever the step is create, and this sheet has three.
test("a new DigitalOcean agent makes its token on the form, with the level's scopes", () => {
  draw(doHandoff("create"), undefined, DIGITALOCEAN_LANE);
  expect(screen.getByText("Set up PM on DigitalOcean")).toBeTruthy();
  const walked = steps("DigitalOcean");
  expect(walked).toHaveLength(2);
  expect(walked[0]).toContain("Generate its token");
  expect(walked[1]).toContain("Paste it here");

  const token = within(sheet()).getByRole("region", { name: "Generate its token" });
  expect(token.textContent).toContain(
    "In the team menu at the top of the control panel, switch to acme. Then choose Generate New Token and fill it in.",
  );
  expect(within(token).getByText("crewlet-acme-pm-operate")).toBeTruthy();
  expect(within(token).getByRole("button", { name: "Copy the token's name" })).toBeTruthy();
  expect(within(token).getByText("Custom Scopes")).toBeTruthy();
  expect(
    within(token).getByText("Never choose Full Access or Read Only: Crewlet refuses both."),
  ).toBeTruthy();

  const ticking = within(token).getByRole("list", { name: "Ticking the DigitalOcean scopes" });
  const items = within(ticking)
    .getAllByRole("listitem")
    .map((li) => li.textContent ?? "");
  expect(items[0]).toBe("Under Quick bulk scope select, choose Select all Reads.");
  expect(items[1]).toBe(
    "Untick these. Crewlet refuses a token that holds any of them: billing:read, security:read.",
  );
  expect(items[2]).toBe("Untick these too. Agents do not need them: genai:read.");
  expect(items[3]).toContain("The whole section, then untick droplet:admin");
  expect(items[3]).toContain("project:assign_resource");
  expect(items[3]).toContain(
    "Never use Select all Creates, Updates or Deletes: they select scopes Crewlet refuses.",
  );
  expect(token.textContent).toContain("choose Add scopes and continue");
  expect(token.textContent).not.toContain("Alex on acme");
  expect(
    within(token)
      .getByRole("link", { name: /Open DigitalOcean/ })
      .getAttribute("href"),
  ).toBe(DO_TOKENS_PAGE);
});

// A ROTATE THERE RENEWS IN PLACE: Regenerate Token on the token's own row keeps
// its name and scopes, so the form and its scopes are not asked for again.
// Mutation: drop the regenerate words and the rotate walks the whole form.
test("a DigitalOcean rotate regenerates the token on its row", () => {
  const sre = digitaloceanAnswer().seats[1]!.lanes.digitalocean!;
  draw(rotateHandoffOf(DIGITALOCEAN_LANE, sre)!, undefined, DIGITALOCEAN_LANE);
  expect(screen.getByText("New token for PM")).toBeTruthy();
  expect(within(sheet()).getByLabelText("Token")).toBeTruthy();
  press("Back");
  const token = within(sheet()).getByRole("region", { name: "Regenerate its token" });
  expect(token.textContent).toContain(
    "open the menu on the row of crewlet-acme-sre-operate and choose Regenerate Token. The token keeps its name and scopes.",
  );
  expect(within(token).queryByText("Custom Scopes")).toBeNull();
});

// WITHOUT THE ENGINE'S NAME, NO NAME: the token's owner is never offered as
// the name to type, and nothing is there to copy. Mutation: fall back to the
// observed account and "Alex on acme" becomes the token's name.
test("a DigitalOcean token the engine did not name is offered no name", () => {
  draw(doHandoff("mint", { named: false }), undefined, DIGITALOCEAN_LANE);
  expect(screen.getByText("New token for PM")).toBeTruthy();
  const token = within(sheet()).getByRole("region", { name: "Generate a new token" });
  expect(within(token).queryByRole("button", { name: /^Copy/ })).toBeNull();
  expect(token.textContent).not.toContain("Alex on acme");
});

// THE TEAM IS NAMED WHERE THE ENGINE SENT IT, and said plainly where not.
test("without the team, the sheet says to switch to the team Crewlet is connected to", () => {
  draw(doHandoff("create"), undefined, { ...DIGITALOCEAN_LANE, vendor_account: undefined });
  const token = within(sheet()).getByRole("region", { name: "Generate its token" });
  expect(token.textContent).toContain("switch to the team Crewlet is connected to.");
});
