# Model proxy on the Mac Studio

This fork of CLIProxyAPI runs on the Mac Studio (`modus-studio`, user `aiserver`) as the
shared model proxy for Eric's agents. It signs in to Claude and ChatGPT (Codex)
subscriptions, keeps their tokens fresh, and serves an Anthropic- and OpenAI-compatible
API on port 8317.

The fork adds two features to upstream:

- **`routing.strategy: reset-first`**: among the accounts eligible for a request, use the one
  whose weekly window (Claude 7-day, or the 7-day window for the requested model; Codex's
  longest window) resets soonest. Quota that would otherwise expire unused is spent first.
  Accounts whose window is exhausted are tried last. Accounts with no known reset follow
  fill-first order. An existing session binding still wins (session affinity).
- **Credential pools**: each client API key is bound to a pool of accounts, and this is
  enforced on the server (see "Pools").

## Layout

| What | Where |
|---|---|
| Binary | `~/model-proxy/bin/cli-proxy-api` |
| Config (holds keys, mode 600, never committed) | `~/model-proxy/config.yaml`, rendered from `config.template.yaml` |
| Key values used for rendering (mode 600) | `~/model-proxy/keys.env`; the source of truth is Infisical |
| Signed-in accounts (mode 700) | `~/model-proxy/auths/` |
| Dashboard | `~/model-proxy/panel/management.html` (served via `MANAGEMENT_STATIC_PATH`) |
| Logs | `~/model-proxy/logs/` |
| Proxy service | `~/Library/LaunchAgents/systems.aeron.model-proxy.plist` |
| Tunnel service | `~/Library/LaunchAgents/systems.aeron.model-proxy-tunnel.plist` |
| Tunnel token (mode 600) | `~/.cloudflared/model-proxy.token` |

Keys live in Infisical, project `shared-credentials`, env `prod`:
`MODEL_PROXY_MANAGEMENT_KEY`, `MODEL_PROXY_KEY_STUDIO_CLAUDE_CODE`, `MODEL_PROXY_KEY_AIR_CLAUDE_CODE`,
`MODEL_PROXY_KEY_T3`, `MODEL_PROXY_KEY_HYDRON_ENGINE`, `MODEL_PROXY_KEY_MUNDER_DIFFLIN`,
`MODEL_PROXY_CF_ACCESS_CLIENT_ID`, `MODEL_PROXY_CF_ACCESS_CLIENT_SECRET`, `MODEL_PROXY_TUNNEL_TOKEN`.

## Addresses

- Tailnet: `http://100.95.87.16:8317` or `http://modus-studio.tail3f016a.ts.net:8317`
- Cloudflare (for Hydron's engine): `https://models.aeron.systems`, behind a Cloudflare
  Access app that admits only the service token `hydron-engine-model-proxy`. Send
  `CF-Access-Client-Id` and `CF-Access-Client-Secret` plus the proxy key.
- Dashboard: `http://100.95.87.16:8317/management.html` (management key required).

Every request needs a client key: `Authorization: Bearer <key>` or `x-api-key: <key>`.
Pointing Claude Code at it: `ANTHROPIC_BASE_URL=http://100.95.87.16:8317` and
`ANTHROPIC_AUTH_TOKEN=<key>`. Codex: an OpenAI provider with base URL
`http://100.95.87.16:8317/v1` and the key.

## Install (or rebuild elsewhere)

Needs Go (`brew install go`) and `/opt/homebrew/bin` on PATH.

1. Clone `aeron-systems/CLIProxyAPI` and add upstream:
   `git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git`
2. Create `~/model-proxy/keys.env` (mode 600) with the `MODEL_PROXY_*` lines named above,
   from Infisical. Only the key lines are used for the config; the Cloudflare ones are not.
3. For the tunnel, put the tunnel token in `~/.cloudflared/model-proxy.token` (mode 600).
   On a new machine, run the tunnel on one machine only.
4. Run `deploy/studio/install.sh`. It builds, renders `config.yaml` if missing, installs both
   plists and starts them. `install.sh --render` re-renders the config from the template.

Both plists run through `/bin/sh -c`: a non-system binary started directly by launchd can
lose LAN routes on this Mac.

## Upgrade

```sh
cd ~/Development/Eric/cliproxyapi
git fetch upstream && git rebase upstream/main
go test ./sdk/cliproxy/... ./internal/api/... ./internal/config/...
deploy/studio/install.sh
git push --force-with-lease origin main
```

The dashboard is replaced by copying a new `management.html` into `~/model-proxy/panel/`;
no restart. Background panel updates from GitHub are off (`disable-auto-update-panel: true`).

## Pools

Config lives under `routing:` in `config.yaml` (template: `config.template.yaml`).

- `pools`: named lists of accounts. Name an account by its auth file name (as listed in
  `~/model-proxy/auths/`) or its account email.
- `reserved: true`: that pool's accounts serve only that pool. They are excluded from every
  other pool, including `shared`. An account can be reserved by one pool only.
- `shared` is implicit: every account not reserved by some pool.
- `pool-clients`: binds each client key to a pool. Keys not listed use `default-pool` (`shared`).
- `fallback`: optional, per pool. When a pool has no usable account (exhausted, cooling down,
  or none signed in) the request fails with `pool_exhausted` naming the pool. It only moves
  to another pool if that pool's `fallback` names one. The default is no fallback.
- Retries, failover and session affinity all stay inside the pool.
- `GET /v8/management/pools` (or `/v8/management/routing/pools`) lists each pool with its
  masked keys and the signed-in accounts it may use, plus each account's pools. The
  dashboard reads this endpoint.
- Every routed request is logged as `credential pool: routed request` with the client
  name, pool and account.

Current bindings:

| Key | Pool |
|---|---|
| studio-claude-code, air-claude-code, t3 (Eric's own work, "dev") | `shared` |
| hydron-engine | `shared` for now. Eric decides: shared, or its own `hydron-engine` pool (reserved or not) |
| munder-difflin | `munder-difflin`, reserved. Fails with `pool_exhausted` until its account is listed |

After signing an account in, edit `~/model-proxy/config.yaml`. For example, to reserve one
account for Munder Difflin:

```yaml
  pools:
    - name: "munder-difflin"
      reserved: true
      credentials: ["claude-md@example.com.json"]
```

The proxy reloads the config on save. Make the same change in `config.template.yaml` so a
re-render keeps it. Account names are not secret, so they can be committed.

## Add an account (needs a browser)

Logins run on the Studio, but the browser runs on the Air. An SSH tunnel carries the
OAuth callback back to the Studio. Run on the Air:

Claude (Pro/Max):

```sh
ssh -t -L 54545:127.0.0.1:54545 aiserver@100.95.87.16 \
  '~/model-proxy/bin/cli-proxy-api -config ~/model-proxy/config.yaml -claude-login -no-browser'
```

Codex (ChatGPT):

```sh
ssh -t -L 1455:127.0.0.1:1455 aiserver@100.95.87.16 \
  '~/model-proxy/bin/cli-proxy-api -config ~/model-proxy/config.yaml -codex-login -no-browser'
```

(or `-codex-device-login`, which needs no tunnel: open the printed link and enter the code).

Open the printed URL in the Air's browser and sign in with the account. The callback reaches
the Studio through the tunnel, and the account file lands in `~/model-proxy/auths/`. The
running proxy picks it up within seconds. Repeat for each account. Then add it to a pool if
it should not be shared.

The dashboard's OAuth page works too. It starts the same callback listener on the Studio, so
the same `ssh -L` tunnel must be open while you sign in.

## Check it works

```sh
curl -s http://100.95.87.16:8317/v1/models -H "Authorization: Bearer <key>"
curl -s http://100.95.87.16:8317/v8/management/pools -H "Authorization: Bearer <management key>"
```

With no accounts signed in, a model request answers `unknown provider for model ...`.

## Dashboard over HTTPS on the tailnet

Browsers force HTTPS on ts.net names, so the dashboard is served by Tailscale on port 8318:
`/Applications/Tailscale.app/Contents/MacOS/Tailscale serve --bg --https=8318 http://127.0.0.1:8317`
Open https://modus-studio.tail3f016a.ts.net:8318/management.html (tailnet only).
