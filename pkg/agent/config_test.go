package agent

import "testing"

func TestWebToolModeAutoDetection(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"https://gateway.example.com/anthropic", "client"},
		{"https://api.anthropic.com", "server"},
		{"", "server"},
	}
	for _, tc := range cases {
		t.Run("base="+tc.baseURL, func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", tc.baseURL)
			eng := NewEngine(Config{}, nil)
			if got := eng.Config().WebToolMode; got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
			_, hasWeb := eng.Registry().Get("web_search")
			if wantWeb := tc.want == "client"; hasWeb != wantWeb {
				t.Fatalf("web_search registered = %v, want %v", hasWeb, wantWeb)
			}
		})
	}
}

func TestWebToolModeExplicitOverrides(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.com/anthropic")

	off := NewEngine(Config{WebToolMode: "off"}, nil)
	if off.Config().WebToolMode != "off" {
		t.Fatalf("off override lost: %q", off.Config().WebToolMode)
	}
	if _, has := off.Registry().Get("web_search"); has {
		t.Fatal("off mode must not register web tools")
	}

	server := NewEngine(Config{WebToolMode: "server"}, nil)
	if _, has := server.Registry().Get("web_search"); has {
		t.Fatal("server mode must not register client web tools")
	}
}
