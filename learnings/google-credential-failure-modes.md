# Google credential failure modes

> **Applies to:** `src/drive/**` `src/enforce/**` `src/notify.ts`

Looked like "Drive is flaky, try again"; was a dead refresh token for five days (2026-10-18 to 10-24). `/healthz` stayed 200 and the only trace was one boot-probe line.

## What the service now says

- A refresh the token endpoint refuses (any 4xx except 408/429) is a **rejection**: `credential_rejected`, "do not retry". 5xx, 408, 429 and network errors stay `drive_error`, "try again".
- A Drive 403 `accessNotConfigured` / `SERVICE_DISABLED` / `ACCESS_TOKEN_SCOPE_INSUFFICIENT` is also a rejection. A bare `insufficientPermissions` is not (it can be one file).
- Calendar has its own `calendar_rejected` and feeds nothing: Calendar failing says nothing about Drive. `DriveError.source` (`oauth|drive|calendar`) carries the split, including through the 401 retry.
- `error_description` is never read: a refresh error body can echo the request. Only `error` is kept, and only if it looks like `^[a-z_]{1,40}$`.
- Every `drive_error`/`credential_rejected` carries a `category` by type. There is no `?? "network"` fallback; add one and `invalid_grant` pushes read `(network)`.

## Health and pushes

- `/healthz` gains `google: {state, since, consecutiveFailures, probe}`. The HTTP status stays tied to the child: failing it over a Google outage would only restart-loop the container.
- The boot probe is the only place the cause shows without a failed tool call. `invalid_grant` anywhere in its text (not just the first 300 characters) sets `rejected`.
- A stale success never clears `rejected`: `onOk(startedAt)` counts only if the request started after the newest rejection.
- One shared ntfy topic, broken-only, no recovery message. At most one delivered push per kind per 30 minutes, one send in flight per kind, one attempt per kind per 60 seconds. A failed send is retried at the **next event** of that kind (no timers), so under quiet traffic nothing retries until then.
- During an outage with steady traffic the same push repeats every 30 minutes per kind.
- Push bodies are fixed templates. The category is whitelisted; free text goes to the log only, after the `SECRETISH` redaction.

## Gaps accepted on purpose

- The relay path (unconfined agents) is not counted: matching `invalid_grant` in relayed text is spoofable by agent input.
- Admin dashboard Drive successes call `onOk` (shared client); admin failures are never counted.
- The `<n> consecutive failures` count is tool calls, and a call that made an earlier successful Drive request resets it, so partial outages under-alert.
- State is in memory. An autoheal restart loop after a successful boot pushes once per restart; a boot that dies before `ready` pushes nothing; a failed probe push is not retried within a boot (the probe fires once).
- A mistyped ntfy topic answers 200 and cannot be detected. On public ntfy.sh the topic is a credential: anyone who knows it can read it and post to it, including text an agent might trust. The four notice templates in the README are the only authentic shapes.
- Consent screen in Testing status expires the refresh token every 7 days (README); the push is the reminder.
