package sessions

import (
	"path/filepath"
	"testing"
)

func TestOMPResumeUsesCheckedSessionIdentity(t *testing.T) {
	profileRoot := filepath.Join(t.TempDir(), ".omp", "profiles")
	journal := func(profile string) string {
		return filepath.Join(profileRoot, profile, "agent", "sessions", "--workspace--", "session.jsonl")
	}
	for _, tc := range []struct {
		name, path, id, want string
	}{
		{name: "default", id: "abc-123", want: "omp --resume abc-123"},
		{name: "named", path: journal("client-a.v2"), id: "abc-123", want: "omp --profile client-a.v2 --resume abc-123"},
		{name: "invalid profile", path: journal("--help"), id: "abc-123"},
		{name: "flag as id", id: "--help"},
		{name: "flag as named id", path: journal("client-a.v2"), id: "--help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeCommand("", "omp", tc.id, "", tc.path); got != tc.want {
				t.Fatalf("resume command = %q, want %q", got, tc.want)
			}
		})
	}
}
