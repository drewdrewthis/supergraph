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
	forwardRepo   string
	hookRepos     []string
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
	// Default hook allowlist covers every owner/repo the harness registers via
	// AddRepo; scenarios that assert hook creation (F3, forward redelivery) need
	// their repo listed, and production defaults to empty (no hooks).
	g.hookRepos = []string{"o/r", "o/newrepo"}
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
	// A token is required for the plugin to leave dormant mode (Start returns
	// immediately with no token resolved, config or GITHUB_TOKEN); fakegh itself
	// never checks auth, so any non-empty value exercises the configured path.
	fmt.Fprintf(&b, "token = %q\n", "test-token")
	fmt.Fprintf(&b, "baseURL = %q\n", g.fake.URL)
	fmt.Fprintf(&b, "graphqlURL = %q\n", g.fake.URL+"/graphql")
	fmt.Fprintf(&b, "ingress = %q\n", g.ingress)
	fmt.Fprintf(&b, "webhookSecret = %q\n", g.webhookSecret)
	if g.ghPath != "" {
		fmt.Fprintf(&b, "ghPath = %q\n", g.ghPath)
	}
	fmt.Fprintf(&b, "selfURL = %q\n", "http://"+g.sw.listen)
	if g.forwardRepo != "" {
		fmt.Fprintf(&b, "forwardRepo = %q\n", g.forwardRepo)
	}
	if len(g.hookRepos) > 0 {
		b.WriteString("hookRepos = [")
		for i, r := range g.hookRepos {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", r)
		}
		b.WriteString("]\n")
	}
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
	// The github-query join scenarios (features/github-query.feature) read claude
	// sessions through the running claude plugin, so it is configured here at an
	// empty projectsDir (nothing to scan) with a long scan interval — harmless to
	// the github-only scenarios, which never query it. tmux is intentionally left
	// unconfigured: its Migrate still runs (creating tmux.db and publishing its
	// accessor), so seeded panes are readable, but its reconcile loop stays dormant
	// and never clobbers a seeded row.
	b.WriteString("[plugins.claude]\n")
	fmt.Fprintf(&b, "projectsDir = %q\n", g.sw.claudeProjectsDir)
	fmt.Fprintf(&b, "settingsPath = %q\n", g.sw.claudeSettingsPath)
	b.WriteString("scanIntervalSeconds = 3600\nretentionDays = 3650\n")
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

// ===================== @live helpers (real GitHub) =====================
//
// The @live github scenarios drive the SAME serve subprocess machinery as the
// @local ones, but point the plugin at api.github.com instead of the fakegh
// server. The PAT is passed to the child via the inherited GITHUB_TOKEN env (the
// plugin's New() falls back to os.Getenv("GITHUB_TOKEN") when the config omits a
// token key), so the credential is NEVER written to config.toml or any repo file.
// Reconcile is pinned to 3600s and never fires inside a scenario window, so a live
// run creates no webhooks and mutates nothing on the account (read-through only).

// liveRepo returns the owner/repo the live scenarios target, from LIVE_REPO
// (owner/repo). GITHUB_ORG + LIVE_REPO=repo (bare) is also accepted.
func liveRepo() (owner, repo string, err error) {
	v := strings.TrimSpace(os.Getenv("LIVE_REPO"))
	if v == "" {
		return "", "", fmt.Errorf("LIVE_REPO not set (want owner/repo)")
	}
	if o, r, ok := strings.Cut(v, "/"); ok {
		return o, r, nil
	}
	org := strings.TrimSpace(os.Getenv("GITHUB_ORG"))
	if org == "" {
		return "", "", fmt.Errorf("LIVE_REPO=%q has no owner and GITHUB_ORG is unset", v)
	}
	return org, v, nil
}

// writeLiveConfig renders a config.toml that targets real GitHub. No token key is
// written; the child inherits GITHUB_TOKEN. ingress=tunnel avoids spawning a gh
// child; the long reconcile interval keeps the run read-only.
func (g *ghWorld) writeLiveConfig() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", "live")
	fmt.Fprintf(&b, "listen = %q\n", g.sw.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", g.sw.dataDir)
	b.WriteString("lagThresholdSeconds = 30.000000\n")
	b.WriteString("[plugins.github]\n")
	b.WriteString("ingress = \"tunnel\"\n")
	b.WriteString("reconcileIntervalSeconds = 3600\n")
	fmt.Fprintf(&b, "notifications = %t\n", g.notifications)
	b.WriteString("[plugins.claude]\n")
	fmt.Fprintf(&b, "projectsDir = %q\n", g.sw.claudeProjectsDir)
	fmt.Fprintf(&b, "settingsPath = %q\n", g.sw.claudeSettingsPath)
	b.WriteString("scanIntervalSeconds = 3600\nretentionDays = 3650\n")
	return os.WriteFile(g.sw.cfgPath, []byte(b.String()), 0o644)
}

// startLive boots the serve subprocess against real GitHub, failing fast with an
// operator-facing error if the PAT is absent.
func (g *ghWorld) startLive() error {
	if os.Getenv("GITHUB_TOKEN") == "" {
		return fmt.Errorf("GITHUB_TOKEN not set: the @live github scenarios need a real PAT (see README > Live scenarios)")
	}
	if _, _, err := liveRepo(); err != nil {
		return err
	}
	if err := g.writeLiveConfig(); err != nil {
		return err
	}
	return g.sw.startServe()
}

// warmOp POSTs a named op to the running plugin's /plugins/github/graphql, which
// fetches from live GitHub and caches the returned nodes. It returns the executor
// status so a caller can assert the warm actually reached upstream.
func (g *ghWorld) warmOp(op, owner, repo string) (int, error) {
	_, status, err := g.postOp(op, map[string]any{"owner": owner, "repo": repo})
	return status, err
}

// restOpenIssueNumbers lists a repo's open issues via the REST API (the same
// authority the scenario compares against), dropping pull requests (which the
// issues endpoint interleaves and which carry a "pull_request" member).
func restOpenIssueNumbers(owner, repo string) ([]int, error) {
	var nums []int
	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open&per_page=100&page=%d", owner, repo, page)
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "token "+os.Getenv("GITHUB_TOKEN"))
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("REST issues status %d: %s", resp.StatusCode, body)
		}
		var page1 []struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		}
		if err := json.Unmarshal(body, &page1); err != nil {
			return nil, err
		}
		for _, it := range page1 {
			if len(it.PullRequest) == 0 {
				nums = append(nums, it.Number)
			}
		}
		if len(page1) < 100 {
			break
		}
	}
	sort.Ints(nums)
	return nums, nil
}

// waitLogLine polls the serve subprocess stdout until it contains substr or the
// deadline passes, returning whether it was seen.
func (g *ghWorld) waitLogLine(substr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if g.sw.serve != nil && strings.Contains(g.sw.serve.stdout.String(), substr) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return g.sw.serve != nil && strings.Contains(g.sw.serve.stdout.String(), substr)
}
