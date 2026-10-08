package database

import "testing"

func TestNormalizeSessionIdentityFingerprintDefault(t *testing.T) {
	for input, want := range map[string]string{
		" Single_Machine_Multi_Window ": "single_machine_multi_window",
		"device":                        "device",
		"unknown":                       "off",
		"":                              "off",
	} {
		if got := NormalizeCodexFingerprintDefaultMode(input); got != want {
			t.Errorf("NormalizeCodexFingerprintDefaultMode(%q) = %q, want %q", input, got, want)
		}
	}
}
