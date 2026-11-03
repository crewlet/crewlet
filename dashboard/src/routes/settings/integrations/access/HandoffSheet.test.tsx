/**
 * The Set up sheet, where a person makes an agent's robot at Expo, makes its
 * token there and pastes it here.
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
import { HandoffSheet } from "./HandoffSheet.tsx";
import { EXPO_LANE, expoWaitingAnswer, seat } from "./fixtures.ts";
import { handoffOf, type Handoff, type HandoffStep } from "./logic.ts";

afterEach(cleanup);

const PM = seat("pm", "PM", {});
const TOKENS_PAGE = "https://expo.dev/accounts/gophrase/settings/access-tokens";

/** PM's handoff as the tab reads it off the engine's wait. */
function handoff(step: HandoffStep, over?: Parameters<typeof expoWaitingAnswer>[1]): Handoff {
  return handoffOf(EXPO_LANE, expoWaitingAnswer(step, over).seats[0]!.lanes.expo!)!;
}

function draw(handoff: Handoff, onPaste: (token: string) => Promise<string> = async () => "") {
  const onClose = vi.fn();
  const onCheck = vi.fn();
  const paste = vi.fn(onPaste);
  render(
    <LayerHost>
      <HandoffSheet
        seat={PM}
        lane={EXPO_LANE}
        handoff={handoff}
        onClose={onClose}
        onPaste={paste}
        onCheck={onCheck}
        refusal={(err) => (err instanceof Error ? err.message : String(err))}
      />
    </LayerHost>,
  );
  return { onClose, onCheck, paste };
}

const sheet = () => screen.getByRole("dialog");
const press = (name: string) => fireEvent.click(within(sheet()).getByRole("button", { name }));
const steps = () =>
  within(within(sheet()).getByRole("list", { name: "Setting up PM on Expo" }))
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
  expect(within(robot).getByText("crewlet-gophrase-pm")).toBeTruthy();
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
  expect(token.textContent).toContain("On the row of crewlet-gophrase-pm, choose Create token.");
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
    "This robot is a Developer on gophrase, and you chose Runner. Change its role with Edit robot at Expo, or choose Developer here.";
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

// AN ACCEPTED TOKEN IS GONE FROM THE SHEET, and the engine says what it
// checked and kept. Mutation: keep the field after success and the value stays.
test("an accepted token is cleared and the engine's sentence is shown", async () => {
  const works = "Works. Robot crewlet-gophrase-pm, Runner on gophrase. Sealed as EXPO_TOKEN_PM.";
  const { onClose } = draw(handoff("mint"), async () => works);
  press("Continue");
  fireEvent.change(within(sheet()).getByLabelText("Token"), {
    target: { value: "robot-token-for-tests" },
  });
  fireEvent.submit(within(sheet()).getByLabelText("Token").closest("form")!);
  await waitFor(() => expect(within(sheet()).getByText(/^Works\./)).toBeTruthy());
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
    within(robot).getByText("Add a robot named crewlet-gophrase-pm with the Runner role at Expo"),
  ).toBeTruthy();
  expect(within(robot).queryByRole("button", { name: /^Copy/ })).toBeNull();
});
