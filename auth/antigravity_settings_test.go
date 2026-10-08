package auth

import "testing"

func TestAntigravitySettingsExposeThoughtsRoundTrip(t *testing.T) {
	encoded, err := EncodeAntigravitySettings(AntigravitySettings{ExposeThoughts: true})
	if err != nil {
		t.Fatal(err)
	}
	if encoded != `{"expose_thoughts":true}` {
		t.Fatalf("encoded = %q, want only expose_thoughts", encoded)
	}
	parsed, err := ParseAntigravitySettings(encoded)
	if err != nil || !parsed.ExposeThoughts {
		t.Fatalf("parsed = %+v err=%v", parsed, err)
	}
	if empty, _ := EncodeAntigravitySettings(AntigravitySettings{}); empty != "{}" {
		t.Fatalf("empty settings encoded = %q", empty)
	}

	previous := ConfiguredAntigravitySettings()
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })
	SetConfiguredAntigravitySettings(parsed)
	if !AntigravityExposeThoughts() || !ConfiguredAntigravitySettings().ExposeThoughts {
		t.Fatal("runtime settings lost expose_thoughts")
	}
}
