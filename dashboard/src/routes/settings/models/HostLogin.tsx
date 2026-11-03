/**
 * A model that signs in with its harness's own login on a node: each node's login
 * as the harness's status command last reported it, the command that makes one
 * there, and a check of the node that answers.
 *
 * NEVER A SIGN-IN OF OUR OWN. The person runs the command on the node and
 * finishes the vendor's sign-in in their browser; the dashboard never sees the
 * login, only whether the harness says it holds one. Shared by the model's page
 * (its Sign-in card, which a host sign-in action opens rather than the edit
 * wizard: design QA Minor 12) and the wizard's Method step.
 */

import { useState } from "react";
import { Button, Copyable, RelativeTime, StatusDot, Text, useToast } from "@crewlethq/ui";
import { listed } from "~/lib/format.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { HostLoginAnswer, ModelHostLogin } from "~/protocol/models.ts";
import { checkHostLogin } from "./api.ts";

type Tone = "success" | "danger" | "neutral";

/** One node's login in a word, and its tone. */
function loginWords(login: Pick<ModelHostLogin, "node" | "state" | "account">): {
  tone: Tone;
  label: string;
} {
  if (login.state === "signed_in")
    return {
      tone: "success",
      label: `Signed in on ${login.node}${login.account ? ` as ${login.account}` : ""}`,
    };
  if (login.state === "signed_out")
    return { tone: "danger", label: `Not signed in on ${login.node}` };
  return { tone: "neutral", label: `Not known on ${login.node}` };
}

export function HostLogin({
  modelKey,
  logins,
  onChecked,
}: {
  /** The model whose login is checked: it must exist in the active document. */
  modelKey: string;
  /** Each node's login as the model's reading carries it (`sign_in.host_logins`). */
  logins: readonly ModelHostLogin[];
  /** Called after a check answered, so the reading is asked again. */
  onChecked: () => void;
}) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [checked, setChecked] = useState<HostLoginAnswer | null>(null);
  const lifetime = useLifetimeSignal();

  async function check() {
    // TIED TO THE CARD: a check that answers after the page moved on neither
    // toasts nor asks the page again.
    const signal = lifetime();
    setBusy(true);
    let answer: Awaited<ReturnType<typeof checkHostLogin>>;
    try {
      answer = await checkHostLogin(modelKey, signal);
    } catch (err) {
      if (isAbort(err)) return;
      throw err;
    }
    if (signal.aborted) return;
    setBusy(false);
    if (answer.kind === "refused") {
      toast.failed(answer.sentence);
      return;
    }
    setChecked(answer.value);
    toast.ok(
      answer.value.state === "signed_in"
        ? `Signed in on ${answer.value.node}.`
        : `Still not signed in on ${answer.value.node}.`,
    );
    onChecked();
  }

  // THE CHECK'S ANSWER IS NEWER than the node's last report until the reading
  // catches up, so it replaces that node's line.
  const lines: ModelHostLogin[] = logins.map((login) =>
    checked && login.node === checked.node ? { ...login, ...checked } : login,
  );
  if (checked && !lines.some((l) => l.node === checked.node)) lines.push(checked);
  const sharing = checked?.shares_login_with.filter((k) => k !== modelKey) ?? [];

  return (
    <div className="models-host">
      {lines.length === 0 ? (
        <Text as="p" variant="body" tone="secondary">
          No node has reported this login yet. Check it to ask the node that answers.
        </Text>
      ) : (
        <ul className="models-host__nodes" aria-label="The login on each node">
          {lines.map((login) => {
            const words = loginWords(login);
            return (
              <li key={login.node} className="models-host__node">
                <StatusDot tone={words.tone}>{words.label}</StatusDot>
                {login.checked_at ? (
                  <Text as="span" variant="caption" tone="tertiary">
                    Checked <RelativeTime value={login.checked_at} />.
                  </Text>
                ) : null}
                {login.state !== "signed_in" ? (
                  <>
                    {login.detail ? (
                      <Text as="span" variant="caption" tone="secondary" mono>
                        {login.detail}
                      </Text>
                    ) : null}
                    <Text as="p" variant="body">
                      Run this in a terminal on {login.node}, then finish the vendor's own sign-in
                      in your browser. The dashboard never sees the login.
                    </Text>
                    <Copyable
                      value={login.command}
                      variant="block"
                      monospace
                      ariaLabel={`Copy the login command for ${login.node}`}
                      className="models-copy-block"
                    />
                  </>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
      {sharing.length > 0 ? (
        <Text as="p" variant="caption" tone="secondary">
          One login also signs in the models {listed(sharing)}: they keep their login in the same
          folder.
        </Text>
      ) : null}
      <Button size="small" variant="secondary" loading={busy} onClick={() => void check()}>
        Check again
      </Button>
    </div>
  );
}
