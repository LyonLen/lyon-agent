package server

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
)

// TestWebJointSmoke is the engineering rule's joint self-test: the real
// stack once — scripted mock model → built lyond binary → vite dev →
// Playwright on system Edge (web/e2e/smoke.mjs). It runs only when asked,
// so plain `go test ./...` stays fast:
//
//	LYON_E2E=1 go test ./pkg/server/ -run TestWebJointSmoke -v
func TestWebJointSmoke(t *testing.T) {
	if os.Getenv("LYON_E2E") == "" {
		t.Skip("joint browser smoke: set LYON_E2E=1")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	// Scripted model: three deltas per reply — per-delta doubling (the
	// StrictMode mutation bug) breaks the exact phrase the smoke asserts.
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{{Text: []string{"联调回复", "一二三四五", "六七八九十。"}}},
	})
	defer mock.Close()

	// Workspace with one subdirectory (breadcrumb scenario) + isolated home.
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	// Compile before the joint pass (the rule).
	bin := filepath.Join(t.TempDir(), "lyond-bin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/lyond")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build lyond: %v: %s", err, out)
	}

	port := freePort(t)
	lyond := exec.Command(bin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-workspace", ws,
		"-token", "e2e",
		"-session-ttl", "0",
	)
	lyond.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+mock.URL(),
		"ANTHROPIC_AUTH_TOKEN=e2e-mock",
		"LYON_HOME="+home,
	)
	var lyondLog bytes.Buffer
	lyond.Stdout, lyond.Stderr = &lyondLog, &lyondLog
	if err := lyond.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		t.Log("lyond log:\n" + lyondLog.String())
		killTree(t, lyond)
	}()
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))

	// Vite dev in front (its proxy targets the isolated lyond — the
	// developer's own instance on 8080 is never touched).
	vport := freePort(t)
	vite := exec.Command("npx", "vite", "--port", strconv.Itoa(vport), "--strictPort", "--host", "127.0.0.1")
	vite.Dir = filepath.Join(root, "web")
	vite.Env = append(os.Environ(),
		"LYOND_PROXY_TARGET=http://127.0.0.1:"+strconv.Itoa(port),
		"BROWSER=none",
	)
	var viteLog bytes.Buffer
	vite.Stdout, vite.Stderr = &viteLog, &viteLog
	if err := vite.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		t.Log("vite log:\n" + viteLog.String())
		killTree(t, vite)
	}()
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/", vport))

	// The browser speaks.
	node := exec.Command("node", "e2e/smoke.mjs")
	node.Dir = filepath.Join(root, "web")
	node.Env = append(os.Environ(),
		"BASE_URL=http://127.0.0.1:"+strconv.Itoa(vport),
		"LYON_TOKEN=e2e",
	)
	out, err := node.CombinedOutput()
	t.Log("\n" + string(out))
	if err != nil {
		t.Fatalf("browser smoke failed: %v", err)
	}
}

// killTree tears a child down WITH its descendants: npx spawns node and
// esbuild children that inherit our stdio, and a leaked grandchild keeps
// `go test`'s output pipe open for a minute ("Test I/O incomplete").
func killTree(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID",
			strconv.Itoa(cmd.Process.Pid)).Run()
	} else {
		_ = cmd.Process.Kill()
	}
	_, _ = cmd.Process.Wait()
}

// freePort grabs an ephemeral TCP port and releases it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// waitHTTP polls a URL until it answers 2xx/3xx (or the budget runs out).
func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 400 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", url)
}
