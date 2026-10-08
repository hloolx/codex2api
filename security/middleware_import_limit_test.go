package security

import (
	"testing"
)

func TestIsLargeImportUploadPath(t *testing.T) {
	cases := map[string]bool{
		"/api/admin/accounts/import":                      true,
		"/api/admin/accounts/grok/import":                 true,
		"/api/admin/accounts/codex/agent-identity/import": true,
		"/api/admin/accounts/grok/sso/import":             true,
		"/api/admin/accounts":                             false,
		"/v1/responses":                                   false,
		"/api/admin/settings":                             false,
		"/api/admin/import":                               true,
		"/api/other/import":                               false,
	}
	for path, want := range cases {
		if got := IsLargeImportUploadPath(path); got != want {
			t.Errorf("IsLargeImportUploadPath(%q) = %v, want %v", path, got, want)
		}
	}
}
