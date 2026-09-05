package features

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// ghWorld is the github plugin scenarios' mutable per-scenario state. It wraps a
// fresh *world (reused for its dataDir/cfgPath/listen fields and its
// startServe/stopServe/runCLI subprocess management, all same-package) plus a fake
// GitHub server and the config knobs a github.feature Given/When step needs. Every
// scenario in features/github.feature gets a fresh one via sc.Before, matching the
// single-worker sequential pattern InitializeScenario already uses for `w`.
type ghWorld struct {
	sw   *world
	fake *fakegh.Server

	// config knobs baked into the config.toml written for the generic Given
	webhookSecret string
	ingress       string
	ghPath        string
	notifications bool
	reconcileSecs int

	// last HTTP results captured by When steps
	lastOp       ghQueryResult
	lastOpStatus int
	lastWebhook  int

	// forward-ingress scenario extras (AC-GH-FORWARD)
	deliveriesDir string
	stubEnvSet    bool

	// timing samples for p95 scenarios (S3/F1)
	samples []time.Duration

	// AC-GH-SINGLEFLIGHT: captured concurrent responses to compare for identity
	forwardCompare []ghQueryResult
	// AC-GH-FLOOR: channel the paused read-through goroutine reports onto
	forwardFloorDone chan floorResult
	// AC-GH-CURSOR: the since cursor value observed after the first reconcile
	cursorT string
}

// ghNodeResult / ghQueryResult mirror the JSON wire shape the plugin's
// /plugins/github/graphql handler returns (executor.go's queryResult / nodeResult).
// This is a black-box copy of the CONTRACT, not an import of the plugin package.
type ghNodeResult struct {
	Key      string          `json:"key"`
	ETag     string          `json:"etag"`
	Typename string          `json:"typename"`
	Node     json.RawMessage `json:"node"`
}

type ghQueryResult struct {
	Data  map[string]json.RawMessage `json:"data,omitempty"`
	Nodes []ghNodeResult             `json:"nodes,omitempty"`
	Scope string                     `json:"scope,omitempty"`
}

// ---------- setup / teardown ----------

func (g *ghWorld) init() error {
	g.sw = &world{}
	if err := g.sw.init(); err != nil {
		return err
	}
	g.fake = fakegh.New()
	g.webhookSecret = "test-webhook-secret"
	g.ingress = "tunnel" // avoids spawning the real `gh` binary; AC-GH-FORWARD overrides
	g.ghPath = ""
	g.notifications = false
	g.reconcileSecs = 3600 // long by default; "reconcile runs once" steps shorten + restart
	return nil
}

func (g *ghWorld) cleanup() {
	if g.sw != nil {
		g.sw.cleanup()
	}
	if g.fake != nil {
		g.fake.Close()
	}
	if g.stubEnvSet {
		_ = os.Unsetenv("GHSTUB_DELIVERIES_DIR")
		_ = os.Unsetenv("GHSTUB_CRASH_AFTER")
		g.stubEnvSet = false
	}
}

// writeConfig renders config.toml for the current knobs and starts the server.
func (g *ghWorld) writeConfig() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", "test")
	fmt.Fprintf(&b, "listen = %q\n", g.sw.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", g.sw.dataDir)
	b.WriteString("lagThresholdSeconds = 30.000000\n")
	b.WriteString("[plugins.github]\n")
	fmt.Fprintf(&b, "baseURL = %q\n", g.fake.URL)
	fmt.Fprintf(&b, "graphqlURL = %q\n", g.fake.URL+"/graphql")
	fmt.Fprintf(&b, "ingress = %q\n", g.ingress)
	fmt.Fprintf(&b, "webhookSecret = %q\n", g.webhookSecret)
	if g.ghPath != "" {
		fmt.Fprintf(&b, "ghPath = %q\n", g.ghPath)
	}
	fmt.Fprintf(&b, "selfURL = %q\n", "http://"+g.sw.listen)
	fmt.Fprintf(&b, "reconcileIntervalSeconds = %d\n", g.reconcileSecs)
	fmt.Fprintf(&b, "notifications = %t\n", g.notifications)
	b.WriteString("[plugins.github.pin]\n")
	b.WriteString("commits = true\nreleases = true\nmergedPRsAfterDays = 7\nclosedIssuesAfterDays = 30\n")
	// A non-zero per-kind TTL lets scenarios deterministically seed a "fresh" node
	// (fetched_at = now) vs an "expired" one (fetched_at far in the past) without
	// needing a second Given phrase; TTL default-off in production is unaffected
	// since this only shapes the test harness's own config.
	b.WriteString("[plugins.github.ttl]\n")
	b.WriteString("issue = 3600\npr = 3600\ncheckRun = 3600\n")
	return os.WriteFile(g.sw.cfgPath, []byte(b.String()), 0o644)
}

func (g *ghWorld) startDefault() error {
	if err := g.writeConfig(); err != nil {
		return err
	}
	return g.sw.startServe()
}

// restartWithShortReconcile stops the server, rewrites config with a 1s reconcile
// interval, restarts on the SAME data dir, and blocks past the first tick — the
// only black-box way to make "the since-cursor reconcile runs once" deterministic
// against a plugin whose reconcile loop is a plain time.Ticker with no manual
// trigger endpoint.
func (g *ghWorld) reconcileRunsOnce() error {
	g.sw.stopServe()
	g.reconcileSecs = 1
	if err := g.writeConfig(); err != nil {
		return err
	}
	if err := g.sw.startServe(); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond) // one 1s tick, plus margin
	return nil
}

// ---------- GraphQL / webhook HTTP helpers ----------

func (g *ghWorld) postOp(op string, vars map[string]any) (ghQueryResult, int, error) {
	body, _ := json.Marshal(map[string]any{"op": op, "variables": vars})
	resp, err := http.Post("http://"+g.sw.listen+"/plugins/github/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		return ghQueryResult{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var qr ghQueryResult
	_ = json.Unmarshal(raw, &qr)
	return qr, resp.StatusCode, nil
}

func newDeliveryID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000))
}

// postWebhook signs payload with secret (the empty string yields no signature at
// all, matching AC-GH-HMAC's "invalid signature" wrong-secret case when secret !=
// the configured one) and POSTs it to the running server's webhook route.
func (g *ghWorld) postWebhook(secret, event, action string, payload map[string]any) (int, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	if action != "" {
		payload["action"] = action
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "http://"+g.sw.listen+"/plugins/github/webhook", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", newDeliveryID())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// ---------- direct github.db access (same pattern as latestTemplatePayload) ----------

func (g *ghWorld) ghDB() (*sql.DB, error) {
	path := filepath.Join(g.sw.dataDir, "github.db")
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout%285000%29")
}

func (g *ghWorld) seedNode(key, typename string, body map[string]any, etag string, pinned bool, fetchedAt, updatedAt time.Time) error {
	db, err := g.ghDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	j, _ := json.Marshal(body)
	p := 0
	if pinned {
		p = 1
	}
	_, err = db.Exec(`INSERT INTO github_nodes (key, typename, node_json, etag, pinned, fetched_at, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET typename=excluded.typename, node_json=excluded.node_json,
		  etag=excluded.etag, pinned=excluded.pinned, fetched_at=excluded.fetched_at, updated_at=excluded.updated_at`,
		key, typename, j, etag, p, fetchedAt.UTC().Format(time.RFC3339Nano), updatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// seedListNode seeds a list-result row (raw JSON body, typename "_list") plus its
// covering tag row, matching store.putList's on-disk shape (EDR §"Tag index").
func (g *ghWorld) seedListNode(listKey, tag string, raw []byte, at time.Time) error {
	db, err := g.ghDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO github_nodes (key, typename, node_json, etag, pinned, fetched_at, updated_at)
		VALUES (?, '_list', ?, '', 0, ?, ?)
		ON CONFLICT(key) DO UPDATE SET node_json=excluded.node_json, fetched_at=excluded.fetched_at, updated_at=excluded.updated_at`,
		listKey, raw, at.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO github_tags (tag, key) VALUES (?, ?)`, tag, listKey)
	return err
}

func (g *ghWorld) nodeExists(key string) (bool, error) {
	db, err := g.ghDB()
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	var one int
	err = db.QueryRow(`SELECT 1 FROM github_nodes WHERE key=?`, key).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (g *ghWorld) nodeTitle(key string) (string, error) {
	db, err := g.ghDB()
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var raw []byte
	if err := db.QueryRow(`SELECT node_json FROM github_nodes WHERE key=?`, key).Scan(&raw); err != nil {
		return "", err
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	t, _ := m["title"].(string)
	return t, nil
}

func (g *ghWorld) countEvents(typ, key string) (int, error) {
	db, err := g.ghDB()
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var n int
	q := `SELECT count(*) FROM events WHERE source='github'`
	args := []any{}
	if typ != "" {
		q += ` AND type=?`
		args = append(args, typ)
	}
	if key != "" {
		q += ` AND key=?`
		args = append(args, key)
	}
	err = db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func (g *ghWorld) cursorValue(name string) (string, error) {
	db, err := g.ghDB()
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var v string
	err = db.QueryRow(`SELECT value FROM cursors WHERE name=?`, name).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (g *ghWorld) setCursorRow(name, value string) error {
	db, err := g.ghDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT INTO cursors (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value=excluded.value`, name, value)
	return err
}

// ---------- misc ----------

// buildGhStub compiles the ghstub test binary directly (not via fakegh.BuildGhStub,
// which needs a testing.TB — unavailable inside a godog step closure since
// testing.TB carries an unexported method only the testing package can implement).
func buildGhStub() (string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("ghstub-%d", time.Now().UnixNano()))
	cmd := exec.Command("go", "build", "-o", out, "./plugins/github/fakegh/ghstub")
	cmd.Dir = repoRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build ghstub: %w: %s", err, b)
	}
	return out, nil
}

func p95(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted))*0.95+0.9999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
