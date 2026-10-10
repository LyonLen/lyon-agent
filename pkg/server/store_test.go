package server

import (
	"testing"
	"time"

	"lyon-agent/pkg/agent"
)

// newTestSession builds a real session (no API traffic happens until
// Submit + drive, which these tests never trigger).
func newTestSession(t *testing.T) *agent.Session {
	t.Helper()
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	return sess
}

func TestStoreAdmissionCap(t *testing.T) {
	st := NewStore(2, 0)
	for i := 0; i < 2; i++ {
		if _, err := st.Create(newTestSession(t)); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := st.Create(newTestSession(t)); err == nil {
		t.Fatal("expected ErrFull at cap, got nil")
	}
	if st.Len() != 2 {
		t.Fatalf("Len=%d, want 2", st.Len())
	}
}

func TestStoreGetDelete(t *testing.T) {
	st := NewStore(0, 0)
	id, err := st.Create(newTestSession(t))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Get(id); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := st.Get("nope"); err == nil {
		t.Fatal("expected ErrNotFound for unknown id")
	}
	if !st.Delete(id) {
		t.Fatal("Delete(existing) = false")
	}
	if st.Delete(id) {
		t.Fatal("second Delete = true, want false")
	}
	if st.Len() != 0 {
		t.Fatalf("Len=%d after delete, want 0", st.Len())
	}
}

func TestStoreReapAndCloseAll(t *testing.T) {
	st := NewStore(0, 50*time.Millisecond)
	id, err := st.Create(newTestSession(t))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Not yet idle: Get refreshes lastActive, so no reap.
	if n := st.Reap(); n != 0 {
		t.Fatalf("reaped %d before idle", n)
	}
	time.Sleep(80 * time.Millisecond)
	if n := st.Reap(); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if _, err := st.Get(id); err == nil {
		t.Fatal("reaped session still retrievable")
	}

	// CloseAll drains everything and latches.
	id2, _ := st.Create(newTestSession(t))
	st.CloseAll()
	if st.Len() != 0 {
		t.Fatalf("Len=%d after CloseAll", st.Len())
	}
	if _, err := st.Create(newTestSession(t)); err == nil {
		t.Fatal("Create after CloseAll should fail")
	}
	_ = id2
}

func TestRoutesNoAPI(t *testing.T) {
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	st := NewStore(1, 0)
	s := New(eng, st)
	ts := newHTTPTestServer(t, s)

	// healthz
	code, body := ts.get("/healthz")
	if code != 200 || !contains(body, `"status":"ok"`) {
		t.Fatalf("healthz = %d %s", code, body)
	}

	// create (no message) → 201 + session_id
	code, body = ts.postJSON("/v1/sessions", `{}`)
	if code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	sid := jsonGetString(t, body, "session_id")
	if sid == "" {
		t.Fatalf("no session_id in %s", body)
	}

	// admission cap → 429
	code, _ = ts.postJSON("/v1/sessions", `{}`)
	if code != 429 {
		t.Fatalf("second create = %d, want 429", code)
	}

	// stats of existing session
	code, _ = ts.get("/v1/sessions/" + sid + "/stats")
	if code != 200 {
		t.Fatalf("stats = %d, want 200", code)
	}

	// unknown session → 404 on all three
	if code, _ := ts.postJSON("/v1/sessions/deadbeef/messages", `{"message":"x"}`); code != 404 {
		t.Fatalf("messages unknown = %d, want 404", code)
	}
	if code, _ := ts.get("/v1/sessions/deadbeef/stats"); code != 404 {
		t.Fatalf("stats unknown = %d, want 404", code)
	}
	if code, _ := ts.del("/v1/sessions/deadbeef"); code != 404 {
		t.Fatalf("delete unknown = %d, want 404", code)
	}

	// empty message → 400
	if code, _ := ts.postJSON("/v1/sessions/"+sid+"/messages", `{"message":"  "}`); code != 400 {
		t.Fatalf("blank message = %d, want 400", code)
	}

	// delete existing → 204, then stats → 404
	if code, _ := ts.del("/v1/sessions/" + sid); code != 204 {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _ := ts.get("/v1/sessions/" + sid + "/stats"); code != 404 {
		t.Fatalf("stats after delete = %d, want 404", code)
	}
}
