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
 */

import { useEffect, useState } from "react";
import { Avatar, Button, Menu, useToast, type MenuEntry } from "@crewlethq/ui";
import { PersonGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { goSignIn, signOut, signOutEverywhere } from "~/lib/session.ts";
import { useViewer } from "~/lib/viewer.ts";
import { auth, type SessionAnswer } from "~/protocol/index.ts";
import { AuthenticatorDialog, RecoveryCodesDialog } from "~/routes/signin/SecondFactor.tsx";

/**
 * The session behind this browser, as the sign-in surface describes it, or
 * null until it has answered — or if it cannot, in which case nothing that
 * depends on it is offered.
 *
 * ASKED ONLY OF SOMEBODY THE ENGINE RESOLVED, and again when that changes:
 * the viewer's login moving is a different session.
 */
function useSessionAnswer(enabled: boolean, login: string): SessionAnswer | null {
  const [answer, setAnswer] = useState<SessionAnswer | null>(null);
  useEffect(() => {
    setAnswer(null);
    if (!enabled) return;
    let live = true;
    auth.session().then(
      (session) => live && setAnswer(session),
      () => {},
    );
    return () => {
      live = false;
    };
  }, [enabled, login]);
  return answer;
}

export function IdentityMenu() {
  const viewer = useViewer();
  const nav = useNavigator();
  const toast = useToast();
  const session = useSessionAnswer(!viewer.loading && !viewer.anonymous, viewer.login);
  const [open, setOpen] = useState<"factor" | "codes" | null>(null);

  if (viewer.loading) return null;
  if (viewer.anonymous) {
    return (
      <Button size="small" variant="secondary" onClick={goSignIn}>
        Sign in
      </Button>
    );
  }

  // A FAILED SIGN-OUT IS SAID, and the page stays: see `signOut` for why a
  // sign-out that nothing answered must not look like one that worked.
  const run = (gesture: () => Promise<void>, what: string) => () => {
    gesture().catch((err: unknown) =>
      toast.failed(`${what} did not go through. ${refusalText(err)}`),
    );
  };

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
  items.push(
    { key: "sign-out", label: "Sign out", onSelect: run(signOut, "Signing out") },
    {
      key: "sign-out-everywhere",
      label: "Sign out everywhere",
      description: "Ends every session you hold, on every device",
      onSelect: run(signOutEverywhere, "Signing out everywhere"),
    },
  );

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
