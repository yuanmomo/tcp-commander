package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanmomo/tcp-commander/internal/config"
)

func startTestServer(t *testing.T, body string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:0"

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	addr := srv.ln.Addr().String()
	return addr, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func sendRecv(t *testing.T, conn net.Conn, br *bufio.Reader, req map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, line)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestEndToEnd_Ping(t *testing.T) {
	addr, stop := startTestServer(t, `allow_command_list: []`)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	resp := sendRecv(t, conn, br, map[string]any{"id": "p1", "cmd": "ping"})
	if resp["id"] != "p1" {
		t.Fatalf("id mismatch: %+v", resp)
	}
	if !strings.Contains(resp["stdout"].(string), "tcp-commander") {
		t.Fatalf("stdout: %v", resp["stdout"])
	}
}

func TestEndToEnd_HappyPath(t *testing.T) {
	addr, stop := startTestServer(t, `allow_command_list: [echo]`)
	defer stop()

	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	br := bufio.NewReader(conn)
	resp := sendRecv(t, conn, br, map[string]any{
		"id":  "1",
		"cmd": `echo "greetings world"`,
	})
	out, _ := resp["stdout"].(string)
	if !strings.Contains(out, "greetings world") {
		t.Fatalf("stdout=%q resp=%+v", out, resp)
	}
}

func TestEndToEnd_NotAllowed(t *testing.T) {
	addr, stop := startTestServer(t, `allow_command_list: [echo]`)
	defer stop()

	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	br := bufio.NewReader(conn)
	resp := sendRecv(t, conn, br, map[string]any{"id": "x", "cmd": "rm -rf /tmp"})
	if !strings.Contains(resp["error"].(string), "allow list") {
		t.Fatalf("expected allow-list error, got %+v", resp)
	}
}

func TestEndToEnd_Async(t *testing.T) {
	addr, stop := startTestServer(t, `allow_command_list: [echo, sh]`)
	defer stop()

	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	br := bufio.NewReader(conn)

	for _, req := range []map[string]any{
		{"id": "slow", "cmd": `sh -c "sleep 0.4; echo slow"`},
		{"id": "fast", "cmd": `echo fast`},
	} {
		body, _ := json.Marshal(req)
		conn.Write(append(body, '\n'))
	}
	first, _ := br.ReadBytes('\n')
	second, _ := br.ReadBytes('\n')
	var r1, r2 map[string]any
	_ = json.Unmarshal(first, &r1)
	_ = json.Unmarshal(second, &r2)
	if r1["id"] != "fast" || r2["id"] != "slow" {
		t.Fatalf("expected fast then slow, got %v then %v", r1["id"], r2["id"])
	}
}

func TestEndToEnd_BashScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "h.sh")
	os.WriteFile(script, []byte("#!/bin/bash\necho got=$1\n"), 0o755)

	addr, stop := startTestServer(t, `allow_command_list: [bash]`)
	defer stop()

	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	br := bufio.NewReader(conn)
	resp := sendRecv(t, conn, br, map[string]any{
		"id":  "s1",
		"cmd": "bash " + script + " hello",
	})
	if !strings.Contains(resp["stdout"].(string), "got=hello") {
		t.Fatalf("got %+v", resp)
	}
}

func TestEndToEnd_Streaming(t *testing.T) {
	addr, stop := startTestServer(t, `allow_command_list: [sh]`)
	defer stop()

	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	br := bufio.NewReader(conn)

	body, _ := json.Marshal(map[string]any{
		"id":     "s1",
		"cmd":    `sh -c "echo one; echo two"`,
		"stream": true,
	})
	conn.Write(append(body, '\n'))

	var frames []map[string]any
	for i := 0; i < 3; i++ {
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		frames = append(frames, m)
	}
	if frames[0]["stream"] != "stdout" || !strings.HasPrefix(frames[0]["data"].(string), "one") {
		t.Fatalf("frame 0: %+v", frames[0])
	}
	if frames[2]["rc"] == nil {
		t.Fatalf("expected final rc frame, got %+v", frames[2])
	}
}
