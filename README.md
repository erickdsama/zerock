# zerock

Share a local port on a subdomain of **your own** domain.

```
zerock http 3000 --sub api-x     →  https://api-x.yourdomain.com
zerock http 3000                 →  https://swift-otter-4f2.yourdomain.com
zerock tcp 5432 --sub db         →  db.yourdomain.com:20500
```

One static Go binary is both sides: `zerock serve` runs on a box that owns your
domain, and `zerock http` runs on your laptop. The laptop dials **out**, so
nothing needs to be open on your machine or your router.

## How it works

```
   your laptop                       your public box                internet
┌──────────────────┐            ┌───────────────────────────┐
│ zerock http 3000 │   TLS      │ control  :7223            │  :443   *.dom.com
│      --sub api-x │═══════════▶│ edge     :80 / :443       │◀─────── api-x.dom.com
│                  │  (yamux)   │ api      :7224 loopback   │
│ localhost:3000   │◀══════════▶│ bbolt: tokens + reserves  │
└──────────────────┘  streams   └───────────────────────────┘
```

The agent authenticates once with a token, then the connection becomes a yamux
session. Every inbound request becomes a new stream that the agent splices
straight to your local port — it never parses HTTP, which is why WebSockets,
SSE, chunked responses and long uploads all work unchanged. The server does the
HTTP parsing and pushes a request log back over a dedicated event stream, so the
CLI can show live traffic.

## Server setup

### 1. DNS

Point a wildcard and the apex at the box:

```
*.yourdomain.com   A   203.0.113.10
yourdomain.com     A   203.0.113.10
```

### 2. Config

```sh
zerock init-config --domain yourdomain.com --email you@yourdomain.com \
  | sudo tee /etc/zerock/zerock.yaml
```

TLS has three modes:

| `tls.mode` | What it does | When to use it |
|---|---|---|
| `auto` (default) | Gets a wildcard `*.yourdomain.com` certificate from Let's Encrypt over a **DNS-01** challenge, and renews it | Standalone. Nothing else to run |
| `files` | Uses a certificate and key you already have | You manage certificates elsewhere |
| `off` | Serves plain HTTP | Behind Caddy, nginx or Traefik — see `--behind-proxy` |

A wildcard certificate can only be issued over DNS-01, so `auto` needs a DNS
provider token. **Cloudflare** and **DigitalOcean** are built in:

```sh
zerock init-config --domain yourdomain.com --dns-provider digitalocean
```

For any other provider, use `tls.mode: files` with a wildcard certificate
obtained elsewhere, or run behind a proxy that handles TLS. Keep the token out of
the config file:

```sh
echo 'ZEROCK_DNS_API_TOKEN=...' | sudo tee /etc/zerock/zerock.env
sudo chmod 600 /etc/zerock/zerock.env
```

Token scope — the wrong one is the usual reason issuance fails:

| Provider | Credential |
|---|---|
| `cloudflare` | API token with **Zone:Read** and **DNS:Edit**, scoped to that one zone |
| `digitalocean` | Personal access token with **read and write** scope |

While testing, set `ca: staging` in the config to avoid Let's Encrypt rate
limits, then remove it once you see a certificate issued.

### 3. Firewall

Open `443`, `80` (redirects), `7223` (agents), and the `tcp_port_range` if you
want `zerock tcp`. Leave `admin_addr` on loopback.

### 4. Run it as a daemon

Copy **just the binary** to the server — it carries its own systemd units and
config templates, so there is nothing else to bring:

```sh
make build                                  # ./bin/zerock, for this machine's OS and arch
make release                                # or ./dist/ for other targets
scp dist/zerock-server-linux-amd64 you@server:zerock
ssh you@server
sudo ./zerock service install --domain yourdomain.com --email you@yourdomain.com
```

Or download `zerock-server-linux-<arch>` from a release rather than building it.
The server binary is the full one — it answers `zerock http` as well, so you do
not need both on that machine.

Add `--dns-provider digitalocean` if your DNS is on DigitalOcean rather than
Cloudflare.

That copies the binary to `/usr/local/bin`, writes `/etc/zerock/zerock.yaml` and
`/etc/zerock/zerock.env`, installs the systemd unit, and starts it. Re-running
keeps an existing config, so it doubles as the upgrade path.

See exactly what it would touch first:

```sh
./zerock service install --domain yourdomain.com --dry-run   # no root needed
```

If the DNS token is not set yet, install **stops before starting** and tells you
what is missing, rather than leaving systemd in a crash loop.

### Checking it works

```sh
sudo zerock doctor
```

It reports the config it loaded, whether the service is active, which ports are
listening, what certificate is actually served (issuer, expiry, whether it
covers the wildcard, and whether it is a staging certificate), whether the apex
and wildcard resolve to an address this host holds, and whether the API answers.
It exits non-zero if anything failed, so it works in a check script.

What it cannot tell you is whether your ports are reachable **from the
internet** — a firewall in the way looks identical to a healthy server from the
server itself. Confirm that from another machine:

```sh
curl -sv https://api-x.yourdomain.com/ 2>&1 | head -20
```

Then close SSH. It keeps running.

```sh
systemctl status zerock            # up? since when? last exit reason?
journalctl -u zerock -f            # follow the log
journalctl -u zerock | grep zk_    # the admin token, printed once at first start
systemctl restart zerock           # after a config edit
sudo zerock service uninstall      # stop, disable, remove the unit
sudo zerock service uninstall --purge   # also delete the config and all tokens
```

`enable --now` is what the installer does for you: **enable** survives reboots,
**--now** starts it immediately. systemd owns the process, so closing your SSH
session does not touch it, and `Restart=always` brings it back after a crash.
`SIGTERM` shutdown is clean, so `systemctl stop` and `restart` never hang.

The unit runs unprivileged under `DynamicUser`, keeping only
`CAP_NET_BIND_SERVICE` so it can still bind 80 and 443. State lives in
`/var/lib/zerock` via `StateDirectory`.

**Without systemd** — in a container, or on a distro that does not use it — run
`zerock serve --config /etc/zerock/zerock.yaml` as PID 1, or under whatever
supervisor you have. `zerock service install` refuses to run where systemd is
absent rather than writing units nothing will read.

### Leaving a tunnel running on a server

The above daemonises the *server*. If the app you want to publish lives on a
different machine, the **agent** there also needs to outlive your SSH session.
Same single binary, one service per tunnel:

```sh
sudo zerock service tunnel api-x -- http 3000 --sub api-x --quiet
sudo zerock service tunnel db    -- tcp 5432 --sub db --quiet
```

Everything after `--` is passed to `zerock http`/`zerock tcp` untouched, so any
flag those accept works here. The server and token come from your saved profile
unless you pass `--server` and `--token`, and they are written to
`/etc/zerock/tunnels/<name>.env` at mode 600 — never to the command line, where
`ps` would show the token to every user on the box.

```sh
journalctl -u zerock-tunnel@api-x -f
systemctl disable --now zerock-tunnel@api-x
```

Reserve the subdomain first (`zerock reserve api-x`) so the tunnel always returns
on the same hostname. The agent reconnects by itself with backoff and keeps its
subdomain, so a server restart or a flaky link recovers without systemd
intervening.

## Client setup

Grab the CLI from the [latest release](../../releases/latest) —
`zerock-linux-amd64` or `zerock-linux-arm64`:

```sh
curl -fsSLo zerock https://github.com/erickdsama/zerock/releases/latest/download/zerock-linux-amd64
chmod +x zerock && sudo mv zerock /usr/local/bin/
```

This is the **client build**: the CLI without the server, so it carries no ACME,
DNS-provider or database code and is about half the size. It is all you need on
a machine that only opens tunnels. The host that runs the server wants
`zerock-server-linux-<arch>` instead, which is the same CLI *plus* the server.

Releases are Linux only. The client has no platform-specific code, so
`GOOS=darwin make build-cli` produces a working macOS binary if you ever want
one — it is simply not published.

Verify a download against the release's `SHA256SUMS`:

```sh
sha256sum -c --ignore-missing SHA256SUMS
```

Then point it at your server:

```sh
zerock login --server zerock.yourdomain.com --token zk_ab12cd34_...
zerock http 3000 --sub api-x
```

`login` verifies the token against the server before saving anything, so a typo
fails immediately rather than at your first tunnel.

Several domains at once — each gets a profile:

```sh
zerock login --server zerock.acme.dev  --token zk_... --profile acme
zerock login --server zerock.other.io  --token zk_... --profile other
zerock http 3000 --profile acme
```

## Commands

| Command | What it does |
|---|---|
| `zerock http <port>` | Share a local HTTP port |
| `zerock tcp <port>` | Share a local TCP port on a public port |
| `zerock ls` | Tunnels running right now |
| `zerock kill <id>` | Close a tunnel (the agent stops, it does not reconnect) |
| `zerock reserve <sub>` | Claim a subdomain for your token only |
| `zerock unreserve <sub>` | Give it up |
| `zerock reservations` | List claimed subdomains |
| `zerock whoami` | Which token you are using and what it may do |
| `zerock token new/ls/revoke/rm` | Manage tokens (admin) |
| `zerock login/logout/profiles` | Manage saved servers |
| `zerock doctor` | Check that a running server actually works |
| `zerock serve` | Run the server |
| `zerock service install/uninstall/tunnel` | Install as a systemd service |
| `zerock init-config` | Print a starter server config |

Useful flags on `http`:

```sh
zerock http 8080 --auth demo:hunter2   # basic auth demanded at the edge, so the
                                       # credentials never reach your app
zerock http 3000 --host 192.168.1.20   # forward to another host on your LAN
zerock http 3000 --quiet               # print only the URL, for scripts
zerock http 3000 --no-reconnect        # exit on disconnect instead of retrying
```

### Random vs reserved subdomains

Without `--sub` you get a fresh name like `swift-otter-4f2`. Reconnects keep the
same name, so a URL stays valid across a dropped connection or a server restart.

`--sub api-x` asks for a specific name. It is granted unless another live tunnel
holds it, another token has reserved it, or the operator listed it in
`reserved_subdomains`. To make a name yours permanently — safe to hard-code in a
webhook — reserve it:

```sh
zerock reserve api-x
```

## Authentication

Tokens look like `zk_<id>_<secret>`: 160 bits of entropy, and only a SHA-256 of
the secret half is stored. The id is embedded so a lookup is one key read rather
than a scan over every token.

Two scopes:

- **`tunnel`** — open tunnels, manage your own reservations, see your own tunnels.
- **`admin`** — everything, plus managing tokens and other people's tunnels.
  Implies `tunnel`.

```sh
zerock token new --label "erick laptop"
zerock token new --label ci --expires 720h --max-tunnels 2
zerock token new --label ops --scopes tunnel,admin
zerock token revoke ab12cd34   # keeps the record and its reservations
zerock token rm ab12cd34       # deletes it and frees its subdomains
```

Revoking or deleting a token **closes its live tunnels immediately** and tells
those agents not to reconnect. It does not wait for the next handshake.

The API refuses to revoke or delete the token making the request, so you cannot
lock yourself out.

## Dashboard

The API host serves a web dashboard at `https://zerock.yourdomain.com`. Sign in
with any token; it is held in `sessionStorage`, so closing the tab discards it.

- **Tunnels** — what is live, with traffic counters, refreshed every 5 seconds.
  Close any of them.
- **Reservations** — claim and release subdomains.
- **Tokens** — admin only: create, revoke and delete. A new token's secret is
  shown once, on creation.

The page is a single self-contained document embedded in the binary: no CDN, no
build step, nothing to deploy alongside it. It serves under a strict
`Content-Security-Policy` that permits only its own inline style and script and
calls back to its own origin, and is sent `no-store` with framing denied.

If you have not opened port 443 yet, reach it over an SSH tunnel to the loopback
admin port instead:

```sh
ssh -L 7224:127.0.0.1:7224 you@server
# then open http://127.0.0.1:7224/
```

## HTTP API

Served on the API host over HTTPS (`https://zerock.yourdomain.com`) and on
`admin_addr` over loopback. Loopback is always reachable, which matters when DNS
or TLS is what broke.

```sh
curl -H "Authorization: Bearer $ZEROCK_TOKEN" https://zerock.yourdomain.com/api/v1/tunnels
```

| Method | Path | Scope | |
|---|---|---|---|
| GET | `/healthz` | none | liveness, version, tunnel count |
| GET | `/api/v1/whoami` | tunnel | the calling token |
| GET | `/api/v1/tunnels` | tunnel | live tunnels (admin sees all; `?mine=true` to narrow) |
| DELETE | `/api/v1/tunnels/{id}` | tunnel | close one |
| GET | `/api/v1/reservations` | tunnel | claimed subdomains |
| POST | `/api/v1/reservations` | tunnel | `{"sub":"api-x","note":"..."}` |
| DELETE | `/api/v1/reservations/{sub}` | tunnel | release |
| GET | `/api/v1/tokens` | admin | list |
| POST | `/api/v1/tokens` | admin | `{"label":"ci","scopes":["tunnel"],"expires_in":"720h"}` |
| GET | `/api/v1/tokens/{id}` | admin | one token |
| POST | `/api/v1/tokens/{id}/revoke` | admin | revoke |
| DELETE | `/api/v1/tokens/{id}` | admin | delete |

`POST /api/v1/tokens` is the only response that ever contains the secret.
Tokens are accepted only in the `Authorization` header — never a query
parameter, which would leak them into access logs.

## Environment variables

| Variable | Side | Purpose |
|---|---|---|
| `ZEROCK_SERVER` | client | `host[:port]`, overrides the profile |
| `ZEROCK_TOKEN` | client | token, overrides the profile |
| `ZEROCK_PROFILE` | client | which profile to use |
| `ZEROCK_CONFIG` | client | config file path |
| `ZEROCK_DNS_API_TOKEN` | server | DNS provider credential for ACME |
| `ZEROCK_DOMAIN` | server | overrides `domain` |
| `ZEROCK_CONFIG_FILE` | server | default `--config` path |

`ZEROCK_SERVER` plus `ZEROCK_TOKEN` are enough on their own, so CI needs no
config file:

```sh
ZEROCK_SERVER=zerock.yourdomain.com ZEROCK_TOKEN=zk_... zerock http 3000 --quiet
```

## Behind an existing reverse proxy

```sh
zerock init-config --domain yourdomain.com --behind-proxy
```

Your proxy terminates TLS for `*.yourdomain.com` and forwards to `http_addr`.
The control port still carries tokens, so put TLS in front of it too (Caddy
`layer4` or nginx `stream` with TLS termination). Only for a network you already
trust, `zerock login --plaintext` skips TLS on the control connection — the token
then crosses the wire in the clear, which is why it is never a fallback.

Set `trust_proxy_headers: true` in this mode so client IPs in
`X-Forwarded-For` are believed. Leave it `false` when zerock faces the internet
directly, or callers can forge their own address.

## Building

```sh
make build      # ./bin/zerock      the full binary: CLI + server
make build-cli  # ./bin/zerock-cli  the client build, no server
make check      # vet + boundary + tests
make race       # tests under the race detector
make release    # ./dist/, static, linux amd64 and arm64,
                # plus SHA256SUMS. Either binary is self-contained: units,
                # config templates and the dashboard are embedded.
```

Two binaries are built from one tree:

| Built from | Artifact | Contains |
|---|---|---|
| `cmd/zerockcli` | `zerock-<os>-<arch>` | client verbs only |
| `cmd/zerock` | `zerock-server-<os>-<arch>` | client verbs + server |

The split is a package boundary, not a build tag. `internal/clientcli` holds the
commands that talk to a server, `internal/cli` adds the ones that *are* the
server, and `internal/cliutil` holds the plumbing both share. `make boundary`
fails if the client build ever reaches `internal/server` or `internal/store`
again — without it a stray import silently doubles the client and pulls ACME and
the database back in.

Tagging `v*` builds and publishes both, for every platform, via
`.github/workflows/release.yml`.

Requires Go 1.22 or newer to bootstrap; the module's `go` directive fetches the
toolchain it needs.

## Notes and limits

- **One label deep.** `api-x.yourdomain.com` works; `a.b.yourdomain.com` does not.
- **Cloudflare and DigitalOcean only** for automatic wildcard certificates.
  Other providers need `tls.mode: files` with a certificate from elsewhere, or a
  proxy in front. Adding one is a single entry in `dnsProviders` in
  `internal/server/tls.go` plus its `libdns` dependency.
- **One server, no clustering.** State is a single bbolt file. Two servers cannot
  share it.
- **TCP tunnels need open ports.** The public port comes from `tcp_port_range`,
  and that range has to be reachable through your firewall.
- **A request arriving mid-handshake waits** up to 3 seconds for the tunnel to
  become routable rather than 404ing, which keeps reconnects mostly invisible.
