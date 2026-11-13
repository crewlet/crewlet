import { describe, expect, test } from "vitest";
import { heldCallNamed, heldCallTitle, heldRunTitle } from "./heldCall.ts";

const HEAD = "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345";
const MERGE = { head: HEAD, method: "squash", reason: "Fixes the login loop." };
const CALL = { tool: "ask_to_merge", server: "crewlet" };

// ONE TITLE FOR A HELD CALL, read from its own arguments in product words:
// the decision row, the Inbox's list and its pane all draw this one string.
describe("a held call's title", () => {
  test("a merge names its change as a person does", () => {
    const pull = { ...MERGE, change: "https://github.com/acme/web/pull/88" };
    expect(heldCallTitle({ ...CALL, act: "pr_merge", args: pull })).toBe(
      "Merge pull request 88 in acme/web?",
    );
    expect(heldCallTitle({ ...CALL, act: "pr_merge", args: { ...MERGE, change: "web!42" } })).toBe(
      "Merge change 42 in web?",
    );
  });

  test("a deploy names its repository and environment, a command its server", () => {
    const deploy = {
      repo: "web",
      commit: HEAD,
      changes: ["web!41"],
      environment: "production",
      rollback: "Redeploy the previous commit.",
      reason: "Ships the login fix.",
    };
    expect(
      heldCallTitle({ tool: "ask_to_deploy", server: "crewlet", act: "deploy", args: deploy }),
    ).toBe("Deploy web to production?");
    expect(
      heldCallTitle({
        tool: "ssh_operate",
        server: "crewlet",
        act: "ssh_operate",
        args: { host: "web-1", command: "uptime", reason: "Checks the load." },
      }),
    ).toBe("Run a command on web-1?");
  });

  // A READER NOT SHOWN THE ARGUMENTS is asked in the engine's own question,
  // never by the tool's name, and so is a call whose arguments are not the
  // act's own (`releaseRequestOf` reads them whole or not at all).
  test("without its arguments it is the engine's own question", () => {
    const question = "May cto merge web!42 at 4f1c2a9b7e3d?";
    expect(heldCallTitle({ ...CALL, act: "pr_merge", args: undefined, question })).toBe(question);
    expect(
      heldCallTitle({ ...CALL, act: "pr_merge", args: { ...MERGE, force: true }, question }),
    ).toBe(question);
    expect(heldCallTitle({ ...CALL, act: "pr_merge", args: null })).toBe(
      "Call ask_to_merge on crewlet?",
    );
  });

  test("an MCP tool's call is named by its tool, whatever its question says", () => {
    const call = { tool: "tr_close_issue", server: "Tracker", args: { issue: 42 } };
    expect(heldCallTitle({ ...call, question: "May swe call tr_close_issue?" })).toBe(
      "Call tr_close_issue on Tracker?",
    );
    expect(heldCallTitle({ ...call, act: "other", question: "May swe call it?" })).toBe(
      heldCallNamed(call),
    );
  });

  test("a parked run's title is its held call's, and null for a run parked on a question", () => {
    expect(
      heldRunTitle({ held_call: undefined, held_arguments: undefined, question: "Why?" }),
    ).toBe(null);
    expect(
      heldRunTitle({
        held_call: {
          id: "h-1",
          server: "crewlet",
          tool: "ask_to_merge",
          raw: "ask_to_merge",
          digest: "sha256:d1",
          held_at: "",
          act: "pr_merge",
          delegation: null,
        },
        held_arguments: { ...MERGE, change: "web!42" },
        question: "May cto merge web!42 at 4f1c2a9b7e3d?",
      }),
    ).toBe("Merge change 42 in web?");
  });
});
