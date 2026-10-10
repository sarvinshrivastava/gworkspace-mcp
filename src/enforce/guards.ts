/**
 * Argument guards that apply to EVERY agent, confined or not.
 *
 * The calendar tools are an outbound channel: an event can address arbitrary
 * people, and a document containing injected instructions — one shared with
 * this account by anyone — would otherwise turn a read tool and a calendar
 * tool into mail-it-to-me.
 */

export type GuardResult =
  | { args: Record<string, unknown>; mutated: boolean }
  | { deny: "quickadd_guest" | "calendar_not_primary" };

export const ID_RE = /^[A-Za-z0-9_-]{10,128}$/;
export const EMAIL_RE = /[^\s@<>()]+@[^\s@<>()]+\.[^\s@<>()]+/;

const forcedNone = (value: unknown) => value !== undefined && value !== "none";

/**
 * createEvent and updateEvent: attendees[].email is an arbitrary address and
 * description is arbitrary text. The event is still created; it just cannot
 * address a stranger, and no invitation is sent.
 */
export function stripAttendees(args: Record<string, unknown>): GuardResult {
  const { attendees, ...rest } = args;
  if (attendees !== undefined) {
    console.warn("[guard] dropped attendees from a calendar call");
  }
  return {
    args: { ...rest, sendUpdates: "none" },
    mutated: attendees !== undefined || forcedNone(args.sendUpdates),
  };
}

/**
 * quickAddEvent: Google parses its free text, and "lunch with a@b.co" makes
 * a@b.co a guest — the same channel as createEvent's attendees, through a
 * field that cannot be stripped without changing the event.
 */
export function quickAddGuard(args: Record<string, unknown>): GuardResult {
  if (typeof args.text === "string" && EMAIL_RE.test(args.text)) {
    return { deny: "quickadd_guest" };
  }
  return { args: { ...args, sendUpdates: "none" }, mutated: forcedNone(args.sendUpdates) };
}

/**
 * Writes go to this account's own calendar only. Any other calendarId is a
 * calendar someone may have shared with this account with edit rights — and
 * an event written there, description and all, is theirs to read.
 */
function primaryOnly(guard: (args: Record<string, unknown>) => GuardResult) {
  return (args: Record<string, unknown>): GuardResult =>
    args.calendarId === undefined || args.calendarId === "primary"
      ? guard(args)
      : { deny: "calendar_not_primary" };
}

export const ALL_AGENT_GUARDS: Record<string, (args: Record<string, unknown>) => GuardResult> = {
  createEvent: primaryOnly(stripAttendees),
  updateEvent: primaryOnly(stripAttendees),
  quickAddEvent: primaryOnly(quickAddGuard),
};

/** updateEvent fields whose text reaches every existing guest's calendar. */
export const EVENT_CONTENT_FIELDS = ["summary", "description", "location"] as const;
