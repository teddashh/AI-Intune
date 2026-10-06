# Operator identity and permissions

clawctl has no accounts, passwords, cookie sessions, or role database of its
own. The Hub's operator plane (web UI and `/v1/operator/*` JSON API) uses the
identity Tailscale already maintains: the node, its tailnet user, and
[grants app capabilities](https://tailscale.com/docs/features/access-control/grants/grants-app-capabilities).
Browser CSRF protection is Go's `http.CrossOriginProtection`.

Upstream references:

- Tailscale grants app capabilities: <https://tailscale.com/docs/features/access-control/grants/grants-app-capabilities>
- Tailscale grants syntax: <https://tailscale.com/docs/reference/syntax/grants>
- `Client.WhoIsForIP`: <https://pkg.go.dev/tailscale.com/client/local#Client.WhoIsForIP>
- `http.CrossOriginProtection`: <https://pkg.go.dev/net/http#CrossOriginProtection>

## 1. What a successful check proves

Every request that matches a registered UI or operator API route must carry an
HTTP `Host` equal to the literal Tailscale `IP:port` the Hub listens on. The Hub
then calls tailscaled's LocalAPI `WhoIsForIP` with `Request.RemoteAddr` and that
listener IP. It never reads `Forwarded`, `X-Forwarded-*`, `X-Real-IP`,
`Tailscale-*`, or `Authorization` to decide who the operator is.

The `Host` check pins the server authority. Without it, an authorized
workstation browsing a malicious site could be hit by DNS rebinding. Only the
literal listener authority is accepted. MagicDNS and custom hostnames are
not accepted.

A successful check proves three things:

1. The source is an untagged node that tailscaled knows.
2. That node has a tailnet user profile and stable user/node IDs.
3. Tailscale grants this source the exact app capability the route requires,
   for this Hub destination.

It does not prove that the person at the keyboard just signed in or passed
MFA. If you need that, put an OIDC proxy in front of the operator plane.

Machine and verifier bearer tokens belong to the producer plane. They are
never operator credentials.

The Hub requires tailscaled `1.100.0` or newer and checks this before it
authorizes a request. Older daemons can ignore the destination when they
return capabilities, so the Hub fails closed on them.

## 2. Three capabilities, no implied inheritance

You choose a prefix under a domain you own and set it explicitly:

```text
CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl
```

The Hub recognizes exactly three fixed, parameter-free keys:

```text
example.com/cap/clawctl-view
example.com/cap/clawctl-operate
example.com/cap/clawctl-admin
```

The Hub has no `admin ⇒ operate ⇒ view` rule. An operator who needs the full
console must receive all three keys in the Tailscale policy. The policy stays
the authority, and the Hub does not reinterpret it as a role hierarchy.

| Capability | Covers |
|---|---|
| `view` | Every HTML/CSV `GET`, the operator JSON list/detail reads (including disk-clean summaries), and `/metrics` |
| `operate` | Day-to-day actions on existing work: terminal, deployment Continue/Retry/skip failed batch, diagnostic jobs |
| `admin` | Changing the fleet's desired state: enrollment, channels, lifecycle, policy publication and assignment, artifact fetch, deployment create/abandon, retention, disk-clean profile/dry-run/canary/continue/abandon (and their previews) |

The route manifest in `cmd/clawctl-hub/operator_boundary.go` is the exact list.
The Hub compares it with the registered routes at startup and refuses to start
if a route is unclassified or a policy names a route that does not exist.

## 3. Tailscale grant

In the Tailscale admin console, open the access controls and merge an entry
like this into the existing `grants` array. Do not overwrite the rest of your
policy.

```json
{
  "src": ["operator@example.com"],
  "dst": ["100.64.0.12"],
  "ip": ["tcp:8787"],
  "app": {
    "example.com/cap/clawctl-view": [{}],
    "example.com/cap/clawctl-operate": [{}],
    "example.com/cap/clawctl-admin": [{}]
  }
}
```

Replace these placeholder values:

- `src`: your tailnet user or group.
- `dst`: the Hub's Tailscale IP, or a tag such as `tag:clawctl-hub`.
- `tcp:8787`: the port in `CLAWCTL_LISTEN`. `8787` is only the conventional
  example; the Hub has no default port.
- `example.com/cap/clawctl`: your `CLAWCTL_OPERATOR_CAPABILITY_PREFIX`.

Before you save, check the entry with the policy editor's own validation and
test features. The grant limits the source identity, the Hub destination, and
TCP reachability. The Hub then reads the destination-scoped app capability from
`WhoIsForIP` and makes the application-level decision.

On the Hub host, set the prefix in `hub.env` (systemd install) or
`ops/docker/hub.env` (Docker):

```text
CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl
```

`CLAWCTL_LISTEN` must be the literal Tailscale `IP:port`. The Hub rejects a
hostname, `0.0.0.0`, a LAN or public IP, or loopback before it opens the
database. If `CLAWCTL_PUBLIC_URL` is set, it must be exactly
`http://<that IP:port>`, ignoring a trailing slash.

## 4. CLI discovery

The grant is a lasting policy, not a per-operation login. The Hub re-checks the
source, destination, and capability on every request. Operator CLIs store no
token. All they need is the Hub origin, which they resolve in this order:

1. `--hub-url`
2. `CLAWCTL_HUB_URL`
3. `${XDG_CONFIG_HOME:-$HOME/.config}/clawctl/operator.json`, a single field:
   `{"hub_url":"http://100.64.0.12:8787"}`

`ops/install-hub.sh` writes that file after the operator home page returns
HTTP 200, which proves `view` only. The directory is `0700` and the file
`0600`. If the selected source is empty, malformed, or unsafe, the CLI fails
instead of guessing. Clients accept only
`http://<canonical literal Tailscale IP>:<nonzero port>`, with no path,
userinfo, query, or fragment. They ignore ambient HTTP proxies and refuse
redirects.

## 5. Request order and errors

```text
route classification → pinned Host authority → LocalAPI authorization → CrossOriginProtection → handler
```

- Wrong `Host`: `421 Misdirected Request`, before identity is looked up.
- No usable identity: `401`. Identity without the route's exact capability:
  `403`.
- LocalAPI unavailable or tailscaled too old: `503`. The operator plane fails
  closed. The machine plane (agent check-in and jobs) is outside this
  middleware and keeps working.
- A cross-origin unsafe request from a browser: `403`.

Operator writes are recorded in the audit log with the Tailscale user and node
that made them.
