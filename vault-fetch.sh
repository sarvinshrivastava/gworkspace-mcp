# vault-fetch.sh — the single implementation of "read one secret from the vault".
#
# SOURCED, never executed. Two consumers, deliberately different environments:
#   - slim-entrypoint.sh   inside the container at boot (baked in by Dockerfile.slim)
#   - .github/workflows/deploy-vps.yml   on the GitHub runner, from the checkout
#
# This file exists because the two copies had already drifted in five ways and
# shared the same two bugs — a retried response body gluing itself in front of the
# good one, and an empty value passing every guard. One implementation, one place
# to fix.
#
# Requires in the environment:
#   VAULT_URL     e.g. https://secrets.vps.sarvinshrivastava.space
#   VAULT_FOLDER  e.g. Root
#   VAULT_TOKEN   a folder-scoped READ token
# Optional:
#   VAULT_ERR_PREFIX  "FATAL:" (default, for container logs) or "::error::" (Actions)

vault_fetch() {
  local key="$1" body http_code curl_rc value
  local prefix="${VAULT_ERR_PREFIX:-FATAL:}"

  # --retry-all-errors matters: plain --retry does NOT retry DNS failure (curl
  # exit 6) or TLS failure (exit 60), which are exactly the reboot and
  # cert-expiry cases.
  set +e
  body=$(curl -sS --max-time 20 --retry 3 --retry-delay 2 --retry-all-errors \
    -w $'\n%{http_code}' \
    -H "Authorization: Bearer ${VAULT_TOKEN}" \
    "${VAULT_URL}/api/secrets/${VAULT_FOLDER}/${key}" 2>/tmp/vault-curl.err)
  curl_rc=$?
  set -e

  if [ $curl_rc -ne 0 ]; then
    # Report curl's own exit code — without it the log says only "unreachable"
    # and you cannot tell DNS from TLS from connection-refused at 2am.
    echo "${prefix} vault unreachable fetching ${key} (curl exit ${curl_rc}: $(tr -d '\n' < /tmp/vault-curl.err | tail -c 200))" >&2
    rm -f /tmp/vault-curl.err
    return 2   # transient — caller may retry
  fi
  rm -f /tmp/vault-curl.err

  http_code="${body##*$'\n'}"
  body="${body%$'\n'*}"

  case "$http_code" in
    200) ;;
    429|5??)
      echo "${prefix} vault returned HTTP ${http_code} for ${VAULT_FOLDER}/${key}, retryable" >&2
      return 2 ;;
    *)
      # 401 token rejected, 403 folder outside token scope, 404 key absent.
      # None of these fix themselves — fail fast rather than loop.
      echo "${prefix} vault returned HTTP ${http_code} for ${VAULT_FOLDER}/${key}" >&2
      return 1 ;;
  esac

  # `-s` + `last`, not a bare `.value`: curl cannot rewind a pipe, so on an HTTP
  # retry the failed attempt's body stays on stdout and the good body is APPENDED
  # to it. A bare `jq -re '.value'` parses both documents in streaming mode and
  # exits 0, yielding "null\n<real secret>" — which exports cleanly and then 401s
  # every downstream call.
  value=$(jq -rse 'last.value' <<<"$body" 2>/dev/null) || {
    echo "${prefix} no .value in vault response for ${key}" >&2
    return 1
  }

  # jq -re exits 0 on an empty string — only null and false trip -e.
  if [ -z "$value" ]; then
    echo "${prefix} vault returned an empty value for ${VAULT_FOLDER}/${key}" >&2
    return 1
  fi

  printf '%s' "$value"
}
