# tcp-commander client instructions

How any client (call it "openclaw" or otherwise) should send requests to a
running tcp-commander daemon. The protocol is small enough to fit on a page.

## 1. Connect

- **Address:** `tcp://<host>:9000` (port from `listen:` in config). Plain TCP
  — no TLS, no auth.
- **Source IP must be in `allow_cidr`.** If not, the daemon closes the
  socket immediately. So either run the client from inside `127.0.0.1/32`,
  `10.0.0.0/8`, or `192.168.0.0/16` (per the default config), or add its
  CIDR to the allowlist on the server.
- **One connection can serve many requests.** Keep it open; don't reconnect
  per call.

## 2. Send: one JSON object per line

Every request is a single line: a JSON object terminated by `\n`. Required
and optional fields:

| field     | type   | required | notes                                                                                                  |
| --------- | ------ | -------- | ------------------------------------------------------------------------------------------------------ |
| `id`      | string | yes      | unique per in-flight request on this connection; echoed in every response frame                        |
| `cmd`     | string | yes      | full command line, exactly as you'd type it in a shell (no shell is invoked — `;`, `\|`, `$(...)`, globs are literal) |
| `stream`  | bool   | no       | if `true`, stdout/stderr stream back line-by-line                                                      |
| `timeout` | string | no       | Go duration (`"15m"`, `"2h"`); overrides per-binary timeout up to `defaults.max_timeout`               |

`argv[0]` (after shell-tokenization) **must** be in `allow_command_list`,
otherwise you get an error response.

## 3. Receive: one or more JSON frames per request

**Non-streaming** — exactly one response frame:

```json
{"id":"abc","rc":0,"stdout":"...","stderr":"...","elapsed_ms":42}
```

`rc = -1` with stderr ending in `tcp-commander: timeout after Xs` means the
per-binary / per-request timeout fired. `rc = -1` with
`tcp-commander: cancelled` means the daemon cancelled the run (usually
because the connection dropped).

**Streaming** — many frames, then one final frame:

```json
{"id":"t1","stream":"stdout","data":"line one\n"}
{"id":"t1","stream":"stderr","data":"warn ...\n"}
{"id":"t1","heartbeat":true}
...
{"id":"t1","rc":0,"elapsed_ms":612345}
```

Heartbeat frames (`{"id":"...","heartbeat":true}`) arrive every ~30s while
the command runs — treat them as a no-op liveness signal. The final frame
is the one with `rc` and `elapsed_ms`.

**Errors** (e.g. allow-list miss, parse error, invalid timeout):

```json
{"id":"abc","error":"command not in allow list: rm"}
```

The connection stays open after these — you can keep sending requests.

## 4. Liveness check

The daemon always allows `ping` regardless of `allow_command_list`. Use it
for health checks:

```json
> {"id":"hc","cmd":"ping"}
< {"id":"hc","rc":0,"stdout":"tcp-commander dev uptime=1m23s","elapsed_ms":0}
```

## 5. Concrete examples

**Bash one-liner with `nc`:**

```sh
printf '{"id":"1","cmd":"df -h /"}\n' | nc 10.10.1.30 9000
```

**Streaming a 10-minute deploy:**

```sh
printf '{"id":"d1","cmd":"bash /opt/scripts/deploy.sh prod","stream":true,"timeout":"20m"}\n' \
  | nc 10.10.1.30 9000
```

**Python client (synchronous, one connection, multiple requests):**

```python
import json, socket, uuid

class Client:
    def __init__(self, host="10.10.1.30", port=9000, timeout=None):
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.f = self.sock.makefile("rwb", buffering=0)

    def send(self, cmd, *, stream=False, timeout=None, id=None):
        req = {"id": id or uuid.uuid4().hex, "cmd": cmd}
        if stream:  req["stream"]  = True
        if timeout: req["timeout"] = timeout
        self.f.write((json.dumps(req) + "\n").encode())
        return req["id"]

    def recv(self):
        line = self.f.readline()
        if not line:
            raise ConnectionError("server closed connection")
        return json.loads(line)

    def run(self, cmd, **kw):
        """Non-streaming: send request, return single response."""
        rid = self.send(cmd, **kw)
        while True:
            resp = self.recv()
            if resp.get("id") == rid:
                return resp

    def run_stream(self, cmd, **kw):
        """Streaming: yield each frame; caller decides when to stop."""
        rid = self.send(cmd, stream=True, **kw)
        while True:
            frame = self.recv()
            if frame.get("id") != rid:
                continue
            yield frame
            if "rc" in frame:
                return  # final frame

# Usage:
c = Client()
print(c.run("ping"))
print(c.run("df -h /"))

for frame in c.run_stream("bash /opt/scripts/deploy.sh prod", timeout="20m"):
    if frame.get("heartbeat"): continue
    if "data" in frame:        print(frame["stream"], frame["data"], end="")
    elif "rc" in frame:        print(f"\n[exit {frame['rc']} in {frame['elapsed_ms']}ms]")
```

## 6. Things clients commonly get wrong

- **Forgetting the trailing `\n`.** The daemon line-scans; without `\n` your
  request is buffered until you eventually send one, or the connection
  times out.
- **Reusing an `id`.** IDs collide on streaming, since multiple frames
  carry the same id. Use a UUID or counter per request.
- **Assuming the next frame is yours.** Multiple in-flight requests on one
  connection interleave. Always dispatch frames by `id`.
- **Treating heartbeat frames as data.** They have neither `data` nor `rc`
  — your loop should skip them, not `break`.
- **Long commands without `stream:true` or `timeout`.** Default per-binary
  timeout is 30s. For 10-minute jobs either configure `limits.<bin>.timeout`
  on the daemon or pass `"timeout":"15m"` in the request, and prefer
  streaming so you see output early and the socket stays warm.
- **Not handling `rc = -1`.** It means timeout *or* cancellation,
  distinguishable by the stderr marker (`tcp-commander: timeout after Xs`
  vs `tcp-commander: cancelled`).
