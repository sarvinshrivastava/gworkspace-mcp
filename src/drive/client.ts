/**
 * A minimal Drive v3 client for the gateway's own lookups: a file's parents,
 * and folder-constrained listings.
 *
 * Plain fetch rather than googleapis. The child already pays ~200 MB for
 * googleapis' six API clients (learnings: "Two Node processes, one cgroup");
 * the parent needs two endpoints and an OAuth refresh.
 */

import { errorMessage } from "../errors.js";

export type DriveFile = {
  id: string;
  name: string;
  mimeType: string;
  parents: string[];
  trashed: boolean;
};

export type DriveCreds = { clientId: string; clientSecret: string; refreshToken: string };

export const FOLDER_MIME = "application/vnd.google-apps.folder";

const TOKEN_URL = "https://oauth2.googleapis.com/token";
const DRIVE_URL = "https://www.googleapis.com/drive/v3/files";
const CALENDAR_URL = "https://www.googleapis.com/calendar/v3/calendars";
const RATE_LIMIT_REASONS = new Set(["rateLimitExceeded", "userRateLimitExceeded"]);

/**
 * `retryable` separates "Drive could not answer right now" (rate limits, 5xx,
 * network) from "Drive answered no". The first must never be shown to a model
 * as "outside your scope", and must never be cached.
 */
export type ErrorSource = "oauth" | "drive" | "calendar";

/**
 * `rejection` is set when Google answered with a standing "no" that retrying
 * cannot fix (a dead refresh token, a disabled API, a missing scope). `source`
 * says which call it came from, so a Calendar rejection is never read as a
 * dead credential.
 */
export class DriveError extends Error {
  constructor(
    public status: number,
    public retryable: boolean,
    message: string,
    public rejection?: string,
    public source: ErrorSource = "drive",
  ) {
    super(message);
  }
}

/** The rejection as shown to people: an OAuth category stands alone, an API one names its service. */
export function rejectionDetail(error: DriveError): string {
  return error.source === "oauth" ? String(error.rejection) : `${error.source} ${error.rejection}`;
}

/**
 * The one place a failure's short category is derived. Never the message
 * itself, so a hostile or secret-bearing message cannot reach a push.
 */
export function categoryOf(error: unknown): string {
  if (error instanceof DriveError) {
    if (error.rejection) return rejectionDetail(error);
    if (error.status >= 400) return `HTTP ${error.status}`;
  }
  const name = error instanceof Error ? error.name : "";
  const message = errorMessage(error);
  if (name === "AbortError" || name === "TimeoutError" || /abort|timeout/i.test(message)) return "timeout";
  if (/not ready/.test(message)) return "unavailable";
  return "network";
}

/** The OAuth `error` code of a failed refresh. `error_description` is never read: it can echo the request. */
async function oauthCategory(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body.error === "string" && /^[a-z_]{1,40}$/.test(body.error)) return body.error;
  } catch {
    // Not JSON; the status alone describes it.
  }
  return `HTTP ${res.status}`;
}

/** Google's service-level 403s: the API is off, or the grant lacks the scope. Not a per-file denial. */
function serviceRejection(reasons: string[], message: string): string | null {
  if (reasons.some((r) => r === "accessNotConfigured" || r === "SERVICE_DISABLED")) return "accessNotConfigured";
  if (reasons.includes("ACCESS_TOKEN_SCOPE_INSUFFICIENT") || /authentication scopes/.test(message)) {
    return "insufficientPermissions";
  }
  return null;
}

export class DriveClient {
  private accessToken: { value: string; expiresAt: number } | null = null;
  private refreshing: Promise<string> | null = null;

  constructor(
    private readonly creds: DriveCreds,
    private readonly fetchImpl: typeof fetch = fetch,
    private readonly timeoutMs = 10_000,
    private readonly now: () => number = Date.now,
    private readonly hooks: { onOk?: (startedAt: number) => void } = {},
  ) {}

  private signal(outer?: AbortSignal): AbortSignal {
    const own = AbortSignal.timeout(this.timeoutMs);
    return outer ? AbortSignal.any([own, outer]) : own;
  }

  /** One refresh in flight at a time; every concurrent caller awaits it. */
  private token(): Promise<string> {
    if (this.accessToken && this.accessToken.expiresAt > this.now()) {
      return Promise.resolve(this.accessToken.value);
    }
    this.refreshing ??= this.refresh().finally(() => {
      this.refreshing = null;
    });
    return this.refreshing;
  }

  private async refresh(): Promise<string> {
    let res: Response;
    try {
      res = await this.fetchImpl(TOKEN_URL, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({
          grant_type: "refresh_token",
          client_id: this.creds.clientId,
          client_secret: this.creds.clientSecret,
          refresh_token: this.creds.refreshToken,
        }),
        signal: this.signal(),
      });
    } catch (error) {
      throw new DriveError(0, true, `token refresh failed: ${errorMessage(error)}`, undefined, "oauth");
    }
    // Only `error` is read, and only as a bare code: `error_description` can echo the request.
    if (!res.ok) {
      const transient = res.status >= 500 || res.status === 429 || res.status === 408;
      throw new DriveError(
        res.status,
        transient,
        `token refresh failed: HTTP ${res.status}`,
        transient ? undefined : await oauthCategory(res),
        "oauth",
      );
    }
    const body = (await res.json()) as { access_token?: string; expires_in?: number };
    if (!body.access_token) throw new DriveError(res.status, true, "token refresh returned no token", undefined, "oauth");
    this.accessToken = {
      value: body.access_token,
      expiresAt: this.now() + ((body.expires_in ?? 3600) - 60) * 1000,
    };
    return body.access_token;
  }

  private async request(
    url: string,
    source: ErrorSource,
    signal?: AbortSignal,
    retried = false,
    startedAt = this.now(),
  ): Promise<Response> {
    const token = await this.token();
    let res: Response;
    try {
      res = await this.fetchImpl(url, {
        headers: { Authorization: `Bearer ${token}` },
        signal: this.signal(signal),
      });
    } catch (error) {
      throw new DriveError(0, true, `Drive unreachable: ${errorMessage(error)}`, undefined, source);
    }
    if (res.status === 401) {
      this.accessToken = null;
      if (!retried) return this.request(url, source, signal, true, startedAt);
      throw new DriveError(401, true, "Drive rejected a freshly refreshed token", undefined, source);
    }
    // Only Drive's own answers say the credential works; Calendar's never do.
    const answered = () => {
      if (source === "drive") this.hooks.onOk?.(startedAt);
    };
    if (res.ok || res.status === 404) {
      answered();
      return res;
    }

    const reasons: string[] = [];
    let message = "";
    try {
      const body = (await res.json()) as {
        error?: { message?: unknown; errors?: Array<{ reason?: unknown }>; details?: Array<{ reason?: unknown }> };
      };
      for (const entry of [body.error?.errors?.[0], ...(body.error?.details ?? [])]) {
        if (typeof entry?.reason === "string") reasons.push(entry.reason);
      }
      if (typeof body.error?.message === "string") message = body.error.message;
    } catch {
      // Not JSON; the status alone decides.
    }
    const reason = reasons[0] ?? "";
    if (res.status === 403) {
      const rejection = serviceRejection(reasons, message);
      if (rejection) {
        throw new DriveError(403, false, `Drive HTTP 403 (${rejection})`, rejection, source);
      }
    }
    const retryable =
      res.status === 429 || res.status >= 500 || (res.status === 403 && reasons.some((r) => RATE_LIMIT_REASONS.has(r)));
    if (!retryable) answered();
    throw new DriveError(
      res.status,
      retryable,
      `Drive HTTP ${res.status}${reason ? ` (${reason})` : ""}`,
      undefined,
      source,
    );
  }

  /** Null when Drive says 404. */
  async getFile(id: string, signal?: AbortSignal): Promise<DriveFile | null> {
    const params = new URLSearchParams({
      fields: "id,name,mimeType,parents,trashed",
      supportsAllDrives: "true",
    });
    const res = await this.request(`${DRIVE_URL}/${encodeURIComponent(id)}?${params}`, "drive", signal);
    if (res.status === 404) return null;
    const body = (await res.json()) as Partial<DriveFile>;
    return {
      id: String(body.id ?? id),
      name: String(body.name ?? ""),
      mimeType: String(body.mimeType ?? ""),
      parents: Array.isArray(body.parents) ? body.parents.map(String) : [],
      trashed: body.trashed === true,
    };
  }

  /**
   * The guests of one calendar event, without this account itself. Null when
   * the event does not exist. Same credential and token as Drive: the grant
   * covers Calendar.
   */
  async eventGuests(calendarId: string, eventId: string, signal?: AbortSignal): Promise<string[] | null> {
    const params = new URLSearchParams({ fields: "attendees(email,self)" });
    const res = await this.request(
      `${CALENDAR_URL}/${encodeURIComponent(calendarId)}/events/${encodeURIComponent(eventId)}?${params}`,
      "calendar",
      signal,
    );
    if (res.status === 404) return null;
    const body = (await res.json()) as { attendees?: Array<{ email?: string; self?: boolean }> };
    return (body.attendees ?? []).filter((a) => !a.self).map((a) => String(a.email ?? ""));
  }

  async listFiles(p: {
    q: string;
    fields: string;
    pageSize: number;
    orderBy?: string;
    pageToken?: string;
    signal?: AbortSignal;
  }): Promise<{ files: Record<string, unknown>[]; nextPageToken?: string; incompleteSearch?: boolean }> {
    const params = new URLSearchParams({
      q: p.q,
      fields: p.fields,
      pageSize: String(p.pageSize),
      supportsAllDrives: "true",
      includeItemsFromAllDrives: "true",
      corpora: "allDrives",
    });
    if (p.orderBy) params.set("orderBy", p.orderBy);
    if (p.pageToken) params.set("pageToken", p.pageToken);
    const res = await this.request(`${DRIVE_URL}?${params}`, "drive", p.signal);
    if (res.status === 404) throw new DriveError(404, false, "Drive HTTP 404");
    const body = (await res.json()) as {
      files?: Record<string, unknown>[];
      nextPageToken?: string;
      incompleteSearch?: boolean;
    };
    return {
      files: body.files ?? [],
      ...(body.nextPageToken ? { nextPageToken: body.nextPageToken } : {}),
      ...(body.incompleteSearch ? { incompleteSearch: true } : {}),
    };
  }
}
