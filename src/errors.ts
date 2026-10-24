/** The message of anything thrown, for a log line or a user-facing failure. */
export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Text that looks like a credential. Never logged: a line matching it is dropped or replaced. */
export const SECRETISH = /refresh_token|client_secret|access_token|Bearer\s|"token"/i;
