package buildinfo

import "testing"

func TestCurrentUsesStampedVersion(t *testing.T) {
	previous := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = previous })

	if got := Current(); got != "v1.2.3" {
		t.Fatalf("Current() = %q, want v1.2.3", got)
	}
}

func TestDisplay(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "plain", version: "1.2.3", want: "v1.2.3"},
		{name: "prefixed", version: "v1.2.3", want: "v1.2.3"},
		{name: "development", version: DevelopmentVersion, want: "v0.1.0-dev"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Display(test.version); got != test.want {
				t.Fatalf("Display(%q) = %q, want %q", test.version, got, test.want)
			}
		})
	}
}
