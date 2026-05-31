# tcp-commander

A personal-use remote command execution daemon. Plain TCP, newline-delimited JSON.
No auth, no TLS — IP CIDR allowlist is the only access control.

## Project layout

```
cmd/tcpcommanderd/main.go        entry point, flags, signal handling
internal/config/config.go        YAML config parsing + validation
internal/server/server.go        TCP listener, per-connection goroutines, request dispatch
internal/server/protocol.go      wire types (Request / Response JSON structs)
internal/command/command.go      process execution, allowlist, timeout, concurrency
internal/logging/logging.go      structured JSON logging + lumberjack rotation
examples/config.yaml             reference config with every knob
scripts/deploy.sh                remote deploy over SSH
```

## Build commands

```sh
make              # ./bin/tcpcommanderd for host arch
make linux        # cross-compile dist/linux-amd64 + dist/linux-arm64
make test         # go test ./... -count=1
make vet          # go vet ./...
make fmt          # go fmt ./...
make clean        # rm bin/ dist/
```

## Testing

Tests live alongside their packages:
- `internal/command/command_test.go`
- `internal/config/config_test.go`
- `internal/server/server_test.go`

Run a single package: `go test ./internal/server/ -v`

## Protocol

One TCP connection per session. Each line = one JSON message terminated by `\n`.

Request: `{"id": "abc", "cmd": "docker compose pull", "stream": true, "timeout": "10m"}`
Response: `{"id": "abc", "rc": 0, "stdout": "...", "stderr": "...", "elapsed_ms": 42}`
Error: `{"id": "abc", "error": "command not in allow list: rm"}`
Stream frame: `{"id": "abc", "stream": "stdout", "data": "line\n"}`
Heartbeat: `{"id": "abc", "heartbeat": true}`

Built-in: `{"id":"1","cmd":"ping"}` — always allowed, returns uptime.

## Key invariants

- No shell is ever invoked. `cmd` is parsed with shellwords into argv; `argv[0]` must match `allow_command_list` by basename.
- Shell metacharacters (`;`, `|`, `$(...)`, globs) are literal — passed to the program, not interpreted.
- `allow_cidr` is the only access control; anyone in the CIDR can run any allowed binary.
- `defaults.max_timeout` caps per-request `timeout` overrides.
- Non-streaming output is capped at `output_cap_bytes` (default 4 MiB) per channel.
- Client disconnect cancels the running command and releases the concurrency slot.

## Dependencies

- `github.com/mattn/go-shellwords` — shell-word tokenization
- `gopkg.in/yaml.v3` — config parsing
- `gopkg.in/natefinch/lumberjack.v2` — log file rotation
