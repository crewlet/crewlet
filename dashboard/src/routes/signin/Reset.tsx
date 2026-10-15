/**
 * `#/reset/<id>.<secret>` — choosing a password from the one-time link an
 * administrator sent: a RESET for somebody who has a password, or the FIRST
 * password of a person an administrator created on a seat.
 *
 * # It is the invitation's screen again, for the invitation's reasons
 *
 * The engine mints `<external_url>/dashboard#/reset/<id>.<secret>`, the whole
 * credential in the FRAGMENT, which no browser sends to a server. The id goes
 * in the route's path and the secret BESIDE it — the `X-Crewlet-Reset-Secret`
 * header for the view, the body for the spend (`protocol/auth.ts`) — never in
 * a request URL. Opening it spends nothing, because a mail client prefetching
 * or a preview card follows the same URL; only the form's POST sets a password.
 *
 * # One answer for every link that no longer works
 *
 * Spent, revoked, past its deadline, a secret that is not the link's, an id
 * nobody issued, a person suspended since: the engine answers all of them
 * `410` in the same bytes, so this screen has one sentence for them too — the
 * engine's — and one remedy: ask an administrator for a new link. It names no
 * lifetime: a reset lasts a day and a first password link a week, and a link
 * that no longer opens does not say which it was.
 *
 * # A first password is not a reset
 *
 * The view says which (`first`, `contract/identity.ts`). A reset replaces a
 * password and ends every session its person held; a first password ends
 * nothing, because there was nothing to end — and told that it would, a person
 * just created on their seat read their first sign-in as a lock-out.
 *
 * # It signs nobody in
 *
 * Setting the password opens no session: they sign in next, where a second
 * factor they hold still applies — which a session handed out by the link
 * would skip — and a reset ends every session they held, on every device. So
 * the screen ends on a button to the sign-in form rather than in the product.
 */

import { useCallback, useEffect, useState } from "react";
import { Button, Callout, EmptyState, Skeleton } from "@crewlethq/ui";
import { CompassGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import type { ResetView } from "~/contract/identity.ts";
import { auth, RestError } from "~/protocol/index.ts";
import { parseLink } from "./link.ts";
import { NewPasswordFields, newPasswordReady } from "./NewPassword.tsx";
import { SignInPage } from "./SignInPage.tsx";

type Read =
  | { state: "loading" }
  | { state: "ready"; view: ResetView }
  | { state: "spent"; sentence: string }
  | { state: "failed"; sentence: string }
  | { state: "set"; login: string; first: boolean };

export function Reset({ link }: { link: string }) {
  const parsed = parseLink(link);
  if (!parsed) {
    return (
      <SignInPage title="This password link is incomplete">
        <EmptyState
          icon={<CompassGlyph size={32} />}
          title="Part of the link is missing."
          description="A password link ends in two parts joined by a dot. Copy the whole link from the message it came in, or ask an administrator for a new one."
          headingLevel="none"
        />
      </SignInPage>
    );
  }
  return <ResetLink id={parsed.id} secret={parsed.secret} />;
}

function ResetLink({ id, secret }: { id: string; secret: string }) {
  const [read, setRead] = useState<Read>({ state: "loading" });

  const load = useCallback(() => {
    setRead({ state: "loading" });
    let live = true;
    auth.viewReset(id, secret).then(
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
        <SignInPage title="Opening your password link">
          <Skeleton variant="text" rows={3} label="Reading the password link" />
        </SignInPage>
      );
    case "spent":
      return <Spent sentence={read.sentence} />;
    case "failed":
      return (
        // NOTHING WAS DECIDED ABOUT THE LINK, so it is not called bad.
        <SignInPage title="Your password link could not be read">
          <Callout
            variant="warning"
            role="alert"
            action={
              <Button size="small" variant="secondary" onClick={load}>
                Try again
              </Button>
            }
          >
            {read.sentence} Nothing about the link has been decided — it has not been used.
          </Callout>
        </SignInPage>
      );
    case "set":
      return <PasswordIsSet login={read.login} first={read.first} />;
    case "ready":
      return (
        <Choose
          id={id}
          secret={secret}
          view={read.view}
          onSpent={(sentence) => setRead({ state: "spent", sentence })}
          onSet={(login) => setRead({ state: "set", login, first: read.view.first === true })}
        />
      );
  }
}

function Spent({ sentence }: { sentence: string }) {
  const nav = useNavigator();
  return (
    <SignInPage title="This password link can no longer be used">
      <EmptyState
        icon={<KeyGlyph size={32} />}
        title={sentence}
        description="A password link works once, and for a limited time. If you already set a password with it, sign in with that password; otherwise ask an administrator for a new link."
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

function PasswordIsSet({ login, first }: { login: string; first: boolean }) {
  const nav = useNavigator();
  return (
    <SignInPage title="Your password is set">
      <EmptyState
        icon={<KeyGlyph size={32} />}
        title={`Sign in as ${login} with your ${first ? "" : "new "}password.`}
        description={
          first
            ? "This link is spent: from now on your login and this password are how you sign in."
            : "Every session you held has ended, on every device. A second factor you hold is still asked for."
        }
        headingLevel="none"
        action={
          // WITH THE LOGIN, AND SAYING WHERE THEY CAME FROM: the sign-in was a
          // blank form under "Use the login or email address your invitation
          // was for", to somebody who had just been told which login to use.
          <Button variant="primary" onClick={() => nav.to(["login"], { login, after: "reset" })}>
            Sign in
          </Button>
        }
      />
    </SignInPage>
  );
}

function Choose({
  id,
  secret,
  view,
  onSpent,
  onSet,
}: {
  id: string;
  secret: string;
  view: ResetView;
  onSpent: (sentence: string) => void;
  onSet: (login: string) => void;
}) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  const floor = view.min_password_length;

  async function submit() {
    setTried(true);
    if (busy || !newPasswordReady(password, confirm, floor)) return;
    setBusy(true);
    setRefusal(null);
    try {
      const answer = await auth.spendReset(id, { secret, password });
      onSet(answer.login || view.login);
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

  return (
    <SignInPage
      title={view.first ? "Choose your password" : "Choose a new password"}
      lede={
        view.first ? (
          <>
            This link sets the first password for <strong>{view.login}</strong>, once. You sign in
            with it afterwards.
          </>
        ) : (
          <>
            This link sets the password for <strong>{view.login}</strong>, once. Setting it ends
            every session you hold, and you sign in with it afterwards.
          </>
        )
      }
    >
      <form
        className="signin-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        {/* FOR A PASSWORD MANAGER, which files a new password under the
            field named `username` beside it. */}
        <input
          hidden
          readOnly
          type="text"
          name="username"
          autoComplete="username"
          value={view.login}
        />
        <NewPasswordFields
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
          {busy ? "Setting the password" : "Set password"}
        </Button>
      </form>
    </SignInPage>
  );
}
