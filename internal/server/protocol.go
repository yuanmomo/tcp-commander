package server

// Request is one inbound JSON message.
//
// Cmd is the full command line, written exactly as you would type it in a
// shell (e.g. "docker compose -f /opt/myapp/compose.yaml pull"). The daemon
// shell-tokenizes it into argv but never invokes a shell.
//
// Stream, when true, switches the response to multiple line-by-line frames
// followed by a final {rc, elapsed_ms} frame.
type Request struct {
	ID     string `json:"id"`
	Cmd    string `json:"cmd"`
	Stream bool   `json:"stream,omitempty"`
}

// Response is one outbound JSON message.
//
// For non-streaming requests, exactly one Response is emitted with RC,
// Stdout, Stderr, and ElapsedMs set.
//
// For streaming requests, intermediate frames carry Stream="stdout" or
// "stderr" and Data; the final frame carries RC and ElapsedMs.
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
	Error     string `json:"error,omitempty"`
}
