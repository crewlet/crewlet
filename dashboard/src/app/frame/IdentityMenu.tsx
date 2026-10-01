/**
 * Who the frame thinks you are, in the page bar — and what you can do about
 * your own session from there.
 *
 * THREE STATES, three different things to draw (see `lib/viewer.ts`): a
 * bound person is their seat's badge and name; an unbound credential is its
 * login, in the mono face, because it is a machine value an operator compares
 * character by character; and nobody at all is a way to sign in. Neutral in
 * every case: colour carries state and never identity, and "who you are" is
 * not a state.
 *
 * # What the menu holds
 *
 * Your seat's page, when you hold one. Your second factor and your recovery
 * codes — only for a PERSON signed in with a session, which `GET
 * /auth/session` says: a machine credential holds no second factor, and
 * offering it a dialog the engine refuses would be a control that lies. And
 * signing out, here or everywhere: the two ways a session ends by its owner's
 * hand, both of which end in a reload at the sign-in (`lib/session.ts`).
 *
 * # Signing out does not wait for the socket
 *
 * The viewer is a SOCKET question, and a person the socket refuses — a seat
 * taken out of the chart (`403 seat_unavailable`), a session without
 * `state:read` — never has it answered: the socket stops dialling on a
 * refusal. The menu used to draw nothing until it was, so exactly the people
 * the engine had shut out were left with no way to end their own session;
 * the engine leaves `/auth/` open to them for that reason. So `GET
 * /auth/session` is asked whatever the viewer says, and while the viewer has
 * not answered, a session it names gets a menu of the two sign-outs under its
 * own login — nothing about a seat or a factor, which that answer does not
 * settle.
 */

import { useState } from "react";
import { Avatar, Button, Menu, useToast, type MenuEntry } from "@crewlethq/ui";
import { PersonGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { adoptReader } from "~/lib/reader.ts";
import { useRestRead } from "~/lib/restRead.ts";
import { goSignIn, signOut, signOutEverywhere } from "~/lib/session.ts";
import { useViewer } from "~/lib/viewer.ts";
import { auth, type SessionAnswer } from "~/protocol/index.ts";
import { AuthenticatorDialog, RecoveryCodesDialog } from "~/routes/signin/SecondFactor.tsx";

/**
 * The session behind this browser, as the sign-in surface describes it, or
 * null until it has answered — or if it cannot, in which case nothing that
 * depends on it is offered.
 *
 * ASKED UNLESS THE VIEWER HAS ANSWERED NOBODY — before it has answered too,
 * see the note above — and again when the viewer's login moves, which is a
 * different session.
 *
 * AND ASKED AGAIN ON ITS OWN WHEN NOTHING ANSWERED IT, because it is the
 * shared REST read (`~/lib/restRead.ts`). It was read once and every failure
 * dropped, and for the people the note above is about — the ones the socket
 * refuses — this read is the ONLY way to the sign-outs: one request past its
 * deadline, or lost on the way, left them with no menu at all until a reload.
 * A refusal is still an answer and is not asked again on a timer: a browser
 * holding no session is told so by the engine.
 */
function useSessionAnswer(enabled: boolean, login: string): SessionAnswer | null {
  return useRestRead(
    // A DIFFERENT LOGIN IS A DIFFERENT SESSION, so it is a different question
    // and starts from nothing.
    `/auth/session as ${login}`,
    async (signal) => {
      const session = await auth.session(signal);
      // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER learns who it is
      // read by here, since no sign-in in it ever said (`lib/reader.ts`).
      adoptReader(session.person);
      return session;
    },
    { enabled },
  ).data;
}

export function IdentityMenu() {
  const viewer = useViewer();
  const nav = useNavigator();
  const toast = useToast();
  const session = useSessionAnswer(!viewer.anonymous, viewer.login);
  const [open, setOpen] = useState<"factor" | "codes" | null>(null);

  // A FAILED SIGN-OUT IS SAID, and the page stays: see `signOut` for why a
  // sign-out that nothing answered must not look like one that worked.
  const run = (gesture: () => Promise<void>, what: string) => () => {
    gesture().catch((err: unknown) =>
      toast.failed(`${what} did not go through. ${refusalText(err)}`),
    );
  };
  const signOuts: MenuEntry[] = [
    { key: "sign-out", label: "Sign out", onSelect: run(signOut, "Signing out") },
    {
      key: "sign-out-everywhere",
      label: "Sign out everywhere",
      description: "Ends every session you hold, on every device",
      onSelect: run(signOutEverywhere, "Signing out everywhere"),
    },
  ];

  if (viewer.loading) {
    // THE SESSION ANSWERED AND THE VIEWER HAS NOT — and the socket may never
    // answer it, for a person it refuses — so the two ways out are drawn under
    // the session's own login. Nothing answered at all is still nothing.
    if (!session) return null;
    return (
      <Menu
        label={`Signed in as ${session.login}`}
        align="end"
        triggerVariant="tertiary"
        icon={<PersonGlyph size="sm" />}
        trigger={<span className="viewer-name truncate mono">{session.login}</span>}
        items={signOuts}
      />
    );
  }
  if (viewer.anonymous) {
    return (
      <Button size="small" variant="secondary" onClick={goSignIn}>
        Sign in
      </Button>
    );
  }

  const person = session?.kind === "person" && session.expires_at !== undefined;
  const items: MenuEntry[] = [];
  if (viewer.handle) {
    items.push({
      key: "seat",
      label: "Your seat",
      description: `@${viewer.handle}`,
      onSelect: () => nav.to(["company", "people", viewer.handle]),
    });
    items.push({ kind: "separator", key: "after-seat" });
  } else {
    // UNBOUND IS AN ORDINARY STATE, not a fault — a pipeline, an operator
    // outside the chart — so it is said as a fact, with what would change it,
    // rather than drawn as a warning.
    items.push(
      {
        key: "unbound",
        label: "Not bound to a seat",
        description: "Your record is kept under your login; crewlet iam bind gives you a seat",
        disabled: true,
        onSelect: () => {},
      },
      { kind: "separator", key: "after-seat" },
    );
  }
  if (person) {
    items.push(
      {
        key: "factor",
        label: "Two-step verification…",
        description: "Add or replace your authenticator app",
        onSelect: () => setOpen("factor"),
      },
      {
        key: "codes",
        label: "New recovery codes…",
        description: "Retires the set you hold",
        onSelect: () => setOpen("codes"),
      },
      { kind: "separator", key: "after-proof" },
    );
  }
  items.push(...signOuts);

  const bound = viewer.handle !== "";
  return (
    <>
      <Menu
        label={`Signed in as ${viewer.login}`}
        align="end"
        triggerVariant="tertiary"
        icon={
          bound ? (
            // THE SAME BADGE THE REST OF THE PRODUCT DRAWS for a human seat —
            // `dashed` is "the engine does not run it" — in `brand`, because
            // this one badge IS the reader, which is the single use that
            // tone exists for.
            <Avatar
              name={viewer.name || viewer.handle}
              size="xs"
              tone="brand"
              variant="dashed"
              decorative
            />
          ) : (
            <PersonGlyph size="sm" />
          )
        }
        trigger={
          bound ? (
            <span className="viewer-name truncate">{viewer.name || viewer.handle}</span>
          ) : (
            <span className="viewer-name truncate mono">{viewer.login}</span>
          )
        }
        items={items}
      />
      {open === "factor" && <AuthenticatorDialog onClose={() => setOpen(null)} />}
      {open === "codes" && <RecoveryCodesDialog onClose={() => setOpen(null)} />}
    </>
  );
}
