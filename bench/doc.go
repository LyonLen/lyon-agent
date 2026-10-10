// Package bench holds the performance acceptance suite. It drives the real
// engine against internal/mockapi and enforces hard thresholds: violations
// fail the build rather than just printing numbers.
//
//	go test ./bench -run 'TestLoad|TestCache|TestSSE' -v
//
// The optional LYON_LIVE_TEST=1 run swaps the mock for the real endpoint
// and reports (not asserts) cache hit rate and time-to-first-token.
package bench
