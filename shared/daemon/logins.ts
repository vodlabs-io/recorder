import type { PlatformLoginView } from "../types";

/**
 * expiredLogins lists the stored platform logins that need signing in again. It
 * is the one definition of "expired", shared by the Accounts tab's dot and the
 * tray menu so the two cannot disagree.
 */
export function expiredLogins(accounts: PlatformLoginView[]): PlatformLoginView[] {
  return accounts.filter((a) => !a.healthy);
}
