package models

import "testing"

func TestCapabilityProfileValuesInRangeAndComplete(t *testing.T) {
	for _, info := range registry {
		if info.Profile == (Profile{}) {
			t.Errorf("%s: registry entry has no capability profile", info.ID)
			continue
		}
		p := info.Profile
		for name, v := range map[string]byte{
			"reasoning": p.Reasoning, "long_haul": p.LongHaul,
			"openness": p.Openness, "speed": p.Speed, "thrift": p.Thrift,
		} {
			if v > 5 {
				t.Errorf("%s: %s = %d, scale is 0..5", info.ID, name, v)
			}
		}
	}
}

func TestCapProfileLookupAndDefault(t *testing.T) {
	info := Lookup("claude-opus-5-5")
	if info == nil {
		t.Fatal("claude-opus-5-5 not registered")
	}
	p := info.CapProfile()
	if p.Reasoning != 5 || p.Thrift != 1 {
		t.Fatalf("opus profile = %+v, want deep thinking, not thrifty", p)
	}
	// Unprofiled entries read as mid-scale, never as zeros.
	blank := &Info{ID: "mystery-model"}
	if got := blank.CapProfile(); got != (Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 3, Thrift: 3}) {
		t.Fatalf("default profile = %+v", got)
	}
}
