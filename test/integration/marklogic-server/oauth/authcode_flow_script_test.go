// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

// The curl driver models Keycloak's form_post login, not a full browser. Every
// request verifies TLS and has a deadline; redirects are inspected, never followed
// automatically. Only sanitized markers leave the client pod.
// Args: start URL, callback base, username, password, carry session (yes/no), CA,
// expected authorization endpoint, registered redirect URI, mode (start/full).
const authCodeFlowScript = `
set -eu
START_URL="$1"; CALLBACK_BASE="$2"; USER="$3"; PASS="$4"; CARRY="$5"
CA="$6"; AUTH_ENDPOINT="$7"; REDIRECT_URI="$8"; MODE="$9"
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
S1=$(request -c "$JAR" -D "$WORK/h1" -o /dev/null "$START_URL")
echo "STEP1_STATUS=$S1"
LOC=$(location "$WORK/h1")
SID=$(awk '$6=="SessionID"{v=$7} END{print v}' "$JAR")
echo "STEP1_SESSIONID_PRESENT=$([ -n "$SID" ] && echo yes || echo no)"
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
PATHQ=$(printf '%s' "$CBACT" | sed 's#^https://[^/]*##')
CBURL="${CALLBACK_BASE}${PATHQ}"

if [ "$CARRY" = yes ]; then
  S4=$(request -b "$JAR" -c "$JAR" -D "$WORK/h4" -o "$WORK/b4" --data-urlencode "code=$CODE" --data-urlencode "state=$STATE" "$CBURL")
else
  S4=$(request -D "$WORK/h4" -o "$WORK/b4" --data-urlencode "code=$CODE" --data-urlencode "state=$STATE" "$CBURL")
fi
echo "STEP4_STATUS=$S4"
LOC4=$(location "$WORK/h4")
case "$LOC4" in *'/protocol/openid-connect/auth'*) echo 'STEP4_RESTARTED=yes';; *) echo 'STEP4_RESTARTED=no';; esac
# Report the known state-specific error, never the body (which may contain state).
if tr '\n' ' ' < "$WORK/b4" | grep -Eq '<dt>XDMP-OAUTH: Novel OAuth state[^<]*potential CSRF attack[^<]*</dt>'; then
  echo 'STEP4_ERROR=NOVEL_OAUTH_STATE'
else
  echo 'STEP4_ERROR=OTHER'
fi
if [ "$CARRY" = yes ]; then
  # Prove the callback established an authenticated session. Request the fixed
  # protected endpoint with its cookie jar; do not follow callback redirects.
  S5=$(request -b "$JAR" -c "$JAR" -o "$WORK/b5" "$START_URL")
  echo "STEP5_STATUS=$S5"
  if [ "$(cat "$WORK/b5")" = "oauth-user:$USER" ]; then echo 'STEP5_IDENTITY_MATCH=yes'; else echo 'STEP5_IDENTITY_MATCH=no'; fi
fi
echo 'RESULT=DONE'
`
