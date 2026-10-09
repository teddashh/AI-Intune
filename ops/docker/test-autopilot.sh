#!/usr/bin/env bash
# Static packaging contract; no Docker daemon or Compose plugin required.
# Match literal variable placeholders in the packaging files.
# shellcheck disable=SC2016
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
compose="$HERE/docker-compose.autopilot.yml"
fail() { echo "FAIL: Autopilot $*" >&2; exit 1; }
! grep -Eq 'network_mode:|tailscaled\.sock' "$compose" || fail 'must use bridge networking without LocalAPI'
grep -q 'driver: bridge' "$compose" || fail 'bridge network missing'
grep -q 'CLAWCTL_AUTH_MODE: local' "$compose" || fail 'local auth missing'
grep -q 'CLAWCTL_LISTEN: 0.0.0.0:8787' "$compose" || fail 'container listener missing'
grep -Fq 'CLAWCTL_PUBLIC_URL: https://${CLAWCTL_PUBLIC_HOST:?' "$compose" || fail 'required public origin missing'
grep -Eq '^      CLAWCTL_SETUP_CODE:$' "$compose" || fail 'optional setup code must not default to empty'
hub="$(sed -n '/^  hub:$/,/^  caddy:$/p' "$compose")"
! grep -Eq '^[[:space:]]+ports:' <<<"$hub" || fail 'Hub must not publish ports'
grep -q 'target: hub$' <<<"$hub" || fail 'Hub build target missing'
grep -q 'service_completed_successfully' <<<"$hub" || fail 'volume initialization dependency missing'
grep -q 'target: hub-init' "$compose" || fail 'initializer missing'
grep -Eq 'image: caddy:2\.[0-9]+$' "$compose" || fail 'Caddy minor version must be pinned'
for required in '"80:80"' '"443:443"' './Caddyfile.autopilot:/etc/caddy/Caddyfile:ro' 'caddy_data:/data' 'caddy_config:/config'; do
  grep -Fq "$required" "$compose" || fail "missing $required"
done
grep -Fq '{$CLAWCTL_PUBLIC_HOST}' "$HERE/Caddyfile.autopilot" || fail 'Caddy hostname missing'
grep -q 'reverse_proxy hub:8787' "$HERE/Caddyfile.autopilot" || fail 'Caddy upstream missing'
grep -Eq '^https:// \{$' "$HERE/Caddyfile.autopilot" || fail 'Caddy catch-all site missing'
grep -Eq '^[[:space:]]+respond "Misdirected Request" 421$' "$HERE/Caddyfile.autopilot" || fail 'Caddy catch-all 421 missing'
! grep -Eiq 'header_up|X-Forwarded|X-Real-IP' <(sed '/^[[:space:]]*#/d' "$HERE/Caddyfile.autopilot") || fail 'unexpected identity header configuration'
echo 'ok - Autopilot standalone Docker+Caddy contract'

subnet="$(sed -n 's/^        - subnet: //p' "$compose")"
trusted="$(sed -n 's/^      CLAWCTL_TRUSTED_PROXIES: //p' "$compose")"
[[ "$subnet" == '${CLAWCTL_DOCKER_SUBNET:-172.31.87.0/24}' && "$subnet" == "$trusted" ]] || fail 'subnet must equal trusted proxies'
grep -q 'CLAWCTL_CLIENT_IP_HEADER: X-Forwarded-For' "$compose" || fail 'client IP header missing'

grep -Eq '^      CLAWCTL_REQUIRE_MFA:$' "$compose" || fail 'MFA must preserve default-on and pass explicit opt-out'
grep -Fq '# CLAWCTL_REQUIRE_MFA=0  # NOT recommended' "$HERE/autopilot.env.example" || fail 'MFA opt-out warning missing'
