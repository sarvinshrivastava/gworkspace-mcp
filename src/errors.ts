/** The message of anything thrown, for a log line or a user-facing failure. */
export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
