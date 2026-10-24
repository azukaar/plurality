# Deploying Plurality

Plurality ships as a single Docker image containing the Go server, the Flutter web UI,
and a bundled LiteLLM proxy. The server listens on **port 8090** and serves both the API
and the web app from there.

## Run it

From Cosmos cloud, install in one click: [install](https://cosmos-cloud.io/proxy#cosmos-ui/market-listing/plurality)

Or from your server's terminal (This assumes you are using Open Router, see details on how to setup your AI models accesses)

```bash
docker run -d --name plurality \
  -p 8090:8090 \
  -v plurality-users:/app/users-data \
  -v plurality-data:/app/data \
  -e OPENROUTER_API_KEY=xxxxx \
  -e INIT_ADMIN_USER=admin \
  -e INIT_ADMIN_PASSWORD=change-me \
  ghcr.io/aseracorp/plurality
```

```yml
services:
  plurality2:
    image: ghcr.io/aseracorp/plurality
    container_name: plurality2
    ports:
      - "8090:8090"
    volumes:
      - plurality-data:/app/data
      - plurality-users:/app/users-data
    environment:
      - OPENROUTER_API_KEY=xxxxx
      - INIT_ADMIN_USER=admin
      - INIT_ADMIN_PASSWORD=change-me
    restart: unless-stopped

volumes:
  plurality-data:
  plurality-users:
```


Open <http://<ip>:8090> or domain.com and log in. Until you configure at least one model, the
assistant has nothing to talk to — do that next.

### Volumes

The image declares three volumes; mount them so data survives upgrades:

| Path | Holds |
|------|-------|
| `/app/users-data` | Per-user data: conversations (SQLite), uploads, schedules, webhooks, memory, per-user skills/MCP. |
| `/app/data` | Server config (`config.json`), `litellm_config.yaml`, global skills and presets. |
| `/root` | The assistant's working home directory (where it runs shell commands / builds things). |

> The assistant is told that only its persistent volume (`PERSIST_VOL`, default `/home/`)
> plus its skills and MCP config survive a restart. If you mount the working home
> somewhere other than the default, set `PERSIST_VOL` to match so it advertises the
> right path.

## Configure models (required)

The server has **no hardcoded models, provider URLs, or keys** — everything comes from
`data/litellm_config.yaml`. Edit that file to declare which models exist, what each can
do, and the provider key behind it, then restart. This is the one piece of setup the
assistant can't work without.

See **[litellm.md](litellm.md)** for the full format and examples.

## Create users

Two options, not mutually exclusive:

**Seed an admin from the environment** — set `INIT_ADMIN_USER` and `INIT_ADMIN_PASSWORD`
(as in the run command above); the server creates that account on boot if it's missing.

**Manage users from the CLI** — run the binary with a subcommand inside the container:

```bash
docker exec -it plurality /app/Plurality adduser <name>
docker exec -it plurality /app/Plurality listusers
docker exec -it plurality /app/Plurality removeuser <name>
```

## Single sign-on (OpenID)

Set `OPENID_ISSUER` to enable OpenID/OIDC login automatically (alongside local
accounts). The client drives the OAuth flow with PKCE, so **no client secret is needed**
— register a *public* client with your provider.

```
OPENID_ISSUER=https://auth.example.com/application/o/plurality/
OPENID_CLIENT_ID=plurality
OPENID_NAME=Login with Authentik       # button label
OPENID_ALLOWLIST=*@example.com,alice    # optional; empty = allow any authenticated user
```

Full provider setup (Authentik, Keycloak, Auth0, Google, …) is in **[openid.md](openid.md)**.

## Environment variables

Env vars override anything in `config.json`.

| Variable | Purpose |
|----------|---------|
| `INIT_ADMIN_USER` / `INIT_ADMIN_PASSWORD` | Seed an admin account on first boot. |
| `JWT_SECRET` | Signing secret for auth tokens. Auto-generated and persisted if unset. |
| `LITELLM_URL` | Use an external LiteLLM proxy instead of the bundled one (default `http://127.0.0.1:4000`). |
| `EMBEDDING_MODEL` | Model used to build search embeddings. |
| `DATA_DIR` | Override the server config/data directory (default `data/` next to the binary). |
| `USER_DATA_STORAGE` | Override the per-user data root (default `users-data/`). |
| `PERSIST_VOL` | Path advertised to the assistant as persistent across restarts (default `/home/`). |
| `OPENID_ISSUER` / `OPENID_CLIENT_ID` / `OPENID_NAME` / `OPENID_ALLOWLIST` | OpenID login (see above). `OPENID_BTN_COLOR` / `OPENID_BTN_BG1` / `OPENID_BTN_BG2` style the login button. |
| `WEBHOOK_EXT` | Public domain to advertise for external webhooks (so the assistant builds correct URLs). |
| `PORT_EXT` | Ports exposed externally, advertised to the assistant so it knows where it may bind servers. |
| `NTFY_URL` / `NTFY_TOPIC` / `NTFY_TOKEN` | Enable the NTFY-based notification tool (needs URL + topic). |
| `GOOGLE_SEARCH_API_KEY` / `GOOGLE_SEARCH_ENGINE_ID` | Credentials for web search. |
| `NEWS_API_KEY` | Credential for the news tool. |
| `MAX_LOOP_ITERATIONS` | Per-request LLM iteration budget for the runaway-loop guard in `RunLLMLoop` (default `150`; the guard pauses a turn that exceeds this with no further user input). Tune up for unusually long autonomous workflows, or down if a conversation is observed spinning. |
| `LOG_LEVEL` | Set to `DEBUG` for verbose logs. |

## Behind a reverse proxy

Terminate TLS at your proxy (Caddy, Nginx, Traefik, Cosmos…) and forward to port 8090.
Plurality streams responses over Server-Sent Events, so disable response buffering on
the proxied routes or streaming will stall. If you forward client IPs via
`X-Forwarded-For`, the per-IP webhook rate limits will see the real source.

## Models the assistant can bind to

When the assistant runs servers or long-lived processes for you, it only persists work
under the persistent volume and its skills/MCP config. Use `PORT_EXT` to tell it which
ports are actually reachable from outside, and `WEBHOOK_EXT` to give it the public
domain for webhooks — otherwise it assumes `localhost:8090`.
