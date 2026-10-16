/**
 * `#/invite/<id>.<secret>` — joining a company from the link somebody sent.
 *
 * # The link is two halves, and only one of them may ever be in a URL
 *
 * The engine mints `<external_url>/dashboard#/invite/<id>.<secret>`, with the
 * whole credential in the FRAGMENT, which no browser sends to any server — so
 * neither half reaches a proxy's access log on the way to this page. This
 * screen keeps that promise on the way back out: the id goes in the route's
 * path, and the secret travels BESIDE it, in a header for the view and in the
 * body for the redemption (`protocol/auth.ts`). A secret in a query string
 * would put the credential in exactly the logs the fragment kept it out of.
 *
 * # Rendering it spends nothing
 *
 * The view is a GET the engine answers without spending the link, because a
 * mail client prefetching, a scanner or a preview card follows the same URL;
 * only the form's POST — a person, having chosen a password — redeems it.
 *
 * # One answer for every link that no longer works
 *
 * Redeemed, withdrawn, expired, a secret that is not the link's, an id nobody
 * issued: the engine answers all of them `410` in the same bytes, because
 * telling them apart would say which links exist to somebody guessing at
 * them, and "already used" to somebody whose link merely aged out. So this
 * screen has one sentence for all of them too — the engine's — and the remedy
 * that is right for every one: ask whoever sent it for a new one.
 *
 * # A refusal the person can fix keeps their form
 *
 * A login outside the grammar is said under the field before anything is
 * posted (`lib/login.ts`, the engine's grammar). A login somebody already
 * holds is `409`, shown in the engine's words above a form that keeps
 * everything typed, because a person told to choose another login should not
 * have to type their password again.
 * The redemption can be retried until it lands: it is one record, so an
 * attempt the engine refused wrote nothing and the next has nothing in its
 * way — what keeps the link single-use is the record that spends it.
 *
 * # A link whose seat is gone is said before anybody types a password
 *
 * Every person holds a human seat, so every invitation names the one its
 * redemption binds them to — and a redemption is refused when the chart no
 * longer holds it as a human seat. The view says which seat it is, or (a seat
 * with no handle) that the chart has none to name; this screen then shows
 * only that message, with no form, since a form that can only be refused is
 * wasted work.
 *
 * # A browser already signed in is told so
 *
 * The view names the login this browser's session is signed in as, and
 * redeeming ends that session before the new one opens — so the form says
 * both before Join is pressed. It switched a browser from one person to
 * another with no word, and left the first person's session live on the
 * engine for its whole deadline.
 */

import { useCallback, useEffect, useState } from "react";
import { Button, Callout, EmptyState, FormField, Input, Skeleton, Text } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { loginProblem } from "~/lib/login.ts";
import { handleLabel } from "~/lib/seats.ts";
import { LANDING, useSignedIn } from "~/lib/session.ts";
import { auth, RestError, type InvitationView } from "~/protocol/index.ts";
import { parseLink } from "./link.ts";
import { NewPasswordFields, newPasswordReady } from "./NewPassword.tsx";
import { SignInPage } from "./SignInPage.tsx";

type Read =
  | { state: "loading" }
  | { state: "ready"; view: InvitationView }
  | { state: "spent"; sentence: string }
  | { state: "failed"; sentence: string };

export function Invite({ link }: { link: string }) {
  const nav = useNavigator();
  const parsed = parseLink(link);
  if (!parsed) {
    return (
      <SignInPage title="This invitation link is incomplete" lede="Copy the whole link.">
        <div>
          <Button variant="secondary" onClick={() => nav.to(["login"])}>
            Go to sign in
          </Button>
        </div>
      </SignInPage>
    );
  }
  return <Invitation id={parsed.id} secret={parsed.secret} />;
}

function Invitation({ id, secret }: { id: string; secret: string }) {
  const [read, setRead] = useState<Read>({ state: "loading" });

  const load = useCallback(() => {
    setRead({ state: "loading" });
    let live = true;
    auth.viewInvite(id, secret).then(
      (view) => live && setRead({ state: "ready", view }),
      (err: unknown) => {
        if (!live) return;
        if (err instanceof RestError && err.status === 410) {
          setRead({ state: "spent", sentence: err.sentence || refusalText(err) });
        } else {
          setRead({ state: "failed", sentence: refusalText(err) });
        }
      },
    );
    return () => {
      live = false;
    };
  }, [id, secret]);

  useEffect(load, [load]);

  switch (read.state) {
    case "loading":
      return (
        <SignInPage title="Opening your invitation">
          <Skeleton variant="text" rows={4} label="Reading the invitation" />
        </SignInPage>
      );
    case "spent":
      return <Spent sentence={read.sentence} />;
    case "failed":
      return (
        // NOTHING WAS DECIDED ABOUT THE LINK: the engine could not be asked
        // or could not read its records, and saying the link is bad would
        // send somebody to ask for a new one they do not need.
        <SignInPage title="Your invitation could not be read">
          <Callout
            variant="warning"
            role="alert"
            action={
              <Button size="small" variant="secondary" onClick={load}>
                Try again
              </Button>
            }
          >
            {read.sentence} Not used. Try again.
          </Callout>
        </SignInPage>
      );
    case "ready":
      return (
        <Redeem
          id={id}
          secret={secret}
          view={read.view}
          onSpent={(sentence) => setRead({ state: "spent", sentence })}
        />
      );
  }
}

function Spent({ sentence }: { sentence: string }) {
  const nav = useNavigator();
  return (
    <SignInPage title="This invitation can’t be used">
      <EmptyState
        icon={<KeyGlyph size={32} />}
        title={sentence}
        description="Already joined? Sign in."
        headingLevel="none"
        action={
          <Button variant="secondary" onClick={() => nav.to(["login"])}>
            Sign in
          </Button>
        }
      />
    </SignInPage>
  );
}

function Redeem({
  id,
  secret,
  view,
  onSpent,
}: {
  id: string;
  secret: string;
  view: InvitationView;
  onSpent: (sentence: string) => void;
}) {
  const signedIn = useSignedIn();
  const nav = useNavigator();
  const [login, setLogin] = useState(view.login ?? "");
  // PREFILLED WITH THE SEAT'S NAME, which already names the person; they can
  // still change it.
  const [name, setName] = useState(view.seat?.name ?? "");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  const floor = view.min_password_length;
  const loginMissing = login.trim() === "";
  const loginWrong = loginProblem("person", login.trim());

  async function submit() {
    setTried(true);
    if (busy || !newPasswordReady(password, confirm, floor) || loginMissing || loginWrong) return;
    setBusy(true);
    setRefusal(null);
    try {
      const answer = await auth.redeemInvite(id, {
        secret,
        login: login.trim(),
        name: name.trim(),
        password,
      });
      signedIn(answer, LANDING);
    } catch (err) {
      if (err instanceof RestError && err.status === 410) {
        onSpent(err.sentence || refusalText(err));
        return;
      }
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  const seat = view.seat;
  const title = "Join your company on Crewlet";
  const lede = (
    <>
      {view.invited_by ? (
        <>
          <strong>{view.invited_by}</strong> invited you as{" "}
        </>
      ) : (
        "You’re invited as "
      )}
      <strong>{view.email}</strong>.
    </>
  );

  // THE ENGINE REFUSES THIS REDEMPTION, and says so only once it is posted:
  // the seat it binds is no longer a human seat of this node's chart. So the
  // form is not offered at all, since filling it in could only end in that
  // refusal; somebody who already joined is offered the sign-in instead.
  if (!seat?.handle) {
    return (
      <SignInPage title={title} lede={lede}>
        <Callout
          variant="warning"
          action={
            <Button size="small" variant="secondary" onClick={() => nav.to(["login"])}>
              Sign in
            </Button>
          }
        >
          This invitation’s seat was removed. Ask for a new one.
        </Callout>
      </SignInPage>
    );
  }

  return (
    <SignInPage title={title} lede={lede}>
      {view.signed_in_as && (
        <Callout variant="warning">
          Signed in as <strong>{view.signed_in_as}</strong>. Joining signs you out first.
        </Callout>
      )}
      <Callout variant="neutral">
        <Text as="span" variant="body">
          You’ll join as <strong>{seat.name || seat.handle}</strong> ({handleLabel(seat.handle)}).
        </Text>
      </Callout>
      <form
        className="signin-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        {/* FOR A PASSWORD MANAGER, which files a new password under the
            field named `username` beside it — and the address is one of
            the two things the sign-in form takes. */}
        <input
          hidden
          readOnly
          type="email"
          name="username"
          autoComplete="username"
          value={view.email}
        />
        <FormField
          label="Login"
          helper="Lowercase words with dots, like jane.doe."
          error={tried ? (loginMissing ? "Choose a login." : (loginWrong ?? undefined)) : undefined}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              autoComplete="off"
              spellCheck={false}
              width="full"
              value={login}
              onChange={(e) => setLogin(e.target.value)}
            />
          )}
        </FormField>
        <FormField label="Your name" optional helper="How your colleagues see you.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              autoComplete="name"
              width="full"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          )}
        </FormField>
        <NewPasswordFields
          label="Password"
          floor={floor}
          password={password}
          confirm={confirm}
          onPassword={setPassword}
          onConfirm={setConfirm}
          tried={tried}
        />
        {refusal && (
          <Callout variant="danger" role="alert">
            {refusal}
          </Callout>
        )}
        <Button type="submit" variant="primary" disabled={busy}>
          {busy ? "Joining" : "Join"}
        </Button>
      </form>
    </SignInPage>
  );
}
