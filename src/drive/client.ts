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
const RATE_LIMIT_REASONS = new Set(["rateLimitExceeded", "userRateLimitExceeded"]);

/**
 * `retryable` separates "Drive could not answer right now" (rate limits, 5xx,
 * network) from "Drive answered no". The first must never be shown to a model
 * as "outside your scope", and must never be cached.
 */
export class DriveError extends Error {
  constructor(
    public status: number,
    public retryable: boolean,
    message: string,
  ) {
    super(message);
  }
}

export class DriveClient {
  private accessToken: { value: string; expiresAt: number } | null = null;
  private refreshing: Promise<string> | null = null;

  constructor(
    private readonly creds: DriveCreds,
    private readonly fetchImpl: typeof fetch = fetch,
    private readonly timeoutMs = 10_000,
    private readonly now: () => number = Date.now,
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
      throw new DriveError(0, true, `token refresh failed: ${errorMessage(error)}`);
    }
    // The status only. The body of a failed refresh can echo the request.
    if (!res.ok) {
      throw new DriveError(res.status, res.status >= 500, `token refresh failed: HTTP ${res.status}`);
    }
    const body = (await res.json()) as { access_token?: string; expires_in?: number };
    if (!body.access_token) throw new DriveError(res.status, true, "token refresh returned no token");
    this.accessToken = {
      value: body.access_token,
      expiresAt: this.now() + ((body.expires_in ?? 3600) - 60) * 1000,
    };
    return body.access_token;
  }

  private async request(url: string, signal?: AbortSignal, retried = false): Promise<Response> {
    const token = await this.token();
    let res: Response;
    try {
      res = await this.fetchImpl(url, {
        headers: { Authorization: `Bearer ${token}` },
        signal: this.signal(signal),
      });
    } catch (error) {
      throw new DriveError(0, true, `Drive unreachable: ${errorMessage(error)}`);
    }
    if (res.status === 401) {
      this.accessToken = null;
      if (!retried) return this.request(url, signal, true);
      throw new DriveError(401, true, "Drive rejected a freshly refreshed token");
    }
    if (res.ok || res.status === 404) return res;

    let reason = "";
    try {
      const body = (await res.json()) as { error?: { errors?: Array<{ reason?: string }> } };
      reason = body.error?.errors?.[0]?.reason ?? "";
    } catch {
      // Not JSON; the status alone decides.
    }
    const retryable =
      res.status === 429 || res.status >= 500 || (res.status === 403 && RATE_LIMIT_REASONS.has(reason));
    throw new DriveError(res.status, retryable, `Drive HTTP ${res.status}${reason ? ` (${reason})` : ""}`);
  }

  /** Null when Drive says 404. */
  async getFile(id: string, signal?: AbortSignal): Promise<DriveFile | null> {
    const params = new URLSearchParams({
      fields: "id,name,mimeType,parents,trashed",
      supportsAllDrives: "true",
    });
    const res = await this.request(`${DRIVE_URL}/${encodeURIComponent(id)}?${params}`, signal);
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
    const res = await this.request(`${DRIVE_URL}?${params}`, p.signal);
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
