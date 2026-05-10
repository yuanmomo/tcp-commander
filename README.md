# tcp-commander

A small remote command execution daemon. Plain TCP, newline-delimited JSON.
You configure a list of allowed binaries; the daemon shell-tokenizes each
incoming command line and executes it as long as `argv[0]` is in the list.

**Personal-use mode.** There is no token authentication and no TLS — the
only access control is an IP allowlist. Run only on a network you trust.

## Build

```sh
make                # ./bin/tcpcommanderd for your host
make linux          # ./dist/linux-amd64 + ./dist/linux-arm64
make test           # go test ./...
make install        # install to $(PREFIX)/bin (default /usr/local/bin)
```

Or directly with Go:

```sh
go build -o tcpcommanderd ./cmd/tcpcommanderd
```

## Run

```sh
tcpcommanderd --config /etc/tcp-commander/config.yaml
# or
CMD_CONFIG=/etc/tcp-commander/config.yaml tcpcommanderd
```

Listens on `:9000` by default. `SIGTERM` / `SIGINT` triggers a graceful
shutdown that finishes in-flight requests before exiting.

## Protocol

One TCP connection per session. Each line is a single JSON message
terminated by `\n`. A connection may carry many sequential requests, and
each request has a unique `id` echoed back on the response — so a client
may dispatch several requests in parallel over one connection.

### Request

```json
{"id": "abc123", "cmd": "docker compose -f /opt/myapp/compose.yaml pull"}
```

`cmd` is the full command line, written exactly as you would type it in a
shell. The daemon **does not invoke a shell** — `cmd` is parsed into argv
with shellwords (handles quotes, escapes), then `argv[0]` is matched against
`allow_command_list`, and the program is exec'd directly. Shell metacharacters
like `;`, `|`, `$(...)`, backticks, globs, and env-var substitution **are
literal characters** to the program, not interpreted by anything.

Optional fields:

- `"stream": true` — switch to line-by-line streaming output (see below).
- `"timeout": "20m"` — override the per-binary timeout for this single
  request. Bounded by `defaults.max_timeout` in config; values past the cap
  are rejected. Useful for one-off long jobs without editing the daemon
  config.

### Response

```json
{"id": "abc123", "rc": 0, "stdout": "...", "stderr": "...", "elapsed_ms": 42}
```

### Error response

```json
{"id": "abc123", "error": "command not in allow list: rm"}
```

The connection is closed only on `invalid json` / `missing id`. An
allow-list miss returns the error and keeps the connection open.

### Streaming

Add `"stream": true` to a request to receive output line-by-line:

```json
> {"id": "t1", "cmd": "tail -F /var/log/syslog", "stream": true}
< {"id": "t1", "stream": "stdout", "data": "Jan 14 12:00:01 host kernel: ...\n"}
< {"id": "t1", "stream": "stdout", "data": "Jan 14 12:00:02 host CRON ...\n"}
< ...
< {"id": "t1", "rc": -1, "elapsed_ms": 1800000}            ← timeout fires
```

The final frame always carries `rc` and `elapsed_ms`. Long-running streams
are stopped by the per-binary timeout from the config.

While a streaming command runs, the daemon also emits periodic heartbeat
frames so an idle stream still keeps the socket warm:

```json
< {"id": "t1", "heartbeat": true}
```

Clients should treat heartbeat frames as a no-op liveness signal. The
interval is set by `defaults.heartbeat` (default 30s, 0 disables).

### Built-in `ping`

`ping` is always allowed and needs no auth. Useful for liveness probes.

```json
> {"id": "1", "cmd": "ping"}
< {"id": "1", "rc": 0, "stdout": "tcp-commander dev uptime=1m23s", "elapsed_ms": 0}
```

### `nc` example

```sh
$ nc 10.0.0.5 9000
{"id":"1","cmd":"df -h /"}
{"id":"1","rc":0,"stdout":"Filesystem ...","elapsed_ms":12}

{"id":"2","cmd":"docker compose -f /opt/myapp/compose.yaml pull"}
{"id":"2","rc":0,"stdout":"...","elapsed_ms":45213}

{"id":"3","cmd":"bash /opt/scripts/deploy.sh prod v1.2.3"}
{"id":"3","rc":0,"stdout":"deploy ok\n","elapsed_ms":12000}
```

## Config

See `examples/config.yaml`. The whole schema:

```yaml
listen: ":9000"

allow_cidr:                          # source-IP allowlist (the only access control)
  - 127.0.0.1/32
  - 10.0.0.0/8

log_file: /var/log/tcp-commander.log # optional; stdout always logs

defaults:
  timeout: 30s
  max_concurrent: 4
  max_timeout: 1h          # cap for per-request `timeout` overrides
  heartbeat: 30s           # streaming-mode liveness frames
  kill_grace: 10s          # SIGTERM → SIGKILL grace on cancel/timeout
  keepalive: 30s           # TCP keepalive on accepted connections
  output_cap_bytes: 4194304   # 4 MiB cap per channel for non-streaming

allow_command_list:                  # programs allowed at argv[0]
  - df
  - docker
  - docker-compose
  - top
  - tail
  - bash

limits:                              # optional per-binary overrides
  docker:
    timeout: 10m
    max_concurrent: 1
  bash:
    timeout: 10m
```

Notes:
- Entries in `allow_command_list` may be bare names (`docker`) or absolute
  paths (`/usr/bin/docker`). Matching is by basename, so `docker ...` and
  `/usr/bin/docker ...` both work either way.
- Bare names are resolved against the daemon's `$PATH` via `exec.LookPath`.
- `defaults.timeout` (default 30s) applies to commands without an override.
- `defaults.max_concurrent` of 0 means unlimited.
- When `max_concurrent` is reached, the daemon returns
  `max concurrent reached` instead of queueing.
- `defaults.max_timeout` (default 1h) caps the per-request `timeout`
  field. Set to `0` to forbid per-request overrides entirely.
- `defaults.kill_grace` (default 10s) is the window between SIGTERM and
  SIGKILL when a command is cancelled (timeout, server shutdown, or
  client disconnect) — long enough for deploy scripts to clean up.
- `defaults.output_cap_bytes` (default 4 MiB) limits how much stdout/stderr
  a non-streaming run will buffer in memory. Output past the cap is
  replaced with a `[tcp-commander: output truncated]` marker. Use
  `"stream": true` for commands that produce more output than this.

### Long-running commands (minutes-scale)

For commands that take minutes (deploys, image pulls, etc.):

- Set a per-binary `limits.<name>.timeout` larger than the expected runtime,
  or pass a per-request `"timeout": "20m"` (bounded by `max_timeout`).
- Prefer `"stream": true` — you get live output, you find out about errors
  immediately, and heartbeat frames keep the socket warm through NATs and
  stateful firewalls.
- If the client disconnects, the daemon now cancels the running command
  and frees the concurrency slot (rather than running it to completion
  against a dead client).

### Running bash scripts

Add `bash` to `allow_command_list` and run scripts as you would locally:

```json
{"id":"d1","cmd":"bash /opt/scripts/deploy.sh prod v1.2.3"}
```

For long deploys, set `"stream": true` to tail the output live:

```json
{"id":"d1","cmd":"bash -x /opt/scripts/deploy.sh prod","stream":true}
```

## Logging

JSON to stdout (perfect for `journald` under systemd), and optionally
duplicated to `log_file`. Each request emits one record with `remote`,
`id`, `cmd`, `prog`, `rc`, and `elapsed_ms`.

## systemd

`examples/tcpcommanderd.service` is included. Install:

```sh
sudo useradd --system --shell /usr/sbin/nologin --home /var/lib/tcp-commander cmdrunner
sudo install -Dm755 bin/tcpcommanderd          /usr/local/bin/tcpcommanderd
sudo install -Dm640 examples/config.yaml       /etc/tcp-commander/config.yaml
sudo install -Dm644 examples/tcpcommanderd.service /etc/systemd/system/tcpcommanderd.service
sudo systemctl daemon-reload
sudo systemctl enable --now tcpcommanderd
sudo journalctl -u tcpcommanderd -f
```

The unit runs as `cmdrunner`, never as root. To let it use Docker, add it
to the `docker` group (note: that membership is effectively root on the
host — keep `allow_cidr` tight):

```sh
sudo usermod -aG docker cmdrunner
sudo systemctl restart tcpcommanderd
```

## Security model — read this

There is **no authentication**. Anyone who can open a TCP connection to
the listen port and pass the IP allowlist can run any binary in
`allow_command_list` with any arguments. With `bash` or `docker`
whitelisted, that is effectively root on the host.

Mitigations baked in:
- No shell is ever invoked. Argv tokenization rejects unterminated quotes
  rather than executing anything.
- Per-binary timeouts and concurrency caps.
- IP allowlist via `allow_cidr`.
- Daemon runs as a dedicated non-root user under systemd.

If your network is not trusted, do not use this build.

## Non-goals

No pipes / redirects / globs / env-substitution (no shell), no file
transfer, no PTY / interactive sessions, no web UI, no auth.
