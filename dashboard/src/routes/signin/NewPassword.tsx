/**
 * A new password, typed twice and held to the deployment's floor — the one
 * form every screen that sets a password draws: redeeming an invitation,
 * spending a reset link and changing your own on the Account page.
 *
 * TYPED TWICE because a password field shows nothing of what was typed, and a
 * slip in the only copy is an account its owner cannot sign in to: the screen
 * that set it is gone, and the way back is an administrator's reset link.
 *
 * THE FLOOR IS THE ENGINE'S (`min_password_length`), counted in runes as the
 * engine counts it, and checked here only so a short password is refused
 * before a round trip — the engine refuses it anyway, with the sentence that
 * says why. Where the floor is not known yet, nothing is checked here and the
 * engine's refusal is the answer.
 */

import { FormField, Input } from "@crewlethq/ui";
import { characters } from "./link.ts";

/** What is wrong with a new password typed twice — a sentence per field, or null. */
export function newPasswordProblems(
  password: string,
  confirm: string,
  floor: number | null,
): { password: string | null; confirm: string | null } {
  const length = characters(password);
  return {
    password:
      floor !== null && length < floor
        ? `This is ${length} characters, and the minimum is ${floor}.`
        : null,
    confirm: confirm !== password ? "The two passwords are not the same." : null,
  };
}

/** Whether a new password typed twice may be sent. */
export function newPasswordReady(password: string, confirm: string, floor: number | null): boolean {
  const problems = newPasswordProblems(password, confirm, floor);
  return password !== "" && problems.password === null && problems.confirm === null;
}

export function NewPasswordFields({
  label = "New password",
  floor,
  password,
  confirm,
  onPassword,
  onConfirm,
  tried,
}: {
  label?: string;
  /** The engine's floor, or null while it is not known. */
  floor: number | null;
  password: string;
  confirm: string;
  onPassword: (next: string) => void;
  onConfirm: (next: string) => void;
  /** Whether a submit was tried: a problem is said only once it was. */
  tried: boolean;
}) {
  const problems = newPasswordProblems(password, confirm, floor);
  const rule =
    floor === null
      ? "There are no rules beyond a minimum length; a long phrase is the strongest password there is."
      : `At least ${floor} characters. There are no other rules; a long phrase is the strongest password there is.`;
  return (
    <>
      <FormField
        label={label}
        helper={rule}
        error={tried ? (problems.password ?? undefined) : undefined}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            type="password"
            autoComplete="new-password"
            width="full"
            value={password}
            onChange={(e) => onPassword(e.target.value)}
          />
        )}
      </FormField>
      <FormField
        label={`Confirm ${label.toLowerCase()}`}
        error={tried ? (problems.confirm ?? undefined) : undefined}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            type="password"
            autoComplete="new-password"
            width="full"
            value={confirm}
            onChange={(e) => onConfirm(e.target.value)}
          />
        )}
      </FormField>
    </>
  );
}
