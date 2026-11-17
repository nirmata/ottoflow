#!/usr/bin/env bash
# Fetch an LLM provider key from a token endpoint and write it into the
# llm-credentials Secret. When agentExecutor.llmCredentialsSecret names that
# Secret, the agent-executor reads the key file on every agent call, so no
# restart is needed.
#
# Usage: TOKEN_URL=https://tokens.example.com/anthropic KEY_NAME=ANTHROPIC_API_KEY ./rotate-llm-key.sh
#   TOKEN_URL         (required) https endpoint that returns the key as the plain response body
#   KEY_NAME          (required) provider variable, for example ANTHROPIC_API_KEY
#   SECRET_NAME       (default llm-credentials)
#   NAMESPACE         (default ottoflow)
#   TOKEN_HEADER_FILE (optional) file holding one header line, for example
#                     "Authorization: Bearer ...", sent with the request. curl reads
#                     the credential from the file, so it is not in curl's process
#                     arguments. Keep the file mode 0600.
#   KEY_PATTERN       (optional) bash regex the token must match, for example ^sk-ant-
{ set +x; } 2>/dev/null # tracing would print the key
set -euo pipefail

: "${TOKEN_URL:?set TOKEN_URL}"
: "${KEY_NAME:?set KEY_NAME}"
SECRET_NAME="${SECRET_NAME:-llm-credentials}"
NAMESPACE="${NAMESPACE:-ottoflow}"

die() { echo "rotate-llm-key: $*" >&2; exit 1; }

[[ $KEY_NAME =~ ^[A-Z_][A-Z0-9_]*$ ]] || die "KEY_NAME must match ^[A-Z_][A-Z0-9_]*\$"

# Bash returns 2 from =~ for an invalid regex; check before any request.
if [[ -n ${KEY_PATTERN:-} ]]; then
  rc=0
  # shellcheck disable=SC2319 # $? here is the =~ status on purpose
  [[ "" =~ $KEY_PATTERN ]] || rc=$?
  (( rc != 2 )) || die "KEY_PATTERN is not a valid regex; Secret left unchanged"
fi

# No -L, so a redirect is a non-2xx status and is refused below. -q (first)
# makes curl ignore any curlrc. --proto '=https' allows https only. The status
# is appended after the body on its own line.
curl_args=(-q -sS --proto '=https' --max-time 30 -w '\n%{http_code}')
if [[ -n ${TOKEN_HEADER_FILE:-} ]]; then curl_args+=(-H @"$TOKEN_HEADER_FILE"); fi
resp=$(curl "${curl_args[@]}" "$TOKEN_URL") || die "token request failed; Secret left unchanged"
status=${resp##*$'\n'}
body=${resp%$'\n'*}

[[ $status =~ ^2[0-9][0-9]$ ]] || die "token endpoint returned HTTP $status, not 2xx; Secret left unchanged"

[[ $body =~ [^[:space:]] ]] || die "token endpoint returned an empty body; Secret left unchanged"

# Allow only characters that appear in API keys and tokens, ignoring surrounding
# whitespace. This rejects an HTML or JSON error page served with a 2xx, and it
# makes the token safe to place in the JSON patch below without a JSON tool.
[[ $body =~ ^[[:space:]]*([A-Za-z0-9._~+/=:-]+)[[:space:]]*$ ]] || die "response contains characters not valid in a key; Secret left unchanged"
token=${BASH_REMATCH[1]}
(( ${#token} >= 20 )) || die "token is shorter than 20 characters; Secret left unchanged"
(( ${#token} <= 512 )) || die "token is longer than 512 characters; Secret left unchanged"
if [[ -n ${KEY_PATTERN:-} ]]; then
  [[ $token =~ $KEY_PATTERN ]] || die "token does not match KEY_PATTERN; Secret left unchanged"
fi

# The token goes to kubectl on stdin: not in argv, and no temp file.
printf '{"stringData":{"%s":"%s"}}' "$KEY_NAME" "$token" |
  kubectl -n "$NAMESPACE" patch secret "$SECRET_NAME" --type merge --patch-file /dev/stdin >/dev/null

echo "rotated $KEY_NAME in secret/$SECRET_NAME (namespace $NAMESPACE)"
