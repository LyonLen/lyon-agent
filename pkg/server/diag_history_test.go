package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
)

func TestDiagHistoryPromptEnd(t *testing.T) {
	client := newConnectFixture(t)
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := cres.Msg.Session

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	counts := map[string]int{}
	last := map[string]string{}
	done := make(chan error, 1)
	go func() {
		stream, err := client.WatchEvents(wctx, connect.NewRequest(&lyonv1.WatchEventsRequest{SessionId: sess.Id, FromSeq: 0}))
		if err != nil {
			done <- err
			return
		}
		for stream.Receive() {
			ev := stream.Msg().Event
			counts[ev.Type.String()]++
			last[ev.Type.String()] = ev.TypeName + "|" + ev.Detail + "|" + ev.StopReason
			if ev.ToolName != "" {
				t.Logf("seq=%d %s tool=%s sub=%v exec=%d err=%v", ev.Seq, ev.Type, ev.ToolName, ev.Sub, ev.ExecId, ev.IsError)
			}
		}
		done <- stream.Err()
	}()

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sess.Id, Prompt: "connect history: one turn.",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	deadline := time.After(12 * time.Second)
	<-deadline
	for k, v := range counts {
		t.Logf("%-30s %4d  last=%q", k, v, last[k])
	}
}
