import { describe, expect, it } from "vitest";
import { activityEntries, parseActivityLine, toolLabel } from "./activity.ts";

const transcript = [
  "I'll check the PR state first.",
  '[tool 1] mcp__crewlet__get_work_item: {"item":"ENG-12"}',
  "[ok 1 40ms]",
  "[tool 2] Bash: python3 - <<'EOF' (+3 more line(s))",
  "[input 2] python3 - <<'EOF'",
  "[input 2] import json",
  "[input 2]   print(1)",
  "[ok 2 2400ms]",
  "[tool 3] Read: /repo/a.jsx",
  "[failed 3 12ms] File does not exist.",
  "All 363 pass.",
  "Now checking the base.",
  "[note] 3 line(s) of output not read",
  "[tool 4] Grep: TODO",
].join("\n");

describe("parseActivityLine", () => {
  it("reads every tag as the engine writes it", () => {
    expect(parseActivityLine("[tool 2] Bash: python3 - <<'EOF' (+3 more line(s))")).toEqual({
      tag: "tool",
      seq: 2,
      tool: "Bash",
      text: "python3 - <<'EOF'",
      more: 3,
      tookMs: null,
    });
    expect(parseActivityLine("[input 2]   print(1)").text).toBe("  print(1)");
    expect(parseActivityLine("[input 2]").text).toBe("");
    expect(parseActivityLine("[ok 2 2400ms]")).toMatchObject({ tag: "ok", seq: 2, tookMs: 2400 });
    expect(parseActivityLine("[failed 2400ms] denied")).toMatchObject({
      tag: "failed",
      seq: 0,
      tookMs: 2400,
      text: "denied",
    });
    expect(parseActivityLine("[tool 1] mcp__crewlet__get_work_item")).toMatchObject({
      tool: "mcp__crewlet__get_work_item",
      text: "",
    });
  });

  it("reads anything malformed as the agent's words, whole", () => {
    for (const line of [
      "[x] done",
      "[tool] Bash: go test",
      "[tool 0] Bash",
      "[tool 2]",
      "[input] a",
      "[ok 2 fastms]",
      "[note 2] a",
      "[ok 2",
    ]) {
      expect(parseActivityLine(line)).toMatchObject({ tag: null, text: line });
    }
  });
});

describe("toolLabel", () => {
  it("keeps a harness's tools and humanises an MCP server's", () => {
    expect(toolLabel("Bash")).toBe("Bash");
    expect(toolLabel("read")).toBe("Read");
    expect(toolLabel("mcp__crewlet__get_work_item")).toBe("Crewlet · Get work item");
    expect(toolLabel("mcp__my-server__listOpenIssues")).toBe("My Server · List open issues");
  });
});

describe("activityEntries", () => {
  it("reads a transcript into narration, calls with their results and notes", () => {
    const entries = activityEntries(transcript, false);
    expect(entries.map((e) => e.kind)).toEqual([
      "say",
      "call",
      "call",
      "call",
      "say",
      "note",
      "call",
    ]);
    expect(entries[0]).toMatchObject({ kind: "say", text: "I'll check the PR state first." });
    expect(entries[1]).toMatchObject({
      tool: "Crewlet · Get work item",
      status: "ok",
      durationMs: 40,
    });
    expect(entries[1]).not.toHaveProperty("input");
    expect(entries[2]).toMatchObject({
      tool: "Bash",
      detail: "python3 - <<'EOF'",
      input: "python3 - <<'EOF'\nimport json\n  print(1)",
      inputUnkept: 1,
      status: "ok",
      durationMs: 2400,
    });
    expect(entries[3]).toMatchObject({
      status: "error",
      error: "File does not exist.",
      durationMs: 12,
    });
    expect(
      activityEntries("[tool 1] Bash: npm test\n[failed 1 9ms] exit 1 (+2 more line(s))", false)[0],
    ).toMatchObject({ error: "exit 1…" });
    expect(entries[4]).toMatchObject({
      kind: "say",
      text: "All 363 pass.\nNow checking the base.",
    });
    expect(entries[6]).toMatchObject({ tool: "Grep", status: "unknown" });
  });

  it("marks a call with no result running while the run is live", () => {
    expect(activityEntries(transcript, true).at(-1)).toMatchObject({ status: "running" });
  });

  it("cuts a file's path at its start and a command at its end", () => {
    const entries = activityEntries(transcript, false);
    expect(entries[3]).toMatchObject({ tool: "Read", detailCut: "start" });
    expect(entries[2]).toMatchObject({ tool: "Bash", detailCut: "end" });
  });

  it("folds lookups by tool and keeps commands apart", () => {
    const entries = activityEntries(transcript, false);
    expect(entries[1]).toMatchObject({ group: "Crewlet · Get work item" });
    expect(entries[3]).toMatchObject({ group: "Read" });
    expect(entries[2]).toMatchObject({ group: entries[2]!.key });
  });

  it("indents an MCP call's arguments when expanded", () => {
    const long = `{"item":"ENG-12","body":"${"x".repeat(200)}"}`;
    const [call] = activityEntries(
      `[tool 1] mcp__crewlet__comment: ${long.slice(0, 150)}…\n[input 1] ${long}`,
      false,
    );
    expect(call).toMatchObject({ kind: "call" });
    expect(call!.kind === "call" && call!.input?.split("\n").length).toBeGreaterThan(1);
  });

  it("keeps a failure whose call began before the view, and drops what else it cannot place", () => {
    const entries = activityEntries(
      "[input 9] rm -rf build\n[ok 9 5ms]\n[failed 8] exit status 1",
      true,
    );
    expect(entries).toEqual([
      {
        kind: "note",
        key: expect.any(String),
        tone: "danger",
        text: "A call failed: exit status 1",
      },
    ]);
  });

  it("keys a row by where it stands in the run, so a view that drops its start keeps it", () => {
    const whole = "first\n[tool 1] Bash: ls\n[ok 1]\nsecond";
    const dropped = "first\n".length;
    const later = activityEntries(whole.slice(dropped), false, dropped);
    expect(later.map((e) => e.key)).toEqual(
      activityEntries(whole, false)
        .slice(1)
        .map((e) => e.key),
    );
  });
});
