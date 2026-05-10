package server

// Request is one inbound JSON message.
//
// Cmd is the full command line, written exactly as you would type it in a
// shell (e.g. "docker compose -f /opt/myapp/compose.yaml pull"). The daemon
// shell-tokenizes it into argv but never invokes a shell.
//
// Stream, when true, switches the response to multiple line-by-line frames
// followed by a final {rc, elapsed_ms} frame.
//
// Timeout, when non-empty, overrides the per-binary timeout for this single
// request. Parsed as a Go duration ("15m", "2h"). Clamped by
// defaults.max_timeout in the daemon config; values exceeding the cap are
// rejected with an error.
type Request struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	Stream  bool   `json:"stream,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Response is one outbound JSON message.
//
// For non-streaming requests, exactly one Response is emitted with RC,
// Stdout, Stderr, and ElapsedMs set.
//
// For streaming requests, intermediate frames carry Stream="stdout" or
// "stderr" and Data; the final frame carries RC and ElapsedMs. While the
// command is running the daemon may also emit Heartbeat=true frames so
// that idle streams keep the socket warm and clients can detect liveness.
//
// Error responses set Error and leave the rest empty.
type Response struct {
	ID        string `json:"id"`
	RC        *int   `json:"rc,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ElapsedMs *int64 `json:"elapsed_ms,omitempty"`
	Stream    string `json:"stream,omitempty"`
	Data      string `json:"data,omitempty"`
	Heartbeat bool   `json:"heartbeat,omitempty"`
	Error     string `json:"error,omitempty"`
}
