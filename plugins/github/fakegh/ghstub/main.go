// Command ghstub is a test-only stand-in for `gh webhook forward`. It connects to
// nothing upstream: it accepts the forward flags the EDR uses (-R -E -U -S -H),
// polls GHSTUB_DELIVERIES_DIR for JSON delivery files, and POSTs each once to the
// -U target, signed with the -S secret (X-Hub-Signature-256, X-GitHub-Delivery,
// X-GitHub-Event). It exits cleanly on SIGTERM and, if GHSTUB_CRASH_AFTER=N is set,
// os.Exit(1)s after sending N deliveries to drive the restart+redelivery scenario.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

type delivery struct {
	Event      string          `json:"event"`
	Action     string          `json:"action"`
	DeliveryID string          `json:"delivery_id"`
	Payload    json.RawMessage `json:"payload"`
}

func main() {
	var target, secret string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-U", "--url":
			target = next(args, &i)
		case "-S", "--secret":
			secret = next(args, &i)
		case "-R", "--repo", "-E", "--events", "-H", "--host":
			_ = next(args, &i) // accepted, unused
		}
	}

	dir := os.Getenv("GHSTUB_DELIVERIES_DIR")
	crashAfter, _ := strconv.Atoi(os.Getenv("GHSTUB_CRASH_AFTER"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	sent := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, f := range pending(dir) {
				b, err := os.ReadFile(f) //nolint:gosec // test stub reads its own delivery dir
				if err != nil {
					continue
				}
				var d delivery
				if json.Unmarshal(b, &d) != nil {
					continue
				}
				if post(target, secret, d) != nil {
					continue
				}
				if os.Rename(f, f+".sent") != nil {
					continue
				}
				sent++
				if crashAfter > 0 && sent >= crashAfter {
					os.Exit(1)
				}
			}
		}
	}
}

func next(args []string, i *int) string {
	if *i+1 < len(args) {
		*i++
		return args[*i]
	}
	return ""
}

// pending returns the *.json delivery files not yet marked *.sent, sorted.
func pending(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	sort.Strings(entries)
	return entries
}

func post(target, secret string, d delivery) error {
	guid := d.DeliveryID
	if guid == "" {
		guid = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	body := []byte(d.Payload)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body)) //nolint:gosec // test stub POSTs to a caller-provided local target
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", d.Event)
	req.Header.Set("X-GitHub-Delivery", guid)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // see request construction above
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
