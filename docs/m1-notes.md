# M1 notes

Decisions, deviations and open points for milestone M1 (restart-only heal MVP) of [PLAN.md](PLAN.md). Most items fill a gap the plan leaves open. Two were deviations from the plan text, **approved by the maintainer on 17 Oct 2026 and recorded in PLAN v3.2**: decision 2 (§3.2, external `start`) and decision 11 (§3.3 rule 3, crash-loop detection). M2 replaces M1's crash-loop code with the stricter rule in PLAN §3.3 (≥3 `RestartCount` increases, the first ≥ `CRASHLOOP_AFTER` ago, cleared after `RECOVERY_PERIOD`).

## Upstream check (17 Oct 2026)

| Item | Version | Source |
|---|---|---|
| Go | 1.27.2 (latest stable; 1.27.1 was installed locally, `go.mod` says `go 1.27.2`) | go.dev/dl |
| `github.com/moby/moby/client` | v0.6.2 (8 Oct) | tag `client/v0.6.2` |
| `github.com/moby/moby/api` | v1.56.1 (1 Oct); the plan does not name it | tag `api/v1.56.1` |
| `golang` image | `1.27.2` exists on Docker Hub | |
| Actions | checkout v7.0.1, setup-go v7.0.0 | releases |

Client API as used: `client.New(client.FromEnv)` (negotiation is the default), `Events()` returns one `EventsResult`, `ContainerList/Inspect/Restart/Start`. `Moby` in `internal/docker` is the only importer.

## What M1 added beyond the plan's package list

- `internal/daemon`: wires events, poll, startup sweep, the pre-action guard and the actor. The engine stays pure, so something has to drive it.
- `internal/clock`: the injected clock (real and fake).
- `internal/control`: a unix socket serving `/healthz` (M2 adds the CLI commands). The image has no shell, so its `HEALTHCHECK` runs `whalenurse healthcheck`, which needs something to ask when `WHALENURSE_HTTP_ADDR` is empty. New variable `WHALENURSE_CONTROL_SOCKET` (default `/run/whalenurse.sock`); added to the §4 table.

## Decisions

1. **Minimal incident state.** Without it the 60s poll would restart an unhealthy container forever. One restart per incident, the cooldown, a re-inspect, then `GaveUp` (in memory). The state clears on recovery (`Recovered`), on an external `create`, or on a `start` after a manual-stop marker. This stands in for M2's SUSPENDED. There is no RECOVERY_PERIOD (the incident closes at the first healthy re-inspect) and no flap limit, so a container that flaps is restarted at most `RATE_LIMIT` times an hour.
2. **DEVIATION from §3.2: a plain external `start` does not clear `GaveUp`.** The plan says an external `start`/`create` does. A restart-policy restart is also a `start`, so honouring it would re-arm the ladder on every crash-loop cycle. M1 clears on `create`, or on a `start` after a kill marker (someone stopped it, someone started it). Revisit with SUSPENDED in M2.
3. **Manual-stop markers are in memory** (M2 persists them). They exist now because the action-in-flight window must not set them, and because `crashed` mode needs them: an app that exits 1 on SIGTERM would otherwise be restarted after a `docker stop`.
4. **The rules cover all of §3.3**, including `heal-exited` and `State.Error` classification (which §7 also lists under M2). They are pure, fixture-tested, and M1 maps every heal to `restart`/`start`.
5. **Rule details the plan leaves open:**
   - `heal-exited=always` heals exit 0 even under policy `no`; the "intended exit" check applies to `crashed` and `never` only.
   - In `crashed` mode with no event history, exit 0 under `always`/`unless-stopped` is left alone.
   - `on-failure:N` exhausted heals regardless of `heal-exited`.
   - `docker pause`d containers are skipped; `removing` is skipped; `dead` is notify-only.
   - A container still `restarting` is measured from when the poll first saw it, not from `StartedAt`.
6. **Dependents.**
   - Parent ID gone: `NeedsRecreate` (event + log), never restarted, even when unhealthy. This is enforced twice: in `rules` (all of `network_mode`, `ipc`, `pid`) and again in the daemon right before the API call, because the parent can be recreated between decision and action.
   - Parent not running or busy: the dependent waits; its pass never acts on the parent. Parents are processed before dependents in a batch.
   - Running dependent whose parent started later (staleness condition 2): recorded as `NeedsDependentRestart`, **not** restarted. The plan puts the dependents step in M2; M1 only does the two things the brief asked for.
   - A failed start with `cannot join network namespace of a non running container` becomes a plain `start` once the parent is running under the same ID.
7. **"Notify" in M1** is the structured log line plus the domain event, both from one subscriber, so they cannot disagree. Shoutrrr is M3.
8. **Dry-run is structural:** `run --dry-run` wires a no-op `docker.Actor`. The Docker `Reader` and `Actor` are separate interfaces.
9. **Fixtures carry no `whalenurse.enable` label**; the rules tests inject opt-in. A test fails if a file in `testdata/inspect` or `testdata/events` has no expected classification, or an expectation names a missing file.
10. **The events stream** reconnects with `since=<last event time>` and exponential backoff to 30s. `/healthz` fails after the stream has been down for 2x the poll interval (or the loop is silent for 3 polls plus the action timeout; plan v3.13 item 13 adds a Docker API failing for 2x the poll interval).
11. **DEVIATION from §3.3 rule 3: crash loops are detected by a rising `RestartCount`, not only by `restarting`.** The rule says "`restarting` for longer than CRASHLOOP_AFTER". Moby's restart manager (`daemon/internal/restartmanager`, docker-v29.9.1) resets its backoff to 100ms whenever a run lasted 10s or more, so a container that crashes every 15s is almost always `running`, and a loop of shorter runs alternates `running`/`restarting`. A timer that restarts whenever the container is seen `running` never fires for either. M1 therefore starts the loop clock when a container is seen `restarting` **or** its `RestartCount` is higher than at the previous inspect, keeps it through `running` observations, and clears it once the count has been steady for GRACE. It is fed by `die` events and by the poll. Tests: `TestFastCrashLoopNeverSeenRestarting`, `TestCrashLoopThatAlternatesRunningAndRestarting`, `TestOneRestartIsNotACrashLoop`. M2's recovery period and flap limit should be designed around the same fact: `RestartCount` and `die` events, not `restarting`, are the loop signal. Approved; M2 uses the inspect-rate-independent definition in PLAN v3.2.

## Integration tests and safety

- They are behind `-tags integration` **and** `WHALENURSE_INTEGRATION=1`, so `go test ./...` never touches Docker.
- `test/integration/guard.go` ports `test/m0/lib.sh`: project regex `^whalenurse-test-[a-z0-9][a-z0-9_-]*$`, `-p`/`down`/`rm` smuggling refused, label lookup before every container mutation, `DOCKER_HOST`/context checks, an image-pull allowlist (`busybox:1.37`), and a preflight that fails if any container outside a matching project carries `whalenurse.enable=true` or `WHALENURSE_MODE` is not `label`. The in-process healer under test gets its actor wrapped in the same guard. The guard has its own Docker-free unit tests (mutating the regex makes them fail).
- The containerised image is tested by `TestImage`; it cannot be wrapped, so the preflight is its protection.
- Scenarios: unhealthy → restarted, same ID, `Recovered`; dry-run leaves ID/`StartedAt`/health untouched; E1 end to end (an unhealthy `network_mode` dependent whose parent was recreated stays `running` and untouched, one `NeedsRecreate`); the built binary's `run --dry-run` logs the expected decisions and `healthcheck` passes; the image runs read-only with `group_add` and passes its own `HEALTHCHECK`.
- **Not in M1:** the compose-version × image-store matrix (§6), which belongs with M0b/M2 when compose is driven by whalenurse. The CI runner used here has Docker 28.0.4, overlay2 (classic store) and compose v2.38.2.

## Scripts

`.shellcheckrc` (`external-sources`, `source-path=SCRIPTDIR`) makes `shellcheck -x scripts/ test/` pass from the repo root, and `test/m0/run.sh` got one `SC2317` suppression for its stub function. No behaviour change.
