// Package server implements the TCP daemon: connection handling, the
// newline-JSON request/response protocol, and dispatch into the executor.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/yuanmomo/tcp-commander/internal/command"
	"github.com/yuanmomo/tcp-commander/internal/config"
)

// Version reported by the ping command. Overridden via ldflags at build time.
var Version = "dev"

// Server owns the TCP listener and per-connection lifecycle.
type Server struct {
	cfg   *config.Config
	exec  *command.Executor
	log   *slog.Logger
	start time.Time

	ln       net.Listener
	conns    sync.WaitGroup
	shutdown chan struct{}
}

// New constructs a Server. It does not bind a port; call Start.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	return &Server{
		cfg:      cfg,
		exec:     command.New(cfg),
		log:      log,
		start:    time.Now(),
		shutdown: make(chan struct{}),
	}, nil
}

// Start binds the listener and begins accepting connections.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	s.ln = ln
	s.log.Info("listening", "addr", ln.Addr().String())
	go s.acceptLoop()
	return nil
}

// Shutdown stops accepting new connections and waits for in-flight ones
// to finish (or for ctx to expire, whichever comes first).
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.shutdown)
	if s.ln != nil {
		_ = s.ln.Close()
	}
	done := make(chan struct{})
	go func() {
		s.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("accept error", "err", err.Error())
			continue
		}
		if !s.ipAllowed(conn) {
			s.log.Warn("ip not allowed", "remote", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}
		// TCP keepalive: long-running commands leave the socket idle for
		// minutes, and stateful middleboxes (NAT / corp firewalls / cloud
		// LBs) reap idle TCP. Keepalive both keeps the conntrack entry
		// warm and detects dead peers so we can free the slot.
		if tc, ok := conn.(*net.TCPConn); ok {
			if ka := s.cfg.KeepAlive(); ka > 0 {
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(ka)
			}
		}
		s.conns.Add(1)
		go s.serve(conn)
	}
}

func (s *Server) ipAllowed(conn net.Conn) bool {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return s.cfg.IPAllowed(ip)
}

// serve reads requests off a single connection and dispatches them
// concurrently. A single writer goroutine serializes responses back.
func (s *Server) serve(conn net.Conn) {
	defer s.conns.Done()
	defer conn.Close()

	defer func() {
		if r := recover(); r != nil {
			s.log.Error("connection panic",
				"remote", conn.RemoteAddr().String(),
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()))
		}
	}()

	remote := conn.RemoteAddr().String()
	s.log.Info("connection open", "remote", remote)

	writes := make(chan Response, 16)
	writerDone := make(chan struct{})
	go s.writer(conn, writes, writerDone)

	var inFlight sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel propagates to in-flight commands. We cancel as soon as either
	// the read loop exits (client EOF / read error) or the server starts
	// shutting down — whichever fires first. This frees concurrency slots
	// instead of letting an abandoned 10-minute command run to completion.
	defer cancel()
	go func() {
		<-s.shutdown
		cancel()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

readLoop:
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writes <- Response{Error: "invalid json: " + err.Error()}
			break readLoop
		}
		if req.ID == "" {
			writes <- Response{Error: "missing id"}
			break readLoop
		}

		// ping bypass
		if req.Cmd == "ping" {
			rc := 0
			elapsed := int64(0)
			writes <- Response{
				ID:        req.ID,
				RC:        &rc,
				Stdout:    fmt.Sprintf("tcp-commander %s uptime=%s", Version, time.Since(s.start).Round(time.Second)),
				ElapsedMs: &elapsed,
			}
			continue
		}

		inFlight.Add(1)
		go func(req Request) {
			defer inFlight.Done()
			s.handle(ctx, remote, req, writes)
		}(req)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.log.Debug("read error", "remote", remote, "err", err.Error())
	}

	// Read loop ended (client disconnect / EOF / error). Cancel before
	// waiting so any in-flight command is signalled to stop instead of
	// running to completion against a dead client.
	cancel()

	inFlight.Wait()
	close(writes)
	<-writerDone
	s.log.Info("connection closed", "remote", remote)
}

// writer serializes Response writes onto a single connection.
func (s *Server) writer(conn net.Conn, ch <-chan Response, done chan<- struct{}) {
	defer close(done)
	bw := bufio.NewWriter(conn)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for resp := range ch {
		if err := enc.Encode(&resp); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}

// handle runs one request and emits its response(s).
func (s *Server) handle(
	ctx context.Context,
	remote string,
	req Request,
	writes chan<- Response,
) {
	opts, err := s.optionsFor(req)
	if err != nil {
		writes <- Response{ID: req.ID, Error: err.Error()}
		s.logRequest(remote, req, nil, err)
		return
	}

	s.log.Info("request start",
		"remote", remote,
		"id", req.ID,
		"cmd", req.Cmd,
		"stream", req.Stream,
		"timeout_override", req.Timeout,
	)

	var streamFn func(command.StreamEvent)
	var hbStop chan struct{}
	if req.Stream {
		streamFn = func(ev command.StreamEvent) {
			writes <- Response{ID: req.ID, Stream: ev.Channel, Data: ev.Data}
		}
		if hb := s.cfg.Heartbeat(); hb > 0 {
			hbStop = make(chan struct{})
			go s.heartbeat(req.ID, hb, hbStop, writes)
		}
	}

	res, err := s.exec.Run(ctx, req.Cmd, streamFn, opts)
	if hbStop != nil {
		close(hbStop)
	}
	if err != nil {
		writes <- Response{ID: req.ID, Error: err.Error()}
		s.logRequest(remote, req, nil, err)
		return
	}
	rc := res.RC
	elapsed := res.ElapsedMs
	resp := Response{
		ID:        req.ID,
		RC:        &rc,
		ElapsedMs: &elapsed,
	}
	if !req.Stream {
		resp.Stdout = res.Stdout
		resp.Stderr = res.Stderr
	}
	writes <- resp
	s.logRequest(remote, req, res, nil)
}

// optionsFor parses optional per-request knobs and validates them against
// the daemon's caps.
func (s *Server) optionsFor(req Request) (command.Options, error) {
	var opts command.Options
	if req.Timeout == "" {
		return opts, nil
	}
	d, err := time.ParseDuration(req.Timeout)
	if err != nil {
		return opts, fmt.Errorf("invalid timeout %q: %w", req.Timeout, err)
	}
	if d <= 0 {
		return opts, fmt.Errorf("timeout must be > 0")
	}
	max := s.cfg.MaxTimeout()
	if max <= 0 {
		return opts, fmt.Errorf("per-request timeout overrides are disabled")
	}
	if d > max {
		return opts, fmt.Errorf("timeout %s exceeds max_timeout %s", d, max)
	}
	opts.Timeout = d
	return opts, nil
}

// heartbeat emits a liveness frame every interval until stop is closed.
// Used for streaming requests so an idle stream still keeps the socket warm
// and gives clients a positive signal that the daemon is alive.
func (s *Server) heartbeat(id string, interval time.Duration, stop <-chan struct{}, writes chan<- Response) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			select {
			case writes <- Response{ID: id, Heartbeat: true}:
			case <-stop:
				return
			}
		}
	}
}

func (s *Server) logRequest(
	remote string,
	req Request,
	res *command.Result,
	err error,
) {
	attrs := []any{
		"remote", remote,
		"id", req.ID,
		"cmd", req.Cmd,
	}
	if res != nil {
		attrs = append(attrs, "prog", res.Prog, "rc", res.RC, "elapsed_ms", res.ElapsedMs)
		if res.TimedOut {
			attrs = append(attrs, "timed_out", true)
		}
		if res.Truncated {
			attrs = append(attrs, "truncated", true)
		}
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
		s.log.Warn("request", attrs...)
		return
	}
	s.log.Info("request", attrs...)
}
