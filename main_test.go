package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

//nolint:paralleltest // t.Setenv cannot be used in parallel tests
func TestRunRejectsNonPositiveDefaultTTL(t *testing.T) {
	// The missing kubeconfig and cancelled context stop run from reaching a cluster if the flag is accepted.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))

	tests := []struct {
		name    string
		arg     string
		wantErr bool
	}{
		{name: "zero", arg: "--default-ttl=0", wantErr: true},
		{name: "negative", arg: "--default-ttl=-1h", wantErr: true},
		{name: "positive control", arg: "--default-ttl=1h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := run(ctx, []string{"kar", tt.arg}, io.Discard)
			if !tt.wantErr {
				if err != nil && strings.Contains(err.Error(), "default-ttl") {
					t.Fatalf("run(%q) = %v, want no default-ttl error", tt.arg, err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), "default-ttl") {
				t.Fatalf("run(%q) = %v, want error mentioning default-ttl", tt.arg, err)
			}
		})
	}
}
