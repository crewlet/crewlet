/**
 * The sign-in chunk: every screen `app/lazyScreen.ts` loads for the routes
 * drawn outside the frame. Nothing outside `app/` imports this file — a
 * static import of it from the frame would pull the sign-in screens back into
 * the entry chunk every signed-in reader downloads.
 */

export { SignIn } from "./SignIn.tsx";
export { Invite } from "./Invite.tsx";
export { Enrol } from "./Enrol.tsx";
// THE TWO PROOF DIALOGS a person opens from the sidebar's user block — in this
// chunk rather than the entry, because every signed-in reader carries the
// block and few of them ever open either.
export { AuthenticatorDialog, RecoveryCodesDialog } from "./SecondFactor.tsx";
