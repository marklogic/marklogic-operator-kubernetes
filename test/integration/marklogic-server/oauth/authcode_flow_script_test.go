// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

// The curl driver models Keycloak's form_post login, not a full browser. Every
// request verifies TLS and has a deadline; redirects are inspected, never followed
// automatically. Only sanitized markers leave the client pod.
// Args: start URL, username, password, CA, expected authorization endpoint,
// registered redirect URI, mode (start/full), affinity (enabled/disabled).
const authCodeFlowScript = `
set -eu
START_URL="$1"; USER="$2"; PASS="$3"; CA="$4"
AUTH_ENDPOINT="$5"; REDIRECT_URI="$6"; MODE="$7"; AFFINITY="$8"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
JAR="$WORK/session"; KCJAR="$WORK/keycloak"
request() {
  curl -q --silent --show-error --proto '=https' --cacert "$CA" \
    --connect-timeout 5 --max-time 20 --write-out '%{http_code}' "$@"
}
location() {
  tr -d '\r' < "$1" | awk 'tolower($1)=="location:" {sub(/^[^:]*:[[:space:]]*/, ""); print; exit}'
}
form_action() {
  grep -io 'action="[^"]*"' "$1" | head -1 | sed -e 's/^[aA][cC][tT][iI][oO][nN]="//' -e 's/"$//' -e 's/&amp;/\&/g'
}
form_value() {
  grep -io "name=\"$2\"[^>]*" "$1" | head -1 | grep -io 'value="[^"]*"' | head -1 | sed -e 's/^[vV][aA][lL][uU][eE]="//' -e 's/"$//'
}
header() {
  tr -d '\r' < "$1" | awk -v key="$2" 'tolower($1)==tolower(key)":" {print $2; exit}'
}
backend() {
  value=$(header "$1" X-Test-Backend)
  case "$value" in node-0|node-1) printf '%s' "$value";; *) printf 'unknown';; esac
}
proxy_mode() {
  value=$(header "$1" X-Test-Affinity)
  case "$value" in affinity|no-affinity) printf '%s' "$value";; *) printf 'unknown';; esac
}
S1=$(request -H "X-Test-Affinity: $AFFINITY" -c "$JAR" -D "$WORK/h1" -o /dev/null "$START_URL")
echo "STEP1_STATUS=$S1"
echo "STEP1_BACKEND=$(backend "$WORK/h1")"
echo "STEP1_AFFINITY=$(proxy_mode "$WORK/h1")"
SESSION_HASH=$(header "$WORK/h1" X-Test-Session-Hash)
LOC=$(location "$WORK/h1")
SID=$(awk '$6=="SessionID"{v=$7} END{print v}' "$JAR")
echo "STEP1_SESSIONID_PRESENT=$([ -n "$SID" ] && echo yes || echo no)"
# Inspect the actual Set-Cookie attributes as well as curl's accepted jar.
ATTRS=$(tr -d '\r' < "$WORK/h1" | awk 'tolower($1)=="set-cookie:" && $2 ~ /^SessionID=/ {sub(/^[^;]*;/, ""); print tolower($0)}')
if [ "$(awk '$6=="SessionID"{print $3}' "$JAR")" = / ] && printf '%s' "$ATTRS" | grep -Eq '(^|;)[[:space:]]*path=/([[:space:]]*;|[[:space:]]*$)'; then
  echo 'STEP1_COOKIE_PATH=yes'
else
  echo 'STEP1_COOKIE_PATH=no'
fi
echo "STEP1_COOKIE_HTTPONLY=$(printf '%s' "$ATTRS" | grep -Eq '(^|;)[[:space:]]*httponly([[:space:]]*;|[[:space:]]*$)' && echo yes || echo no)"
case "$LOC" in "$AUTH_ENDPOINT"\?*) echo 'STEP1_IDP_MATCH=yes';; *) echo 'STEP1_IDP_MATCH=no'; echo 'RESULT=INVALID_REDIRECT'; exit 0;; esac
QUERY=${LOC#*\?}
if printf '%s\n' "$QUERY" | grep -Eq '(^|&)code_challenge=[^&]+(&|$)' && \
   printf '%s\n' "$QUERY" | grep -Eq '(^|&)code_challenge_method=S256(&|$)' && \
   printf '%s\n' "$QUERY" | grep -Eq '(^|&)response_type=code(&|$)' && \
   printf '%s\n' "$QUERY" | grep -Eq '(^|&)state=[^&]+(&|$)'; then
  echo 'STEP1_PKCE_PRESENT=yes'
else
  echo 'STEP1_PKCE_PRESENT=no'; echo 'RESULT=INVALID_PKCE'; exit 0
fi
case "$S1" in 302|303) ;; *) echo 'RESULT=INVALID_START_STATUS'; exit 0;; esac
if [ -z "$SID" ]; then echo 'RESULT=NO_SESSION'; exit 0; fi
if [ "$MODE" = start ]; then echo 'RESULT=START_ONLY'; exit 0; fi

S2=$(request -c "$KCJAR" -b "$KCJAR" -o "$WORK/login" "$LOC")
if [ "$S2" != 200 ]; then echo 'RESULT=LOGIN_PAGE_FAILED'; exit 0; fi
ACTION=$(form_action "$WORK/login")
# Never post credentials to an arbitrary form action.
REALM_BASE=${AUTH_ENDPOINT%/protocol/openid-connect/auth}
case "$ACTION" in "$REALM_BASE"/login-actions/authenticate\?*) echo 'STEP2_ACTION_PRESENT=yes';; *) echo 'STEP2_ACTION_PRESENT=no'; echo 'RESULT=NO_LOGIN_FORM'; exit 0;; esac

S3=$(request -c "$KCJAR" -b "$KCJAR" -o "$WORK/post" --data-urlencode "username=$USER" --data-urlencode "password=$PASS" --data-urlencode 'credentialId=' "$ACTION")
if [ "$S3" != 200 ]; then echo 'RESULT=LOGIN_FAILED'; exit 0; fi
# The pinned Keycloak form_post response uses uppercase NAME/VALUE attributes.
CODE=$(form_value "$WORK/post" code)
STATE=$(form_value "$WORK/post" state)
CBACT=$(form_action "$WORK/post")
echo "STEP3_CODE_PRESENT=$([ -n "$CODE" ] && echo yes || echo no)"
echo "STEP3_STATE_PRESENT=$([ -n "$STATE" ] && echo yes || echo no)"
if [ "$CBACT" = "$REDIRECT_URI" ]; then echo 'STEP3_CALLBACK_MATCH=yes'; else echo 'STEP3_CALLBACK_MATCH=no'; echo 'RESULT=INVALID_CALLBACK'; exit 0; fi
if [ -z "$CODE" ] || [ -z "$STATE" ]; then echo 'RESULT=NO_CODE'; exit 0; fi
# Keep the exact registered origin, path and TLS identity in both cases.
BASE=${START_URL%/identity.xqy}
if [ "$CBACT" != "$BASE/oauth/callback" ]; then echo 'RESULT=CHANGED_ORIGIN'; exit 0; fi
S4=$(request -H "X-Test-Affinity: $AFFINITY" -b "$JAR" -c "$JAR" -D "$WORK/h4" -o "$WORK/b4" --data-urlencode "code=$CODE" --data-urlencode "state=$STATE" "$CBACT")
echo "STEP4_BACKEND=$(backend "$WORK/h4")"
echo "STEP4_AFFINITY=$(proxy_mode "$WORK/h4")"
RECEIVED_HASH=$(header "$WORK/h4" X-Test-Received-Session-Hash)
if [ -n "$SESSION_HASH" ] && [ "$SESSION_HASH" = "$RECEIVED_HASH" ]; then echo 'STEP4_SESSION_PRESERVED=yes'; else echo 'STEP4_SESSION_PRESERVED=no'; fi
echo "STEP4_STATUS=$S4"
LOC4=$(location "$WORK/h4")
case "$LOC4" in
 "$START_URL") echo 'STEP4_REDIRECT=initial-resource';;
 "$REDIRECT_URI") echo 'STEP4_REDIRECT=callback';;
 "$AUTH_ENDPOINT"\?*) echo 'STEP4_REDIRECT=idp';;
 "$BASE"/*) echo 'STEP4_REDIRECT=same-origin';;
 /*) echo 'STEP4_REDIRECT=relative';;
 "") echo 'STEP4_REDIRECT=none';;
 *) echo 'STEP4_REDIRECT=other';;
esac
echo "STEP4_SESSION_COOKIE_SET=$(tr -d '\r' < "$WORK/h4" | awk 'tolower($1)=="set-cookie:" && $2 ~ /^SessionID=/ {found=1} END{print found ? "yes" : "no"}')"
case "$LOC4" in *'/protocol/openid-connect/auth'*) echo 'STEP4_RESTARTED=yes';; *) echo 'STEP4_RESTARTED=no';; esac
# Report the known state-specific error, never the body (which may contain state).
if tr '\n' ' ' < "$WORK/b4" | grep -Eq '<dt>XDMP-OAUTH: Novel OAuth state[^<]*potential CSRF attack[^<]*</dt>'; then
  echo 'STEP4_ERROR=NOVEL_OAUTH_STATE'
else
  echo 'STEP4_ERROR=OTHER'
fi
if [ "$AFFINITY" = enabled ]; then
  # Prove the callback established an authenticated session. Request the fixed
  # protected endpoint with its cookie jar; do not follow callback redirects.
  S5=$(request -H "X-Test-Affinity: $AFFINITY" -b "$JAR" -c "$JAR" -D "$WORK/h5" -o "$WORK/b5" "$START_URL")
  echo "STEP5_STATUS=$S5"
  EXPECTED_HASH=$(header "$WORK/h4" X-Test-Session-Hash)
  if [ -z "$EXPECTED_HASH" ]; then EXPECTED_HASH="$SESSION_HASH"; fi
  FINAL_HASH=$(header "$WORK/h5" X-Test-Received-Session-Hash)
  if [ -n "$EXPECTED_HASH" ] && [ "$EXPECTED_HASH" = "$FINAL_HASH" ]; then echo 'STEP5_SESSION_PRESERVED=yes'; else echo 'STEP5_SESSION_PRESERVED=no'; fi
  echo "STEP5_BACKEND=$(backend "$WORK/h5")"
  case "$(location "$WORK/h5")" in "$AUTH_ENDPOINT"\?*) echo 'STEP5_RESTARTED=yes';; *) echo 'STEP5_RESTARTED=no';; esac
  if [ "$(cat "$WORK/b5")" = "oauth-user:$USER" ]; then echo 'STEP5_IDENTITY_MATCH=yes'; else echo 'STEP5_IDENTITY_MATCH=no'; fi
fi
echo 'RESULT=DONE'
`
