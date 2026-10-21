package server

import (
	"context"
	"os"
	"testing"
)

// Opt-in live probe: hits the endpoint the process is configured against
// and prints the list the panel would show. Run with
//
//	LYON_LIVE_GATEWAY=1 go test ./pkg/server -run TestLiveGatewayModels -v
func TestLiveGatewayModels(t *testing.T) {
	if os.Getenv("LYON_LIVE_GATEWAY") == "" {
		t.Skip("set LYON_LIVE_GATEWAY=1 to hit the configured endpoint")
	}
	list, source, errText := buildModels(context.Background(), "")
	t.Logf("endpoint=%s source=%s err=%q rows=%d", modelsEndpoint(""), source, errText, len(list))
	for _, m := range list {
		t.Logf("  %-28s provider=%-10s ctx=%d vision=%v", m.Id, m.Provider, m.Context, m.Vision)
	}
	if source != "gateway" || len(list) == 0 {
		t.Fatalf("expected a live gateway list, got source=%q rows=%d err=%q", source, len(list), errText)
	}
}
