# Configuration

whalenurse is configured with environment variables (`WHALENURSE_*`, on the daemon) and container labels (`whalenurse.*`,
on the containers it watches, see [labels.md](labels.md)). An unknown `WHALENURSE_*` name logs a warning; an invalid value
stops the start with an error that names the variable (the secrets among them are never quoted in it).

Durations take Go's `30s`, `5m`, `2h` and also `d` for days (`30d`). Booleans take `true` or `false`.

Variables marked **secret** are never logged, never in `status` or `/healthz`, and are masked in everything the dashboard and
the API show (see [security.md](security.md)). Put them in an env file that stays out of git.

## The healer

| Variable | Default | What it does |
|---|---|---|
| `WHALENURSE_MODE` | `label` | `label`: only containers with `whalenurse.enable=true`. `all`: every container except those labelled `whalenurse.enable=false`. whalenurse never acts on itself, on `docker compose run` containers or on swarm tasks, whatever the mode. |
| `WHALENURSE_DRY_RUN` | `false` | Log (and notify, marked `[dry-run]`) what it would do; change nothing. The uptime ping (`WHALENURSE_HEARTBEAT_URL`) is still sent. Also `whalenurse run --dry-run`. A dry run stores nothing and needs no `/data`. |
| `WHALENURSE_POLL_INTERVAL` | `60s` | The safety poll: it inspects every watched container, because events are only triggers (a failed restart emits no event, `health_status` fires once). |
| `WHALENURSE_GRACE` | `30s` | How long to wait after a trigger before inspecting and deciding (Docker's event order is unreliable). |
| `WHALENURSE_COOLDOWN` | `30s` | The wait after a rung before the re-inspect. It doubles per rung, capped at 5 minutes (a value above 5 minutes acts as 5 minutes). A label can override it per container. |
| `WHALENURSE_RECOVERY_PERIOD` | `10m` | How long a container must stay recovered before its incident closes (and a crash loop clears). |
| `WHALENURSE_FLAP_LIMIT` | `3` | More than this many incidents within `WHALENURSE_FLAP_WINDOW` SUSPENDS the container ("flapping"), even if every one recovered. A label can override it. |
| `WHALENURSE_FLAP_WINDOW` | `24h` | The window of the flap limit. Closing an incident does not reset the count; only the window expiring does. |
| `WHALENURSE_CRASHLOOP_AFTER` | `5m` | A crash loop is at least 3 `RestartCount` increases, the first at least this long ago and the latest less than this long ago. |
| `WHALENURSE_RATE_LIMIT` | `10` | Actions per hour, over all containers (a restart, a recreate and a pull each count). Stops an action storm during a host-wide outage; a notification says when it is reached. |
| `WHALENURSE_STARTUP_SETTLE` | `2m` | After start (and after every reconnect following a lost events stream) whalenurse waits this long, then sweeps everything. It is also the restore window in which a `start` is Docker restoring containers, not someone starting one. |
| `WHALENURSE_DRIFT_GUARD` | `strict` | `strict`: a recreate that would deploy a compose edit or a moved image tag is skipped and the container is SUSPENDED. `warn`: recreate anyway and say so, for *verified* drift only; a finding that says "can't verify" still blocks. |
| `WHALENURSE_COMPOSE_BIN` | — | The compose binary to use, ahead of everything else. If it does not work that is an error, not a reason to use another one. Lookup order otherwise: `/host/docker-compose` (the host plugin, mounted read-only), the bundled `/usr/libexec/whalenurse/docker-compose`, then under systemd the host's plugin directories. |

## State and identity

| Variable | Default | What it does |
|---|---|---|
| `WHALENURSE_HOST_ID` | the hostname | Carried in every container key (`host/project/service/1`), event and API record, for the future multi-host hub. Set it in a container, where the hostname is whalenurse's container ID and changes with every recreate (an upgrade); whalenurse warns at startup when it is. The state store's records (SUSPENDED, incidents, markers, flap and crash-loop history, learned parents) are moved to the current host ID at startup, so a change loses nothing; the event history keeps the host it was written with. |
| `WHALENURSE_DATA_DIR` | `/data` | The SQLite store: incidents, SUSPENDED, pauses, flap history, the event log, the notification dedupe. `run` refuses to start if it cannot open it; a dry run does not need it. |
| `WHALENURSE_HISTORY_RETENTION` | `30d` | Closed incidents and events older than this are pruned (hourly). Events are also capped at 100,000 rows. |
| `WHALENURSE_CONTROL_SOCKET` | `/run/whalenurse.sock` | The unix socket (mode 0600) that serves the CLI (`status`, `pause`, `resume`, `unsuspend`) and the image's health check. Under systemd, point it at a directory the service owns. |
| `WHALENURSE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on stderr. |

## Notifications

| Variable | Default | What it does |
|---|---|---|
| `WHALENURSE_NOTIFY_URLS` | — | **Secret.** One or more [Shoutrrr](https://nicholas-fedor.github.io/shoutrrr/) URLs (ntfy, Gotify, Discord, Telegram, Slack, Matrix, Pushover, a generic webhook and more), separated by whitespace or by a comma that is followed by `scheme://` (a comma inside a URL, as in `tags=warning,skull`, stays in it). Inside a URL, write a comma that is followed by `scheme://` as `%2C` (an ntfy `actions=view%2COpen%2Chttps://...` button) and a space as `%20`. An invalid URL is skipped and shown as `INVALID` in `status`. |
| `WHALENURSE_NOTIFY_TIMEOUT` | `10s` | One delivery attempt, per URL (two retries follow, after 2 s and 10 s). |
| `WHALENURSE_NOTIFY_RATE_LIMIT` | `20` | Messages per hour per URL. Routine messages may use half, important ones (giving up, flapping, drift blocked, upgraded) all of it; messages held back are counted and the next message says how many. |
| `WHALENURSE_HEARTBEAT_URL` | — | **Secret.** An uptime-monitor push URL (healthchecks.io, Uptime Kuma). Called with a GET at once and then every interval, only while whalenurse is healthy: when it is not, the ping stays quiet so that the monitor alarms by the missing ping. A dry run sends it too: a second instance (a dry run on another host, say) needs its own URL or none, or it keeps the live instance's monitor green. (The name is historical; in code and docs it is the "uptime ping".) |
| `WHALENURSE_HEARTBEAT_INTERVAL` | `5m` | The uptime ping's interval. |

## Private registries (the pull rung)

| Variable | Default | What it does |
|---|---|---|
| `WHALENURSE_REGISTRY_AUTH_DIR` | — | **The file in it is a secret.** A directory holding a `config.json` with inline `auths` and nothing else, mounted read-only. It is given to `docker compose pull` (the pull rung's pull) as `DOCKER_CONFIG` and to no other command. Any other key in the file (`credsStore`, `credHelpers`, `currentContext`, `proxies`, ...), a `contexts` entry in the directory, an entry without an inline credential, or an empty `auths` stops `run` at startup with a message that names the problem and never a value. The file is checked again before every pull. See [security.md](security.md#private-registries) and [deployment.md](deployment.md#private-registries-for-the-pull-rung). |

Check the setup with `docker exec whalenurse /whalenurse notify test`. See [troubleshooting.md](troubleshooting.md) if
nothing arrives.

## The dashboard and the API

Off unless `WHALENURSE_HTTP_ADDR` is set. Read-only: GET and HEAD only, no write endpoint. Read [security.md](security.md)
before you publish the port.

| Variable | Default | What it does |
|---|---|---|
| `WHALENURSE_HTTP_ADDR` | — (off) | The listener for the dashboard, `/api/v1`, `/healthz` and, if enabled, `/metrics`. In a container use `:8484` and publish it on `127.0.0.1:8484:8484` or give it to a reverse proxy or Tailscale on a shared network; under systemd use `127.0.0.1:8484`. |
| `WHALENURSE_HTTP_TOKEN` | — | **Secret.** When set, every request except `/healthz` needs it: `Authorization: Bearer <token>`, or HTTP Basic with the token as the password and any user name (how a browser sends it). Both are clear text: use them over TLS or on loopback only. Use at least 16 characters (`openssl rand -hex 24`); a shorter token logs a warning. Never read from the URL. |
| `WHALENURSE_HTTP_ALLOWED_HOSTS` | — | Without a token, only the Host names `localhost`, `127.0.0.1` and `[::1]` are served (a defence against DNS rebinding in a browser); list the other names you reach it by, comma or space separated (`nas.lan,192.168.1.5`). Ignored, with a warning, when a token is set. |
| `WHALENURSE_HTTP_TRUSTED_PROXIES` | — | Reverse proxies (CIDRs or single addresses, comma or space separated, for example `172.18.0.0/16`) whose `X-Forwarded-For` the wrong-token limit believes, so that it counts per real client instead of per proxy. The right-most address that is not itself a trusted proxy is the client. `0.0.0.0/0` and `::/0` are refused. Only matters with a token; ignored, with a warning, without one. See [security.md](security.md#the-wrong-token-limit-and-reverse-proxies). |
| `WHALENURSE_METRICS` | `false` | `true` serves Prometheus `/metrics` on that listener, behind the token. |

`/healthz` is open (no token, no Host check) and answers only `ok` or `not ok` (503) on the TCP listener. The reason is in
`/api/v1/status` and in `whalenurse status`.

## Versions it needs

Fixed, not configurable: Docker Engine 25 for rung 1 (restart); Engine 28.1 and Compose v5.2.0 for rungs 2 and 3 and the
drift guard. Checked at startup and again after the events stream comes back; below that, one warning, rungs 2 and 3 are
disabled, and a container that a restart does not fix is SUSPENDED after the restart. `WHALENURSE_DRIFT_GUARD=warn` does not
bypass this.
