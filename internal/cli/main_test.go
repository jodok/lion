package cli

import (
	"os"
	"testing"
)

// TestMain isolates every test in this package from the machine's real lion
// home directory, mirroring internal/voyager's TestMain.
//
// Most tests here call isolateHome (t.Setenv) themselves, but the ones that
// build a client directly via voyager.New — the auth-status validation tests,
// for example — don't, and voyager.New installs the persisted rate limiter by
// default (ratelimit.NewDefault). That limiter creates and chmods the resolved
// lion home and writes ratelimit.json into it, so without this guard a plain
// `go test ./...` on a developer's machine would mutate their real credentials
// directory and rate-limit state. Setting LION_HOME for the whole test binary
// makes that impossible regardless of which tests remember to isolate.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lion-cli-test-home")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("LION_HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
