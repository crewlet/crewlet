/**
 * Sign the company in to a server's OAuth, or in again, and give the sign-in
 * up (`POST`, `GET /mcp-sign-ins/{id}`, `DELETE /mcp-servers/{name}/oauth`).
 *
 * THE PERSON CONSENTS AT THE VENDOR, in a tab of their own: the engine answers
 * the authorization address once, the tab is opened on it, and the vendor
 * sends the browser back to the engine, which finishes the sign-in. The dialog
 * only reads where the sign-in is until it is approved or failed. The tab is
 * opened blank on the click itself and pointed once the address is answered,
 * so a browser's popup rule (a tab opened by a click, not by an answer) lets
 * it through; where it is held anyway, the address is offered as a link.
 *
 * NOTHING HERE IS A TOKEN. The engine keeps the grant sealed and answers the
 * sign-in's state alone; the address is the person's own consent page, kept
 * only while this dialog is open.
 *
 * A RECENT STEP-UP is asked by the engine for both gestures, which the
 * dashboard's one ceremony answers and replays.
 */

import { useEffect, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  ConfirmModal,
  Copyable,
  FormField,
  Modal,
  Spinner,
  Text,
  useToast,
} from "@crewlethq/ui";
import type { McpMemberServerStatus, McpOAuthView, McpSignIn } from "~/contract/mcp.ts";
import { useLatestHandlers } from "~/lib/useLatestHandlers.ts";
import { readSignIn, signOut, startSignIn } from "../api.ts";
import { clientNoun, signOutWords } from "../words.ts";

/**
 * How often an open dialog reads where its sign-in is. The vendor sends the
 * browser back within a second of the person's consent, and one read of a
 * coordination record a second, for the fifteen minutes a sign-in can take at
 * most, is what lets the dialog say "connected" the moment it is.
 */
export const SIGN_IN_POLL_MS = 1000;

type Stage =
  | { kind: "idle" }
  | { kind: "starting" }
  /** Started: `url` is the consent page, offered as a link where no tab could be opened. */
  | { kind: "waiting"; signIn: McpSignIn; url: string; blocked: boolean }
  | { kind: "approved" }
  | { kind: "failed"; detail: string };

export function SignInDialog({
  server,
  oauth,
  callbackUrl,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  oauth: McpOAuthView;
  /** Where the vendor sends the browser back, for a vendor that allowlists it. */
  callbackUrl: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [stage, setStage] = useState<Stage>({ kind: "idle" });
  // WHAT THE DIALOG WAS OPENED TO DO, read once: the sign-in it completes
  // turns the live status to connected, and a title read from that renamed
  // the dialog at the moment it succeeded.
  const [again] = useState(() => oauth.status === "connected");
  const go = useRef<HTMLButtonElement>(null);
  // The caller's reload, newest each render, without restarting the reads.
  const on = useLatestHandlers({ done: onDone });

  const waitingFor = stage.kind === "waiting" ? stage.signIn.id : null;
  useEffect(() => {
    if (!waitingFor) return;
    const controller = new AbortController();
    const timer = window.setInterval(() => {
      void readSignIn(waitingFor, controller.signal)
        .then((answer) => {
          if (answer.kind !== "ok") {
            setStage({ kind: "failed", detail: answer.sentence });
            return;
          }
          if (answer.value.status === "approved") {
            setStage({ kind: "approved" });
            on.done();
          } else if (answer.value.status === "failed") {
            setStage({ kind: "failed", detail: answer.value.detail });
          }
        })
        .catch(() => {});
    }, SIGN_IN_POLL_MS);
    return () => {
      window.clearInterval(timer);
      controller.abort();
    };
  }, [waitingFor, on]);

  const start = async () => {
    // OPENED ON THE CLICK, pointed once answered: see the package doc.
    const tab = window.open("about:blank", "_blank");
    setStage({ kind: "starting" });
    const answer = await startSignIn(server.name);
    if (answer.kind !== "ok") {
      tab?.close();
      setStage({ kind: "failed", detail: answer.sentence });
      return;
    }
    const url = answer.value.authorize_url;
    if (tab) {
      tab.opener = null;
      tab.location.href = url;
    }
    setStage({ kind: "waiting", signIn: answer.value, url, blocked: !tab });
  };

  const scopes = oauth.scopes.length ? oauth.scopes.join(" and ") : "the server's default scopes";
  const client = clientNoun(oauth.client);
  return (
    <Modal
      open
      onClose={onClose}
      title={again ? `Sign in to ${server.title} again` : `Sign in to ${server.title}`}
      size="md"
      initialFocus={() => go.current}
      footer={
        stage.kind === "approved" ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="secondary" onClick={onClose}>
              {stage.kind === "waiting" ? "Close" : "Cancel"}
            </Button>
            {stage.kind === "waiting" ? null : (
              <Button
                ref={go}
                variant="primary"
                loading={stage.kind === "starting"}
                onClick={() => void start()}
              >
                {stage.kind === "failed" ? "Try again" : `Continue to ${server.title}`}
              </Button>
            )}
          </>
        )
      }
    >
      <div className="mcp-stack">
        {stage.kind === "approved" ? (
          <Callout variant="success" title={`${server.title} is connected`}>
            The agents use this sign-in for the company, and the engine renews it before it ends.
          </Callout>
        ) : stage.kind === "waiting" ? (
          <>
            <div className="mcp-testing">
              <Spinner size="sm" aria-label="Waiting" />
              <Text as="span" tone="secondary">
                Waiting for {server.title} to confirm the sign-in.
              </Text>
            </div>
            {stage.blocked ? (
              <Callout variant="info" title="Your browser kept the sign-in from opening">
                Open it here to continue at {server.title}.
              </Callout>
            ) : null}
            <ButtonLink
              href={stage.url}
              target="_blank"
              rel="noopener noreferrer"
              variant="secondary"
              size="small"
            >
              Open {server.title}'s sign-in
            </ButtonLink>
          </>
        ) : (
          <>
            {stage.kind === "failed" ? (
              <Callout variant="danger" title="The sign-in did not finish">
                {stage.detail || `${server.title} did not confirm it.`}
              </Callout>
            ) : null}
            <Text as="p">
              You sign in at {server.title} and approve the access it asks for. The engine keeps the
              grant, sealed, and the agents use it for the company: what they do there is done as
              the account you sign in with.
            </Text>
            <Text as="p" variant="caption" tone="secondary">
              {client ? `Signing in as ${client}, asking for ${scopes}.` : `Asking for ${scopes}.`}
            </Text>
            <FormField
              as="fieldset"
              label="Callback address"
              helper="For a vendor that allowlists redirects."
            >
              <Copyable value={callbackUrl} variant="block" monospace />
            </FormField>
          </>
        )}
      </div>
    </Modal>
  );
}

/**
 * Give a server's sign-in up: revoked at the vendor where it offers that, and
 * forgotten here whatever it said. The server's settings and levels stay.
 */
export function RevokeDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  return (
    <ConfirmModal
      open
      onClose={onClose}
      submitting={busy}
      onConfirm={async () => {
        setBusy(true);
        const answer = await signOut(server.name);
        if (answer.kind === "ok") {
          toast.show(signOutWords(server.title, answer.value));
          onDone();
          onClose();
          return;
        }
        toast.show({ variant: "danger", title: "Nothing was revoked", message: answer.sentence });
        setBusy(false);
      }}
      title={`Revoke ${server.title}'s sign-in?`}
      message={`The engine revokes the grant at ${server.title} and forgets it. Its tools stop answering until somebody signs in again; the server's settings and levels are kept.`}
      confirmLabel="Revoke"
      destructive
    />
  );
}
