package fakegh

import (
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func newSink(t *testing.T, c *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(c.handler())
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func pathEnv() string { return os.Getenv("PATH") }

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}
