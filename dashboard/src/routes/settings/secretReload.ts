/**
 * Whether a secret write switched the running company to it.
 *
 * `PUT /secrets/{name}` and `DELETE /secrets/{name}` answer 200 once the
 * value is sealed or gone, and say in `reload` whether the re-activation that
 * makes the company read it landed (ADR-0070). A write whose re-activation
 * failed is stored and NOT in use, and a dialog that closed on the 200 told
 * the operator the opposite.
 */

/** The `reload` member of a secret write's answer, as the engine writes it. */
interface Reload {
  error?: string;
  detail?: string;
}

/**
 * Why the re-activation did not land, or null when it did, was not needed
 * (nothing reads the name), or the answer says nothing about it.
 */
export function reloadFailure(answer: unknown): string | null {
  if (!answer || typeof answer !== "object") return null;
  const reload = (answer as { reload?: Reload | null }).reload;
  if (!reload || !reload.error) return null;
  return reload.detail || reload.error;
}
