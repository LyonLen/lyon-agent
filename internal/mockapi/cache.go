package mockapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// cacheStore simulates Anthropic-style prompt caching over raw request
// bodies. The gateway hashes the token stream in semantic order — tools,
// then system, then messages — at each cache_control breakpoint, and a later
// request reads the longest previously-written prefix that still matches.
//
// Raw bytes (not re-serialized JSON) are hashed on purpose: this is what
// makes the simulator a drift detector. If a tool schema's key order changes
// between requests, the tools segment no longer matches and the "cache"
// reports a miss — exactly what a real prefix cache does.
type cacheStore struct {
	mu      sync.Mutex
	entries map[[32]byte]int64 // sha256(prefix) → prefix byte length
	lens    []int64            // distinct stored lengths, unsorted
	min     int64              // don't store prefixes shorter than this (bytes)

	read, create, input, requests int64
}

// observe processes one request body and returns the usage it would
// produce: tokens read from cache, tokens written to cache, fresh tokens.
func (s *cacheStore) observe(raw []byte) (read, create, input int64) {
	var req struct {
		Tools        json.RawMessage `json:"tools"`
		System       json.RawMessage `json:"system"`
		Messages     json.RawMessage `json:"messages"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	_ = json.Unmarshal(raw, &req)

	// Semantic prefix order: tools, system, messages — concatenated element
	// by element, skipping array brackets and separators. This mirrors how
	// the real cache tokenizes content blocks: appending a message extends
	// the stream in place, so the previous stream stays a true byte prefix
	// (an array's closing "]" would otherwise sit at a different offset in
	// every request and no history could ever match). Raw element bytes are
	// preserved exactly, key order included — drift inside a schema still
	// breaks the prefix.
	toolElems := elements(req.Tools)
	sysElems := elements(req.System)
	msgElems := elements(req.Messages)

	n := lensOf(toolElems) + lensOf(sysElems) + lensOf(msgElems)
	stream := make([]byte, 0, n)
	for _, e := range toolElems {
		stream = append(stream, e...)
	}
	for _, e := range sysElems {
		stream = append(stream, e...)
	}
	for _, e := range msgElems {
		stream = append(stream, e...)
	}
	total := int64(len(stream))

	// Breakpoints: a cache_control inside the system block sits at the end
	// of tools+system (tools precede system semantically); a top-level
	// cache_control checkpoints the full stream.
	var bps []int64
	if len(req.System) > 0 && bytes.Contains(req.System, []byte("cache_control")) {
		bps = append(bps, int64(lensOf(toolElems)+lensOf(sysElems)))
	}
	if len(req.CacheControl) > 0 && total > 0 {
		bps = append(bps, total)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++

	// Longest stored prefix of this stream that still matches byte-for-byte.
	var best int64
	for _, L := range s.lens {
		if L > total || L <= best {
			continue
		}
		if _, ok := s.entries[sha256.Sum256(stream[:L])]; ok {
			best = L
		}
	}

	// Standard breakpoint accounting: the span up to the last breakpoint is
	// read (if a matching prefix was stored) or written; anything past the
	// last breakpoint is uncached input.
	var lastBp int64
	for _, bp := range bps {
		if bp > lastBp {
			lastBp = bp
		}
	}
	read = toks(best)
	if lastBp > best {
		create = toks(lastBp - best)
	}
	covered := best
	if lastBp > covered {
		covered = lastBp
	}
	if total > covered {
		input = toks(total - covered)
	}

	// Write this request's breakpoints for future requests to read.
	for _, bp := range bps {
		if bp < s.min {
			continue
		}
		h := sha256.Sum256(stream[:bp])
		if _, ok := s.entries[h]; !ok {
			s.entries[h] = bp
			s.lens = append(s.lens, bp)
		}
	}

	s.read += read
	s.create += create
	s.input += input
	return read, create, input
}

// Stats returns cumulative token accounting across all observed requests.
func (s *cacheStore) Stats() (read, create, input, requests int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read, s.create, s.input, s.requests
}

// elements splits a JSON array into its raw element bytes, or nil when the
// value is absent/not an array.
func elements(raw json.RawMessage) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	out := make([][]byte, len(elems))
	for i, e := range elems {
		out[i] = e
	}
	return out
}

func lensOf(elems [][]byte) int {
	n := 0
	for _, e := range elems {
		n += len(e)
	}
	return n
}

// toks estimates tokens from bytes at ~4 bytes/token, rounding to nearest.
func toks(bytes int64) int64 { return (bytes + 2) / 4 }
