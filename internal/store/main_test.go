package store

import (
	"os"
	"testing"
)

// TestMain isolates every test in this package from the machine's real lion
// home directory, mirroring internal/cli and internal/voyager's TestMain:
// DefaultPath resolves the lion home dir via internal/config and creates
// it, so without this a plain `go test ./...` would create/modify
// ~/Library/Application Support/lion (or the platform equivalent) on
// whatever machine runs the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lion-store-test-home")
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
