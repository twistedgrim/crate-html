//go:build smoke

package smoke

import (
	"strings"
	"testing"
)

func TestCrateToken(t *testing.T) {
	out := runCrateOK(t, "token")
	got := strings.TrimSpace(out)
	if got != token {
		t.Errorf("token: got %q, want %q", got, token)
	}
}
