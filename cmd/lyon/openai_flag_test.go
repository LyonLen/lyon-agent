package main

import (
	"flag"
	"strings"
	"testing"

	"lyon-agent/pkg/openaix"
)

// TestOpenAIFlags: -openai targets the default gateway, -openai-base
// overrides it (and implies -openai), neither leaves the Anthropic path.
func TestOpenAIFlags(t *testing.T) {
	parse := func(args ...string) (commonFlags, *flag.FlagSet) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cf := registerFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return cf, fs
	}

	cf, _ := parse("-openai")
	cfg, err := cf.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIBase != openaix.DefaultBase {
		t.Fatalf("-openai base = %q, want %q", cfg.OpenAIBase, openaix.DefaultBase)
	}

	cf2, _ := parse("-openai-base", "https://my.gateway/v1")
	cfg2, err := cf2.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.OpenAIBase != "https://my.gateway/v1" {
		t.Fatalf("-openai-base = %q", cfg2.OpenAIBase)
	}

	cf3, fs3 := parse()
	cfg3, err := cf3.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.OpenAIBase != "" {
		t.Fatalf("OpenAIBase without flags = %q, want empty", cfg3.OpenAIBase)
	}

	// Help text advertises the flag so it is discoverable.
	if f := fs3.Lookup("openai-base"); f == nil || !strings.Contains(f.Usage, "OpenAI") {
		t.Fatalf("-openai-base usage: %v", f)
	}
}
