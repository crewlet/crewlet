/**
 * Test a server: on every node that runs it, the stages a connection passes
 * (address, resolve, connect, certificate, discover, list tools; or launch,
 * discover, list for a local server), each timed, and where one fails, its
 * class in plain words with the one thing that fixes it. A test asks the
 * server who it is and lists its tools; it never calls a tool, because no
 * tool is guaranteed to be free of side effects.
 *
 * THE ENGINE DECIDES each stage, its state and the failure's class
 * (`POST /mcp-servers/{name}/check`); the page names them. A per-agent server
 * is tested as one agent, on the node that holds that agent, with its own
 * credentials: the first agent the engine grants it, unless the reader picks
 * another.
 */

import { useEffect, useEffectEvent, useState } from "react";
import {
  Button,
  Callout,
  Disclosure,
  FormField,
  Modal,
  Select,
  Spinner,
  StepList,
  Text,
} from "@crewlethq/ui";
import type { McpCheckAnswer, McpCheckNodeView, McpMemberServerStatus } from "~/contract/mcp.ts";
import { plural } from "~/lib/format.ts";
import { testServer } from "../api.ts";
import type { McpData } from "../data.ts";
import { FAILURE, failureWords, STAGE, STAGE_STATE } from "../words.ts";

/** A stage's time, as the step list prints it. */
function took(ms: number): string {
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

/**
 * One node's stages, as a list a reader scans down to the failure: the kit's
 * StepList, whose one state column keeps each stage's mark and time on its
 * name's line however far what it found wraps. A stage after a failure was
 * never reached, and says so.
 */
function NodeSteps({ node, multi }: { node: McpCheckNodeView; multi: boolean }) {
  return (
    <StepList
      label={node.node ? `Stages on ${node.node}` : "Stages"}
      summary={multi ? node.node : undefined}
      labels={{ pending: "Not reached" }}
      steps={node.steps.map((s) => ({
        id: s.stage,
        label: STAGE[s.stage],
        state: STAGE_STATE[s.state],
        ...(s.state === "passed" || s.state === "failed" ? { duration: took(s.took_ms) } : {}),
        ...(s.detail ? { detail: s.detail } : {}),
      }))}
    />
  );
}

/** What a test found, for the dialog, the wizard's Test step and a refused value alike. */
export function TestResult({ answer }: { answer: McpCheckAnswer }) {
  const answered = answer.nodes.filter((n) => n.answered);
  const silent = answer.nodes.filter((n) => !n.answered);
  const failed = answered.filter((n) => n.failure && n.failure.class !== "empty_tools");
  const empty = answered.some((n) => n.failure?.class === "empty_tools");
  const multi = answer.nodes.length > 1;
  const first = failed[0]?.failure ?? null;
  const words = first ? failureWords(first.class, first.said) : null;
  return (
    <div className="mcp-stack">
      {answered.length === 0 ? (
        <Callout variant="danger" title="No node answered the test">
          {silent.map((n) => n.reason).find(Boolean) ??
            "Ask again in a moment, or test from the server's page once it runs."}
        </Callout>
      ) : words ? (
        <Callout
          variant={
            first?.class === "needs_sign_in" || first?.class === "rate_limited"
              ? "warning"
              : "danger"
          }
          title={words.title}
        >
          {words.remedy}
          {multi && failed.length < answered.length
            ? ` It fails on ${failed.map((n) => n.node).join(", ")} only.`
            : ""}
        </Callout>
      ) : empty && answer.tools.length === 0 ? (
        <Callout variant="warning" title={FAILURE.empty_tools.title}>
          {FAILURE.empty_tools.remedy}
        </Callout>
      ) : (
        <Callout
          variant="success"
          title={`It answers, with ${plural(answer.tools.length, "tool")}`}
        >
          {answer.server_info
            ? `Reported as ${[answer.server_info.name, answer.server_info.version].filter(Boolean).join(" ")}${answer.server_info.protocol ? `, protocol ${answer.server_info.protocol}` : ""}.`
            : "The server answered."}
        </Callout>
      )}
      {answer.warnings.length > 0 ? (
        <Callout
          variant="warning"
          title={
            answer.warnings.length === 1 ? "One warning" : `${answer.warnings.length} warnings`
          }
        >
          <ul className="mcp-list">
            {answer.warnings.map((w) => (
              <li key={w}>{w}</li>
            ))}
          </ul>
        </Callout>
      ) : null}
      {answered.map((n) => (
        <NodeSteps key={n.node || "here"} node={n} multi={multi} />
      ))}
      {answered.length > 0 && silent.length > 0 ? (
        <Text as="p" variant="caption" tone="secondary">
          {silent.map((n) => `${n.node || "A node"} did not answer: ${n.reason}`).join(" ")}
        </Text>
      ) : null}
      {first?.said ? (
        <Disclosure title="What the engine recorded" size="compact" headingLevel="none" flush>
          <Text as="p" variant="caption" tone="secondary" className="mono">
            {first.said}
          </Text>
        </Disclosure>
      ) : null}
    </div>
  );
}

/** A test that is running, in one line. */
export function Testing() {
  return (
    <div className="mcp-testing">
      <Spinner size="sm" aria-label="Testing" />
      <Text as="span" tone="secondary">
        Asking the server who it is and listing its tools.
      </Text>
    </div>
  );
}

export function TestDialog({
  server,
  seat,
  data,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  /** The agent the address names, for a per-agent server; "" for the first it reaches. */
  seat: string;
  data: McpData;
  onClose: () => void;
  onDone: () => void;
}) {
  const reach = server.shared ? [] : (data.reaches(server.name) ?? []);
  const [as, setAs] = useState(
    () => (seat && reach.some((s) => s.handle === seat) ? seat : reach[0]?.handle) ?? "",
  );
  const [answer, setAnswer] = useState<McpCheckAnswer | null>(null);
  const [error, setError] = useState<string | null>(null);
  // A TEST RUNS ON OPEN, ON "TEST AGAIN" AND ON A NEW AGENT, AND NOTHING ELSE
  // STARTS ONE. The effect is keyed on the attempt and the agent, never on
  // `onDone`: the caller's `onDone` reloads the page's data, which hands this
  // dialog a new callback, and an effect keyed on it would test again after
  // every reload.
  const [attempt, setAttempt] = useState(0);
  const tested = useEffectEvent(() => onDone());
  const nobody = !server.shared && as === "";
  useEffect(() => {
    if (nobody) return;
    let live = true;
    setAnswer(null);
    setError(null);
    void testServer(server.name, server.shared ? "" : as).then((next) => {
      if (!live) return;
      if (next.kind === "ok") {
        setAnswer(next.value);
        tested();
      } else {
        setError(next.sentence);
      }
    });
    return () => {
      live = false;
    };
  }, [server.name, server.shared, as, attempt, nobody]);
  const running = !nobody && !answer && !error;
  return (
    <Modal
      open
      onClose={onClose}
      title={`Test ${server.title}`}
      subtitle={
        server.shared
          ? "Run on each node, as the company."
          : "Run as one agent, on the node that holds it, with its own credentials."
      }
      size="md"
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
          <Button
            variant="primary"
            onClick={() => setAttempt((n) => n + 1)}
            disabled={running || nobody}
          >
            Test again
          </Button>
        </>
      }
    >
      <div className="mcp-stack">
        {reach.length > 1 ? (
          <FormField label="Test as">
            {(field) => (
              <Select
                id={field.id}
                value={as}
                disabled={running}
                options={reach.map((s) => ({ value: s.handle, label: s.name }))}
                onChange={(v) => setAs(String(v))}
              />
            )}
          </FormField>
        ) : null}
        {nobody ? (
          <Callout variant="info" title="No agent has this server">
            It runs one instance per agent, and no agent is given it yet, so there is nobody to test
            it as.
          </Callout>
        ) : error ? (
          <Callout variant="danger" title="The test did not run">
            {error}
          </Callout>
        ) : answer ? (
          <TestResult answer={answer} />
        ) : (
          <Testing />
        )}
      </div>
    </Modal>
  );
}
