package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStreamBufFlushesFirstWrite: the zero last-time makes time.Since
// exceed the coalesce window, so the very first chunk ships synchronously —
// an interactive command's first line must not wait out the window.
func TestStreamBufFlushesFirstWrite(t *testing.T) {
	var got []string
	w := &streamBuf{sink: func(s string) { got = append(got, s) }}
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "first\n" {
		t.Fatalf("first write must flush synchronously, got %q", got)
	}
	if w.buf.String() != "first\n" {
		t.Fatalf("result buffer lost bytes: %q", w.buf.String())
	}
}

// TestStreamBufCoalescesWithinWindow: writes inside the window hold and
// ship on the timer — the coalescing that keeps a build from flooding the
// event channel. The hold itself is only observable as "it arrives soon",
// never as "it never arrives".
func TestStreamBufCoalescesWithinWindow(t *testing.T) {
	var mu sync.Mutex
	var sb strings.Builder
	w := &streamBuf{sink: func(s string) { mu.Lock(); sb.WriteString(s); mu.Unlock() }}
	w.Write([]byte("first\n"))
	w.Write([]byte("second\n"))

	deadline := time.Now().Add(2 * time.Second)
	var all string
	for {
		mu.Lock()
		all = sb.String()
		mu.Unlock()
		if strings.Contains(all, "second\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("held chunk never shipped: %q", all)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(all, "first\n") {
		t.Fatalf("first chunk lost: %q", all)
	}
}

// TestStreamBufPendCapForcesFlush: a newline-free stream (progress bar)
// past the pend cap flushes regardless of the time window — pend is at
// least streamPendCap, so the flush is structural, not timing-dependent.
func TestStreamBufPendCapForcesFlush(t *testing.T) {
	var got []string
	w := &streamBuf{sink: func(s string) { got = append(got, s) }}
	w.Write([]byte("warm\n")) // first flush; sets w.last = now
	w.last = time.Now()       // firmly inside the coalesce window
	if _, err := w.Write(make([]byte, streamPendCap)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pend-cap write must flush synchronously, got %d chunks", len(got))
	}
	if len(got[1]) != streamPendCap {
		t.Fatalf("flushed %d bytes, want %d", len(got[1]), streamPendCap)
	}
}

// TestStreamBufCloseFlushesTail: Close ships the held tail before any
// OnToolEnd can fire downstream, and later writes stay out of the sink
// while still landing in the result buffer.
func TestStreamBufCloseFlushesTail(t *testing.T) {
	var mu sync.Mutex
	var sb strings.Builder
	w := &streamBuf{sink: func(s string) { mu.Lock(); sb.WriteString(s); mu.Unlock() }}
	w.Write([]byte("first\n"))
	w.Write([]byte("tail\n")) // inside the window: held (or, on a stalled
	// machine, already flushed — both paths must end at the same place)
	w.Close()

	mu.Lock()
	all := sb.String()
	mu.Unlock()
	if !strings.Contains(all, "tail\n") {
		t.Fatalf("Close did not ship the held tail: %q", all)
	}
	if _, err := w.Write([]byte("late\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.buf.String(), "late\n") {
		t.Fatalf("result buffer must still capture post-Close writes: %q", w.buf.String())
	}
	mu.Lock()
	all = sb.String()
	mu.Unlock()
	if strings.Contains(all, "late\n") {
		t.Fatalf("post-Close write leaked to the sink: %q", all)
	}
}

// TestShellRunStreamsToSink: a run under a session sink delivers every
// output byte to it, and the bytes equal the captured result — the tee
// neither drops nor duplicates.
func TestShellRunStreamsToSink(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	var mu sync.Mutex
	var sb strings.Builder
	ctx := WithOutputSink(context.Background(), func(s string) {
		mu.Lock()
		sb.WriteString(s)
		mu.Unlock()
	})
	out, code, _, err := sh.run(ctx, "echo lyon-tee-ok && echo lyon-tee-err >&2")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(string(out), "lyon-tee-ok") || !strings.Contains(string(out), "lyon-tee-err") {
		t.Fatalf("result lost output: %q", out)
	}
	mu.Lock()
	seen := sb.String()
	mu.Unlock()
	if !strings.Contains(seen, "lyon-tee-ok") || !strings.Contains(seen, "lyon-tee-err") {
		t.Fatalf("sink lost output: %q", seen)
	}
	if seen != string(out) {
		t.Fatalf("tee diverged from result:\n sink %q\n result %q", seen, out)
	}
}

// TestShellRunWithoutSink: probes and metrics run with no session listening
// — nil sink, plain capture, no panic.
func TestShellRunWithoutSink(t *testing.T) {
	if OutputSink(context.Background()) != nil {
		t.Fatal("bare context must carry no sink")
	}
	sh := &Shell{Root: tempDir(t)}
	out, code, _, err := sh.run(context.Background(), "echo plain-ok")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(string(out), "plain-ok") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// TestAdaptWiresOutputSink: an observer opted in via OnToolOutput gets the
// sink installed with the running exec's ExecID on every chunk.
func TestAdaptWiresOutputSink(t *testing.T) {
	var startID uint64
	var chunks []string
	obs := ObserverFuncs{
		Start: func(e ExecEvent) { startID = e.ExecID },
		Output: func(e ExecEvent) {
			if e.ExecID != startID {
				t.Errorf("chunk ExecID %d != start ExecID %d", e.ExecID, startID)
			}
			chunks = append(chunks, e.Output)
		},
	}
	tool := &funcTool{name: "live", parallel: true, fn: func(ctx context.Context) error {
		sink := OutputSink(ctx)
		if sink == nil {
			t.Error("OutputObserver must install a sink")
			return nil
		}
		sink("chunk-1")
		sink("chunk-2")
		return nil
	}}
	a := Adapt(tool, AdapterConfig{Observer: obs})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0] != "chunk-1" || chunks[1] != "chunk-2" {
		t.Fatalf("chunks = %q", chunks)
	}
}

// bareObs implements Observer with only Start/End — deliberately NOT an
// OutputObserver.
type bareObs struct{ starts, ends int }

func (o *bareObs) OnToolStart(ExecEvent) { o.starts++ }
func (o *bareObs) OnToolEnd(ExecEvent)   { o.ends++ }

// TestAdaptPlainObserverNoSink: an observer without OnToolOutput gets no
// sink — plain captures pay nothing for the feature.
func TestAdaptPlainObserverNoSink(t *testing.T) {
	o := &bareObs{}
	tool := &funcTool{name: "quiet", parallel: true, fn: func(ctx context.Context) error {
		if OutputSink(ctx) != nil {
			t.Error("sink installed for a non-OutputObserver")
		}
		return nil
	}}
	a := Adapt(tool, AdapterConfig{Observer: o})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if o.starts != 1 || o.ends != 1 {
		t.Fatalf("starts=%d ends=%d", o.starts, o.ends)
	}
}
