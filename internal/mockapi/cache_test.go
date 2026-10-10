package mockapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// reqBody builds a minimal but realistic request: tools+system carry a
// cache_control breakpoint, plus a top-level one covering messages.
func reqBody(tools, system, messages string) []byte {
	b := []byte(`{"max_tokens":100,"stream":true,"model":"mock","messages":` + messages +
		`,"system":` + system + `,"tools":` + tools +
		`,"cache_control":{"type":"ephemeral"}}`)
	return b
}

const (
	sysA = `[{"type":"text","text":"You are a mock.","cache_control":{"type":"ephemeral"}}]`
	msgA = `[{"role":"user","content":"hi"}]
`
	toolsA   = `[{"name":"t","input_schema":{"properties":{"a":{"type":"string"}},"required":["a"],"type":"object"}}]`
	toolsAlt = `[{"name":"t","input_schema":{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}}]`
)

// TestCacheHitOnByteStableRepeat: an identical request must read cache.
func TestCacheHitOnByteStableRepeat(t *testing.T) {
	s := New(Config{})
	defer s.Close()

	body := reqBody(toolsA, sysA, msgA)
	r1, c1, i1 := s.cache.observe(body)
	if r1 != 0 || c1 == 0 || i1 != 0 {
		t.Fatalf("first request: read=%d create=%d input=%d, want pure creation", r1, c1, i1)
	}
	r2, c2, i2 := s.cache.observe(body)
	if r2 == 0 {
		t.Fatal("identical second request must read cache")
	}
	if c2 != 0 || i2 != 0 {
		t.Fatalf("second request: read=%d create=%d input=%d, want pure read", r2, c2, i2)
	}
	if r2 > c1 {
		t.Fatalf("read (%d) larger than the entry written (%d)", r2, c1)
	}
}

// TestCacheMissOnSchemaKeyOrderDrift is the simulator's credibility check:
// semantically identical tool schemas whose KEY ORDER differs must NOT hit
// cache — this is exactly the drift the real gateway exhibited (and exactly
// what stableSchema in pkg/tools eliminates).
func TestCacheMissOnSchemaKeyOrderDrift(t *testing.T) {
	s := New(Config{})
	defer s.Close()

	s.cache.observe(reqBody(toolsA, sysA, msgA))
	r, _, _ := s.cache.observe(reqBody(toolsAlt, sysA, msgA))
	if r != 0 {
		t.Fatalf("schema key-order drift reported cache_read=%d, want 0 (drift must break the prefix)", r)
	}
}

// TestCacheGrowsWithHistory: appending messages extends the cached prefix.
// Turn N+1 reads the whole stream turn N wrote (base + old messages), and
// once history grows again that larger prefix is what gets read — so reads
// climb toward the full conversation size instead of staying pinned at
// tools+system.
func TestCacheGrowsWithHistory(t *testing.T) {
	s := New(Config{})
	defer s.Close()

	short := reqBody(toolsA, sysA, msgA)
	s.cache.observe(short)
	rBase, _, _ := s.cache.observe(short)
	if rBase == 0 {
		t.Fatal("repeat must read base prefix")
	}

	longer := `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello there, this reply is longer"},{"role":"user","content":"more"}]`
	rGrow, cGrow, _ := s.cache.observe(reqBody(toolsA, sysA, longer))
	if rGrow < rBase {
		t.Fatalf("extended history read=%d, want >= base %d (at least tools+system)", rGrow, rBase)
	}
	if cGrow == 0 {
		t.Fatal("the appended tail must be written to cache")
	}

	// One more turn: now the previously written long stream is a prefix of
	// this request, so the read must exceed the original base.
	longest := strings.TrimSuffix(longer, "]") + `,{"role":"assistant","content":"even more conversation history padding here"}]`
	rNext, _, _ := s.cache.observe(reqBody(toolsA, sysA, longest))
	if rNext <= rBase {
		t.Fatalf("grown read=%d, want > base %d (history prefix should be read)", rNext, rBase)
	}
}

// TestSSEStreamShape walks one scripted text response frame by frame,
// asserting the exact event order the SDK requires and that usage rides
// message_start and message_delta.
func TestSSEStreamShape(t *testing.T) {
	s := New(Config{
		Script:  []Turn{{Text: []string{"Hel", "lo"}}},
		Latency: 5 * time.Millisecond,
	})
	defer s.Close()

	body := reqBody(toolsA, sysA, msgA)
	start := time.Now()
	resp, err := http.Post(s.URL()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("configured latency not applied: %v", elapsed)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order:\n got %v\nwant %v", events, want)
	}
}

// TestSSEUsageReportsCache: the second streamed response must report
// cache_read_input_tokens in its message_delta usage.
func TestSSEUsageReportsCache(t *testing.T) {
	s := New(Config{})
	defer s.Close()
	body := reqBody(toolsA, sysA, msgA)

	post := func() usage {
		resp, err := http.Post(s.URL()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		var u usage
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, "message_delta") {
				var d struct {
					Usage usage `json:"usage"`
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) == nil {
					u = d.Usage
				}
			}
		}
		return u
	}

	post() // warm
	got := post()
	if got.CacheReadInputTokens == 0 {
		t.Fatalf("warm request usage: %+v, want cache_read>0", got)
	}
	if got.InputTokens != 0 {
		t.Fatalf("fully-cached request should report input=0, got %d", got.InputTokens)
	}
}
