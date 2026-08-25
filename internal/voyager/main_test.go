package voyager

import (
	"os"
	"testing"
)

// TestMain isolates every test in this package from the machine's real lion
// home directory. Client construction defaults to a persisted rate limiter
// (ratelimit.NewDefault, see client.go) that writes a small state file under
// internal/config's resolved home dir; without this, running the test suite
// would create/modify that file on whatever machine runs the tests. Point
// LION_HOME at a throwaway temp directory for the whole test binary instead.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lion-voyager-test-home")
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
