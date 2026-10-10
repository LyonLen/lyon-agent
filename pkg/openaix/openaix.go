// Package openaix adapts OpenAI-compatible /chat/completions gateways to
// callers that speak the Anthropic Messages protocol.
//
// The whole adaptation lives in one http.RoundTripper: every request the
// anthropic-sdk-go client sends (main loop, tool runner, compaction,
// subagent, title/distill one-shots) passes through here, gets rewritten to
// POST {Base}/chat/completions in OpenAI protocol, and the response — plain
// JSON, SSE stream, or error body — is translated back to the Anthropic
// shapes the SDK's stream decoder and BetaMessage accumulator expect
// (message_start → content_block_* → message_delta → message_stop, with
// strictly sequential block indexes and overwrite-style usage). Nothing
// above this layer knows a second protocol exists.
//
// Enable it via agent.Config.OpenAIBase (-openai / -openai-base /
// LYON_OPENAI_BASE). Requests that are not POST /messages (client web
// tools, for instance) pass through to the inner transport untouched, and
// the API key rides along unchanged: x-api-key becomes
// Authorization: Bearer — gateways that issue one key for both protocols
// keep working without reconfiguration.
package openaix

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// DefaultBase is the gateway the -openai flag targets when no explicit
// base URL is given.
const DefaultBase = "https://token-plan-cn.xiaomimimo.com/v1"

// Transport translates Anthropic-protocol POST /v1/messages requests into
// OpenAI chat/completions requests against {Base}/chat/completions and
// translates every response shape back. All other traffic goes to Inner.
type Transport struct {
	// Base is the OpenAI-compatible base URL, e.g. https://host/v1.
	// Trailing slashes are ignored.
	Base string
	// Inner performs the actual network round trips (tuned connection
	// pool). nil uses http.DefaultTransport.
	Inner http.RoundTripper
}

func (t *Transport) inner() http.RoundTripper {
	if t.Inner != nil {
		return t.Inner
	}
	return http.DefaultTransport
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/messages") {
		return t.inner().RoundTrip(req)
	}

	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("openaix: read request body: %w", err)
		}
	}
	var areq anthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		return nil, fmt.Errorf("openaix: decode anthropic request: %w", err)
	}

	base := strings.TrimRight(t.Base, "/")
	if base == "" {
		return nil, fmt.Errorf("openaix: empty base URL")
	}
	nreq, err := http.NewRequestWithContext(req.Context(), http.MethodPost,
		base+"/chat/completions", bytes.NewReader(mustMarshal(translateRequest(&areq))))
	if err != nil {
		return nil, fmt.Errorf("openaix: build openai request: %w", err)
	}
	// Fresh header set: anthropic-version/beta headers are meaningless
	// upstream. The credential moves from x-api-key to Bearer verbatim.
	if k := req.Header.Get("x-api-key"); k != "" {
		nreq.Header.Set("Authorization", "Bearer "+k)
	} else if a := req.Header.Get("Authorization"); a != "" {
		nreq.Header.Set("Authorization", a)
	}
	nreq.Header.Set("Content-Type", "application/json")
	if areq.Stream {
		nreq.Header.Set("Accept", "text/event-stream")
	} else {
		nreq.Header.Set("Accept", "application/json")
	}
	if ua := req.Header.Get("User-Agent"); ua != "" {
		nreq.Header.Set("User-Agent", ua)
	}

	nresp, err := t.inner().RoundTrip(nreq)
	if err != nil {
		return nil, err
	}
	nresp.Request = nreq // SDK error decoration reads URL/method from here

	statusOK := nresp.StatusCode >= 200 && nresp.StatusCode < 300
	isSSE := strings.Contains(nresp.Header.Get("Content-Type"), "text/event-stream")

	// Non-2xx: translate the error body, keep the status so the SDK's
	// retry policy (429/5xx/529) and error types still line up.
	if !statusOK {
		raw, _ := io.ReadAll(nresp.Body)
		nresp.Body.Close()
		return jsonResponse(nresp.Header, nresp.StatusCode, translateError(nresp.StatusCode, raw)), nil
	}

	switch {
	case areq.Stream && isSSE:
		return t.streamFromStream(nresp, areq.Model), nil
	case areq.Stream:
		// Gateway ignored stream:true and answered with JSON — synthesize
		// the SSE sequence from it so the SDK's streaming path still works.
		raw, err := readAllClose(nresp)
		if err != nil {
			return nil, err
		}
		return bufferSSEFromJSON(nresp.Header, raw, areq.Model, nresp), nil
	case isSSE:
		// Request was non-stream but the gateway streamed anyway: consume
		// the whole stream through the same state machine, answer JSON.
		st := newStreamState(areq.Model, noopEmit)
		if err := translateSSE(nresp.Body, st); err != nil {
			return nil, err
		}
		return jsonResponse(nresp.Header, http.StatusOK, mustMarshal(st.finalMessage())), nil
	default:
		raw, err := readAllClose(nresp)
		if err != nil {
			return nil, err
		}
		return jsonResponse(nresp.Header, nresp.StatusCode, messageFromJSON(raw, areq.Model)), nil
	}
}

// streamFromStream pipes an upstream OpenAI SSE stream through the
// translator into Anthropic SSE frames. The pump goroutine owns the
// upstream body; closing the returned body (or cancelling the request
// context) unwinds it.
func (t *Transport) streamFromStream(nresp *http.Response, reqModel string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		// newStreamState's emit writes one Anthropic frame per event.
		st := newStreamState(reqModel, sseWrite(pw))
		err := translateSSE(nresp.Body, st)
		if err != nil {
			var ae *streamAPIError
			if errors.As(err, &ae) {
				// In-band gateway error: surface it as an Anthropic SSE
				// error event, which the SDK turns into a rich API error.
				_ = st.emit("error", ae.payload)
				pw.Close()
				return
			}
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	resp := sseResponse(nresp.Header, pr, -1)
	resp.Request = nresp.Request
	return resp
}

// bufferSSEFromJSON answers a streaming request with pre-rendered frames
// built from a non-streaming JSON body.
func bufferSSEFromJSON(src http.Header, raw []byte, reqModel string, nresp *http.Response) *http.Response {
	var buf bytes.Buffer
	emit := sseWrite(&buf)
	var or oaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		// Gateway answered 200 with non-JSON (error page): fail loudly.
		_ = emit("error", errorPayload(http.StatusBadGateway, raw))
	} else if or.Error != nil {
		_ = emit("error", errorPayload(http.StatusInternalServerError, raw))
	} else {
		st := newStreamState(reqModel, emit)
		_ = st.consumeMessage(&or)
		_ = st.finish()
	}
	resp := sseResponse(src, io.NopCloser(bytes.NewReader(buf.Bytes())), int64(buf.Len()))
	resp.Request = nresp.Request
	return resp
}

// messageFromJSON translates a non-streaming OpenAI body into an Anthropic
// message; bodies that are error objects or not JSON at all come back as
// Anthropic error documents (the SDK only accepts messages on 2xx).
func messageFromJSON(raw []byte, reqModel string) []byte {
	var or oaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return translateError(http.StatusBadGateway, raw)
	}
	if or.Error != nil {
		return translateError(http.StatusInternalServerError, raw)
	}
	st := newStreamState(reqModel, noopEmit)
	_ = st.consumeMessage(&or)
	_ = st.finish()
	return mustMarshal(st.finalMessage())
}

func readAllClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openaix: read response body: %w", err)
	}
	return raw, nil
}

// skipResponseHeaders are hop-by-hop or body-derived; the translated body
// never keeps the upstream values.
var skipResponseHeaders = map[string]bool{
	"content-length":    true,
	"content-encoding":  true,
	"transfer-encoding": true,
	"connection":        true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipResponseHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// jsonResponse wraps an already-translated body as an HTTP response,
// preserving upstream headers (request-id, retry-after) along the way.
func jsonResponse(src http.Header, status int, body []byte) *http.Response {
	h := http.Header{}
	copyHeaders(h, src)
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Header:        h,
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
}

// sseResponse wraps a (possibly live) SSE body; contentLength < 0 means
// unknown/streaming.
func sseResponse(src http.Header, body io.ReadCloser, contentLength int64) *http.Response {
	h := http.Header{}
	copyHeaders(h, src)
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Del("Content-Length")
	if contentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Header:        h,
		ContentLength: contentLength,
		Body:          body,
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Shapes here are all plain structs/maps of primitives.
		panic(fmt.Sprintf("openaix: marshal %T: %v", v, err))
	}
	return b
}

func noopEmit(string, any) error { return nil }
