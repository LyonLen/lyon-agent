//go:build windows

// Command tuipty drives a full-screen TUI child inside a ConPTY and
// executes a small steps file against it. The demo-60s script needs the
// real `lyon chat` TUI — the diagnosis memory writeback lives in its
// bubbletea Update loop — while the driver's own stdout is a pipe, where
// `lyon chat` would silently fall back to the plain REPL. A pseudo
// console gives the child a genuine console (isatty true, VT input,
// no window), so the TUI runs exactly as in a real terminal.
//
// Steps file (one per line, '#' comments):
//
//	sleep <ms>                        wait
//	send <text...>                    type text into the TUI, then Enter
//	wait-audit <substr> <timeout_ms>  poll $LYON_HOME/audit/*.jsonl for a
//	                                  line containing substr that appeared
//	                                  after this driver started (or after
//	                                  the previous wait-audit matched)
//	wait-exit <timeout_ms>            wait for the child to exit (code 0)
//
// Exit code: 0 only when every step passed and the child exited 0.
// All child output lands in -transcript for assertions and debugging.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	procThreadAttributePseudoconsole = 0x00020016
	extendedStartupinfoPresent       = 0x00080000
	startfUseStdHandles              = 0x00000100
	waitObject0                      = 0x00000000
	waitTimeout                      = 0x00000102
)

var (
	modkernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreatePseudoConsole          = modkernel32.NewProc("CreatePseudoConsole")
	procClosePseudoConsole           = modkernel32.NewProc("ClosePseudoConsole")
	procInitAttrList                 = modkernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateAttr                   = modkernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteAttrList               = modkernel32.NewProc("DeleteProcThreadAttributeList")
	procCreatePipe                   = modkernel32.NewProc("CreatePipe")
	procCreateProcessW               = modkernel32.NewProc("CreateProcessW")
	procGetExitCodeProcess           = modkernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess             = modkernel32.NewProc("TerminateProcess")
	procSetHandleInformation         = modkernel32.NewProc("SetHandleInformation")
	handleFlagInherit        uintptr = 0x00000001
)

type startupInfo struct {
	Cb            uint32
	LpReserved    *uint16
	LpDesktop     *uint16
	LpTitle       *uint16
	DwX           uint32
	DwY           uint32
	DwXSize       uint32
	DwYSize       uint32
	DwXCountChars uint32
	DwYCountChars uint32
	DwFillAttr    uint32
	DwFlags       uint32
	WShowWindow   uint16
	CbReserved2   uint16
	LpReserved2   *byte
	HStdInput     uintptr
	HStdOutput    uintptr
	HStdError     uintptr
}

type startupInfoEx struct {
	StartupInfo   startupInfo
	AttributeList *byte
}

type processInformation struct {
	Process   uintptr
	Thread    uintptr
	ProcessId uint32
	ThreadId  uint32
}

func main() {
	cmd := flag.String("cmd", "", "full command line of the TUI child")
	cwd := flag.String("cwd", "", "child working directory (default: inherit)")
	stepsPath := flag.String("steps", "", "steps file to execute")
	transcript := flag.String("transcript", "", "file receiving all child output")
	cols := flag.Int("cols", 140, "pseudo console width")
	rows := flag.Int("rows", 43, "pseudo console height")
	timeout := flag.Duration("timeout", 60*time.Second, "global deadline for the whole run")
	flag.Parse()

	if *cmd == "" || *stepsPath == "" {
		fmt.Fprintln(os.Stderr, "tuipty: -cmd and -steps are required")
		os.Exit(2)
	}
	steps, err := parseSteps(*stepsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tuipty:", err)
		os.Exit(2)
	}

	child, err := spawn(*cmd, *cwd, int16(*cols), int16(*rows))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tuipty: spawn:", err)
		os.Exit(2)
	}
	// Transcript first so no output byte is ever lost, then the watchdog.
	child.transcript, err = os.Create(*transcript)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tuipty: transcript:", err)
		child.kill()
		os.Exit(2)
	}
	child.startReader()
	fmt.Printf("[tuipty] spawned pid=%d cols=%d rows=%d\n", child.pi.ProcessId, *cols, *rows)

	time.AfterFunc(*timeout, func() {
		fmt.Fprintf(os.Stderr, "[tuipty] FAIL global timeout (%s) — killing child, see %s\n", *timeout, *transcript)
		child.kill()
		child.wait(2 * time.Second)
		child.closeFiles()
		os.Exit(1)
	})

	cursor := newAuditCursor()
	for i, st := range steps {
		if err := runStep(child, cursor, st); err != nil {
			fmt.Fprintf(os.Stderr, "[tuipty] FAIL step %d (%s): %v\n", i+1, st.raw, err)
			child.kill()
			child.wait(3 * time.Second)
			child.closeFiles()
			os.Exit(1)
		}
		fmt.Printf("[tuipty] ok   step %d: %s\n", i+1, st.raw)
	}

	if child.alive() {
		fmt.Fprintln(os.Stderr, "[tuipty] FAIL steps finished but child is still running")
		child.kill()
		child.wait(3 * time.Second)
		child.closeFiles()
		os.Exit(1)
	}
	code, err := child.exitCode()
	if err != nil || code != 0 {
		fmt.Fprintf(os.Stderr, "[tuipty] FAIL child exit code=%d err=%v\n", code, err)
		child.closeFiles()
		os.Exit(1)
	}
	child.closeFiles()
	fmt.Printf("[tuipty] done, child exited 0, transcript=%s\n", *transcript)
}

type step struct {
	kind string // sleep | send | wait-audit | wait-exit
	a    string
	b    int
	raw  string
}

func parseSteps(path string) ([]step, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []step
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		fields := strings.Fields(trim)
		switch fields[0] {
		case "sleep", "wait-exit":
			if len(fields) != 2 {
				return nil, fmt.Errorf("line %d: %s needs one <ms> argument", n+1, fields[0])
			}
			ms, err := strconv.Atoi(fields[1])
			if err != nil || ms < 0 {
				return nil, fmt.Errorf("line %d: bad ms %q", n+1, fields[1])
			}
			out = append(out, step{kind: fields[0], b: ms, raw: trim})
		case "send":
			if !strings.HasPrefix(line, "send ") || strings.TrimSpace(strings.TrimPrefix(line, "send ")) == "" {
				return nil, fmt.Errorf("line %d: send needs text", n+1)
			}
			out = append(out, step{kind: "send", a: strings.TrimPrefix(line, "send "), raw: "send " + strings.TrimPrefix(line, "send ")})
		case "wait-audit":
			if len(fields) != 3 {
				return nil, fmt.Errorf("line %d: wait-audit needs <substr> <ms>", n+1)
			}
			ms, err := strconv.Atoi(fields[2])
			if err != nil || ms < 0 {
				return nil, fmt.Errorf("line %d: bad ms %q", n+1, fields[2])
			}
			out = append(out, step{kind: "wait-audit", a: fields[1], b: ms, raw: trim})
		default:
			return nil, fmt.Errorf("line %d: unknown step %q", n+1, fields[0])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no steps in %s", path)
	}
	return out, nil
}

func runStep(c *child, cur *auditCursor, st step) error {
	switch st.kind {
	case "sleep":
		time.Sleep(time.Duration(st.b) * time.Millisecond)
		return nil
	case "send":
		if !c.alive() {
			code, _ := c.exitCode()
			return fmt.Errorf("child not running (exit code %d)", code)
		}
		return c.write([]byte(st.a + "\r"))
	case "wait-audit":
		return cur.wait(st.a, time.Duration(st.b)*time.Millisecond)
	case "wait-exit":
		return c.waitExit(time.Duration(st.b) * time.Millisecond)
	}
	return fmt.Errorf("unknown step %q", st.kind)
}

// auditCursor tracks how much of each audit file previous waits have
// consumed, so a wait only sees lines that appeared after the driver
// started (or after the previous match). A fresh LYON_HOME across demo
// sessions shares one audit file — without the cursor, session 2 would
// match session 1's diagnosis line instantly.
type auditCursor struct {
	offsets map[string]int64
}

func newAuditCursor() *auditCursor {
	cur := &auditCursor{offsets: map[string]int64{}}
	// Existing content predates the child — it belongs to no wait.
	for _, f := range cur.glob() {
		if fi, err := os.Stat(f); err == nil {
			cur.offsets[f] = fi.Size()
		}
	}
	return cur
}

func (cur *auditCursor) glob() []string {
	home := os.Getenv("LYON_HOME")
	if home == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(home, "audit", "*.jsonl"))
	return files
}

// wait polls until one *complete* line containing substr shows up beyond
// the cursor, then parks the cursor right after that line.
func (cur *auditCursor) wait(substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastSeen string
	for {
		for _, f := range cur.glob() {
			off := cur.offsets[f] // missing key = 0 = brand-new file
			got, err := readFrom(f, off)
			if err != nil {
				continue // file vanished or mid-rotate — next poll
			}
			if after, ok := firstMatch(got, substr); ok {
				cur.offsets[f] = off + after
				return nil
			}
			if len(got) > 0 {
				lastSeen = f + ": " + tail(got, 240)
			}
		}
		if time.Now().After(deadline) {
			if lastSeen == "" {
				lastSeen = "no audit lines seen"
			}
			return fmt.Errorf("timeout %s waiting for %q (last: %s)", substr, timeout, lastSeen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readFrom(path string, off int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// firstMatch scans only complete (\n-terminated) lines: a half-written
// line must not match and must not be skipped past. Returns the offset
// just past the first matching line.
func firstMatch(chunk []byte, substr string) (int64, bool) {
	lastNL := bytes.LastIndexByte(chunk, '\n')
	if lastNL < 0 {
		return 0, false
	}
	complete := chunk[:lastNL+1]
	lineStart := 0
	for lineStart < len(complete) {
		nl := bytes.IndexByte(complete[lineStart:], '\n')
		line := complete[lineStart : lineStart+nl]
		if bytes.Contains(line, []byte(substr)) {
			return int64(lineStart + nl + 1), true
		}
		lineStart += nl + 1
	}
	return 0, false
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return strings.ReplaceAll(string(b), "\n", " ")
	}
	return strings.ReplaceAll(string(b[len(b)-n:]), "\n", " ")
}

// child is one spawned TUI process attached to a pseudo console.
type child struct {
	pi         processInformation
	hpc        uintptr
	inFile     *os.File
	outFile    *os.File
	transcript *os.File
	outDone    chan struct{}
}

func spawn(cmdline, cwd string, cols, rows int16) (*child, error) {
	// Two pipe pairs: [0] is conpty-side, [1] is ours.
	var inRead, inWrite, outRead, outWrite uintptr
	if r, _, e := procCreatePipe.Call(uintptr(unsafe.Pointer(&inRead)), uintptr(unsafe.Pointer(&inWrite)), 0, 0); r == 0 {
		return nil, fmt.Errorf("CreatePipe in: %v", e)
	}
	if r, _, e := procCreatePipe.Call(uintptr(unsafe.Pointer(&outRead)), uintptr(unsafe.Pointer(&outWrite)), 0, 0); r == 0 {
		return nil, fmt.Errorf("CreatePipe out: %v", e)
	}
	// Our write end (input) and the conpty's read end (output) must not
	// leak into the child through handle inheritance — the pseudo console
	// attribute, not inheritance, wires the child's std handles.
	if r, _, e := procSetHandleInformation.Call(inWrite, handleFlagInherit, 0); r == 0 {
		return nil, fmt.Errorf("SetHandleInformation inWrite: %v", e)
	}
	if r, _, e := procSetHandleInformation.Call(outRead, handleFlagInherit, 0); r == 0 {
		return nil, fmt.Errorf("SetHandleInformation outRead: %v", e)
	}

	var hpc uintptr
	// COORD packs X (cols) into the low word, Y (rows) into the high word.
	coordPacked := uintptr(uint32(uint16(cols)) | (uint32(uint16(rows)) << 16))
	hr, _, _ := procCreatePseudoConsole.Call(
		coordPacked,
		inRead,
		outWrite,
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	// The pseudo console holds its own refs to these ends now.
	syscall.CloseHandle(syscall.Handle(inRead))   //nolint:errcheck
	syscall.CloseHandle(syscall.Handle(outWrite)) //nolint:errcheck
	if int32(hr) < 0 {
		return nil, fmt.Errorf("CreatePseudoConsole hr=0x%08x", uint32(hr))
	}

	// Attribute list carrying the pseudo console into CreateProcess.
	var listSize uintptr
	procInitAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&listSize)))
	if listSize == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList size query failed")
	}
	list := make([]byte, listSize)
	if r, _, e := procInitAttrList.Call(uintptr(unsafe.Pointer(&list[0])), 1, 0, uintptr(unsafe.Pointer(&listSize))); r == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %v", e)
	}
	defer procDeleteAttrList.Call(uintptr(unsafe.Pointer(&list[0])))
	if r, _, e := procUpdateAttr.Call(
		uintptr(unsafe.Pointer(&list[0])),
		0,
		procThreadAttributePseudoconsole,
		hpc,
		unsafe.Sizeof(hpc),
		0, 0,
	); r == 0 {
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %v", e)
	}

	cmdUTF16, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var cwdUTF16 *uint16
	if cwd != "" {
		if cwdUTF16, err = syscall.UTF16PtrFromString(cwd); err != nil {
			return nil, err
		}
	}
	var si startupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	// STARTF_USESTDHANDLES with all-zero hStd fields: without this flag
	// CreateProcess copies the *parent's* std handles (here: the harness
	// pipes) into the child no matter what bInheritHandles says, and the
	// child's writes bypass the pseudo console entirely. With the flag
	// and NULL handles the child starts with no std handles, and the
	// console host wires its clients' std handles to the pty — the same
	// recipe go-pty ships (cmd_windows.go: siEx.Flags = STARTF_USESTDHANDLES).
	si.StartupInfo.DwFlags = startfUseStdHandles
	si.AttributeList = &list[0]
	var pi processInformation
	r, _, e := procCreateProcessW.Call(
		0,
		uintptr(unsafe.Pointer(cmdUTF16)),
		0,
		0,
		0, // handles ride the pseudo console, not inheritance
		extendedStartupinfoPresent,
		0,
		uintptr(unsafe.Pointer(cwdUTF16)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CreateProcessW %q: %v", cmdline, e)
	}
	syscall.CloseHandle(syscall.Handle(pi.Thread)) //nolint:errcheck

	return &child{
		pi:      pi,
		hpc:     hpc,
		inFile:  os.NewFile(inWrite, "conpty-in"),
		outFile: os.NewFile(outRead, "conpty-out"),
	}, nil
}

func (c *child) startReader() {
	c.outDone = make(chan struct{})
	go func() {
		defer close(c.outDone)
		if c.transcript == nil {
			_, _ = io.Copy(io.Discard, c.outFile)
			return
		}
		_, _ = io.Copy(c.transcript, c.outFile)
	}()
}

func (c *child) write(b []byte) error {
	for len(b) > 0 {
		n, err := c.inFile.Write(b)
		if err != nil {
			return fmt.Errorf("pty write: %w", err)
		}
		b = b[n:]
	}
	return nil
}

func (c *child) alive() bool {
	r, _ := syscall.WaitForSingleObject(syscall.Handle(c.pi.Process), 0)
	return r == waitTimeout
}

func (c *child) wait(d time.Duration) {
	ms := uint32(d / time.Millisecond)
	syscall.WaitForSingleObject(syscall.Handle(c.pi.Process), ms)
}

func (c *child) waitExit(d time.Duration) error {
	ms := uint32(d / time.Millisecond)
	r, _ := syscall.WaitForSingleObject(syscall.Handle(c.pi.Process), ms)
	if r == waitTimeout {
		return fmt.Errorf("child still running after %s", d)
	}
	if r != waitObject0 {
		return fmt.Errorf("WaitForSingleObject: %v", syscall.Errno(r))
	}
	code, err := c.exitCode()
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("child exited with code %d", code)
	}
	return nil
}

func (c *child) exitCode() (int, error) {
	var code uint32
	r, _, e := procGetExitCodeProcess.Call(c.pi.Process, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return -1, fmt.Errorf("GetExitCodeProcess: %v", e)
	}
	return int(code), nil
}

func (c *child) kill() {
	if c.alive() {
		procTerminateProcess.Call(c.pi.Process, 1)
	}
}

// closeFiles shuts the pseudo console (which ends the output stream),
// drains the reader and releases every handle we own.
func (c *child) closeFiles() {
	if c.inFile != nil {
		c.inFile.Close()
	}
	procClosePseudoConsole.Call(c.hpc)
	if c.outDone != nil {
		select {
		case <-c.outDone:
		case <-time.After(2 * time.Second):
		}
	}
	if c.outFile != nil {
		c.outFile.Close()
	}
	if c.transcript != nil {
		c.transcript.Close()
	}
	if c.pi.Process != 0 {
		syscall.CloseHandle(syscall.Handle(c.pi.Process))
		c.pi.Process = 0
	}
}
