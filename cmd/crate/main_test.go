package main

import "testing"

func TestCommandNeedsConfig(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{command: "version", want: false},
		{command: "update", want: false},
		{command: "status", want: true},
		{command: "token create <name>", want: true},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			if got := commandNeedsConfig(test.command); got != test.want {
				t.Fatalf("commandNeedsConfig(%q) = %v, want %v", test.command, got, test.want)
			}
		})
	}
}
