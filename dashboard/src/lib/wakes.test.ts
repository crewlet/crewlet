import { expect, test } from "vitest";
import { wakeWords } from "./wakes.ts";

// EVERY WAKE KIND THE LEDGER KEYS HAS WORDS and says who can act; one this
// build does not know is said as written, says so, and sends nobody anywhere.
test("a wake reason is said with who can act on it", () => {
  expect(wakeWords("schedule:standup", "ceo")).toEqual({
    label: "Schedule standup",
    who: "An admin, by changing how often it runs",
    path: ["company", "schedules", "role", "ceo", "standup"],
  });
  expect(wakeWords("work_item:pickup", "swe").label).toBe("Work handed to it while idle");
  expect(wakeWords("work_item:ask_answered", "swe").label).toBe("Work items (ask answered)");
  expect(wakeWords("chat_message:mention", "swe").path).toEqual(["chatlet"]);
  expect(wakeWords("colleague_ask", "swe").path).toEqual(["company", "org", "seats", "swe"]);
  expect(wakeWords("code_change:review_requested", "swe").path).toEqual(["gitlet"]);
  expect(wakeWords("alarm:fired", "sre").label).toBe("Engine alarms");
  expect(wakeWords("delegated_request:ssh_operate", "security-engineer").path).toEqual([
    "settings",
    "delegated-decisions",
  ]);
  expect(wakeWords("task_assigned", "swe")).toEqual({
    label: "task assigned",
    who: "Not described in this version",
    path: [],
  });
});
