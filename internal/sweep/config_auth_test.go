package sweep

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGitHubReviewUsesHostScopedLogin(t *testing.T) {
	dir := t.TempDir()
	gh := `#!/bin/sh
[ "$*" = 'auth token --hostname github.com' ] || exit 1
echo stored-login
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TEAM_TOKEN", "")
	for _, tc := range []struct {
		name, server, tokenEnv, envValue, want string
	}{
		{"signed in", "https://github.com", "GITHUB_TOKEN", "", "stored-login"},
		{"explicit token wins", "https://github.com", "GITHUB_TOKEN", "explicit", "explicit"},
		{"different host", "https://git.example", "GITHUB_TOKEN", "", ""},
		{"custom credential", "https://github.com", "TEAM_TOKEN", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.envValue)
			path := filepath.Join(t.TempDir(), "sweep.yaml")
			raw := fmt.Sprintf("review: true\nresults_dir: results\nconnections:\n  - name: oss\n    provider: github\n    server_url: %s\n    token_env: %s\n    include:\n      repositories: [go-chi/render]\n", tc.server, tc.tokenEnv)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if tc.want == "" {
				if err == nil {
					t.Fatal("used a github.com login for a different credential")
				}
				return
			}
			if err != nil || cfg.Connections[0].token != tc.want {
				t.Fatalf("unexpected credential selection: %v", err)
			}
		})
	}
}
