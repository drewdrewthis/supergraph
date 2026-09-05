package fakegh

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func get(t *testing.T, url string, ifNoneMatch string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func remaining(resp *http.Response) string { return resp.Header.Get("X-RateLimit-Remaining") }

func TestETagConditional304(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddIssue("o", "r", 5, "hi", "open")

	url := s.URL + "/repos/o/r/issues/5"
	r1 := get(t, url, "")
	if r1.StatusCode != 200 {
		t.Fatalf("want 200, got %d", r1.StatusCode)
	}
	etag := r1.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	remAfter := remaining(r1)
	_ = r1.Body.Close()

	r2 := get(t, url, etag)
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304, got %d", r2.StatusCode)
	}
	if remaining(r2) != remAfter {
		t.Fatalf("304 spent quota: %s -> %s", remAfter, remaining(r2))
	}
	_ = r2.Body.Close()

	s.Mutate("issue:o/r#5", func(b map[string]any) { b["title"] = "changed" })
	r3 := get(t, url, etag)
	if r3.StatusCode != 200 {
		t.Fatalf("stale etag should 200, got %d", r3.StatusCode)
	}
	_ = r3.Body.Close()
}

func TestLinkPagination(t *testing.T) {
	s := New()
	defer s.Close()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		s.AddRepo("o", n)
	}

	r1 := get(t, s.URL+"/user/repos?per_page=2&page=1", "")
	if !strings.Contains(r1.Header.Get("Link"), `rel="next"`) || !strings.Contains(r1.Header.Get("Link"), `rel="last"`) {
		t.Fatalf("page1 Link missing next/last: %q", r1.Header.Get("Link"))
	}
	var page1 []map[string]any
	_ = json.NewDecoder(r1.Body).Decode(&page1)
	_ = r1.Body.Close()
	if len(page1) != 2 {
		t.Fatalf("page1 want 2 repos, got %d", len(page1))
	}

	r3 := get(t, s.URL+"/user/repos?per_page=2&page=3", "")
	link := r3.Header.Get("Link")
	if !strings.Contains(link, `rel="prev"`) || strings.Contains(link, `rel="next"`) {
		t.Fatalf("last page Link wrong: %q", link)
	}
	_ = r3.Body.Close()
}

func TestRESTFloorClamps(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddIssue("o", "r", 1, "x", "open")
	s.SetRESTRate(2, time.Now().Add(time.Hour))
	s.SetRESTFloor(1)

	url := s.URL + "/repos/o/r/issues/1"
	for i := 0; i < 5; i++ {
		r := get(t, url, "")
		_ = r.Body.Close()
	}
	r := get(t, url, "")
	if remaining(r) != "1" {
		t.Fatalf("floor not held, remaining=%s", remaining(r))
	}
	_ = r.Body.Close()
}

func TestGraphQLRateLimit(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddIssue("o", "r", 5, "hi", "open")
	s.AddIssue("o", "r", 6, "closed one", "closed")
	s.SetGraphQLRate(4000, time.Now().Add(30*time.Minute), 3)

	body := `{"operationName":"openIssues","variables":{"owner":"o","repo":"r"},"query":"query openIssues($owner:String!,$repo:String!){ repository(owner:$owner,name:$repo){ issues { nodes { number } } } rateLimit{remaining resetAt cost} }"}`
	resp, err := http.Post(s.URL+"/graphql", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data struct {
			RateLimit struct {
				Remaining int    `json:"remaining"`
				ResetAt   string `json:"resetAt"`
				Cost      int    `json:"cost"`
			} `json:"rateLimit"`
			Repository struct {
				Issues struct {
					Nodes []map[string]any `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()

	if out.Data.RateLimit.Cost != 3 || out.Data.RateLimit.Remaining != 3997 {
		t.Fatalf("rateLimit wrong: %+v", out.Data.RateLimit)
	}
	if out.Data.RateLimit.ResetAt == "" {
		t.Fatal("resetAt empty")
	}
	if len(out.Data.Repository.Issues.Nodes) != 1 {
		t.Fatalf("openIssues should return 1 open issue, got %d", len(out.Data.Repository.Issues.Nodes))
	}
	if s.CountOp("openIssues") != 1 {
		t.Fatalf("op not logged")
	}
}

func TestNotifications304(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetPollInterval(45)

	r0 := get(t, s.URL+"/notifications", "")
	if r0.Header.Get("X-Poll-Interval") != "45" {
		t.Fatalf("poll interval header wrong: %q", r0.Header.Get("X-Poll-Interval"))
	}
	_ = r0.Body.Close()

	s.SetNotifications([]map[string]any{{"id": "t1"}})
	r1 := get(t, s.URL+"/notifications", "")
	lm := r1.Header.Get("Last-Modified")
	remAfter := remaining(r1)
	_ = r1.Body.Close()
	if lm == "" {
		t.Fatal("no Last-Modified")
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/notifications", nil)
	req.Header.Set("If-Modified-Since", lm)
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304, got %d", r2.StatusCode)
	}
	if remaining(r2) != remAfter {
		t.Fatalf("304 spent quota")
	}
	if r2.Header.Get("X-Poll-Interval") != "45" {
		t.Fatal("304 missing poll interval")
	}
	_ = r2.Body.Close()
}

// capture is a tiny sink for signed webhook POSTs.
type capture struct {
	mu   sync.Mutex
	reqs []capturedReq
}
type capturedReq struct {
	event, delivery, sig string
	body                 []byte
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, capturedReq{
			event:    r.Header.Get("X-GitHub-Event"),
			delivery: r.Header.Get("X-GitHub-Delivery"),
			sig:      r.Header.Get("X-Hub-Signature-256"),
			body:     b,
		})
		c.mu.Unlock()
		w.WriteHeader(200)
	}
}
func (c *capture) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.reqs) }

func TestSignedPOSTVerifies(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetWebhookSecret("topsecret")

	cap := &capture{}
	sink := newSink(t, cap)
	defer sink.Close()

	_, status, err := s.EmitWebhook(sink.URL, "issues", "opened", map[string]any{"number": 5})
	if err != nil || status != 200 {
		t.Fatalf("emit: status=%d err=%v", status, err)
	}
	if cap.count() != 1 {
		t.Fatalf("sink got %d reqs", cap.count())
	}
	got := cap.reqs[0]
	if got.event != "issues" {
		t.Fatalf("event header %q", got.event)
	}
	if got.sig != Sign("topsecret", got.body) {
		t.Fatal("signature does not verify against secret")
	}
	if got.sig == Sign("wrong", got.body) {
		t.Fatal("signature verifies against wrong secret")
	}
	var p map[string]any
	_ = json.Unmarshal(got.body, &p)
	if p["action"] != "opened" {
		t.Fatalf("action not set in payload: %v", p["action"])
	}
}

func TestHooksDeliveriesRedeliver(t *testing.T) {
	s := New()
	defer s.Close()

	cap := &capture{}
	sink := newSink(t, cap)
	defer sink.Close()

	// Create a hook via the REST API.
	hookBody := `{"name":"web","events":["issues"],"config":{"url":"` + sink.URL + `","secret":"hooksec"}}`
	cr, err := http.Post(s.URL+"/repos/o/r/hooks", "application/json", strings.NewReader(hookBody))
	if err != nil || cr.StatusCode != http.StatusCreated {
		t.Fatalf("create hook: status=%d err=%v", cr.StatusCode, err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(cr.Body).Decode(&created)
	_ = cr.Body.Close()
	if created.ID == 0 {
		t.Fatal("no hook id")
	}

	// A delivery that failed while the forwarder was down.
	delivID := s.QueueUndelivered(created.ID, "issues", "opened", map[string]any{"number": 9})

	// It shows up in the deliveries listing as pending.
	dl := get(t, s.URL+"/repos/o/r/hooks/"+itoa(created.ID)+"/deliveries", "")
	var listed []map[string]any
	_ = json.NewDecoder(dl.Body).Decode(&listed)
	_ = dl.Body.Close()
	if len(listed) != 1 || listed[0]["status"] != "pending" {
		t.Fatalf("deliveries listing wrong: %v", listed)
	}

	// Redeliver via /attempts -> the sink receives exactly one signed POST.
	att, err := http.Post(s.URL+"/repos/o/r/hooks/"+itoa(created.ID)+"/deliveries/"+itoa(delivID)+"/attempts", "application/json", nil)
	if err != nil || att.StatusCode != http.StatusAccepted {
		t.Fatalf("attempts: status=%d err=%v", att.StatusCode, err)
	}
	_ = att.Body.Close()

	if cap.count() != 1 {
		t.Fatalf("sink received %d, want 1", cap.count())
	}
	if cap.reqs[0].sig != Sign("hooksec", cap.reqs[0].body) {
		t.Fatal("redelivery not signed with hook secret")
	}

	// Now marked delivered (won't be re-offered).
	dl2 := get(t, s.URL+"/repos/o/r/hooks/"+itoa(created.ID)+"/deliveries", "")
	var listed2 []map[string]any
	_ = json.NewDecoder(dl2.Body).Decode(&listed2)
	_ = dl2.Body.Close()
	if listed2[0]["status"] != "OK" {
		t.Fatalf("delivery not marked delivered: %v", listed2[0])
	}
}

func TestConcurrentGetsSafe(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddIssue("o", "r", 7, "x", "open")
	url := s.URL + "/repos/o/r/issues/7"

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := get(t, url, "")
			_ = r.Body.Close()
		}()
	}
	wg.Wait()
	if s.CountPath("GET", "/issues/7") != 20 {
		t.Fatalf("want 20 logged fetches, got %d", s.CountPath("GET", "/issues/7"))
	}
}

func TestBuildGhStubDelivers(t *testing.T) {
	s := New()
	defer s.Close()

	cap := &capture{}
	sink := newSink(t, cap)
	defer sink.Close()

	bin := BuildGhStub(t)
	dir := t.TempDir()
	deliv := `{"event":"issues","action":"opened","delivery_id":"d-123","payload":{"action":"opened","number":1}}`
	if err := writeFile(filepath.Join(dir, "001.json"), deliv); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-R", "o/r", "-E", "issues", "-U", sink.URL, "-S", "stubsec")
	cmd.Env = append([]string{"GHSTUB_DELIVERIES_DIR=" + dir}, "PATH="+pathEnv())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()

	// First-delivery deadline is generous: under `go test ./...` this package's
	// process-start-and-deliver window competes with other packages' concurrent
	// `go build`/test compilation for CPU, so a tight deadline flakes under load
	// even though the stub itself starts almost instantly in isolation.
	waitFor(t, 15*time.Second, func() bool { return cap.count() >= 1 })
	got := cap.reqs[0]
	if got.event != "issues" || got.delivery != "d-123" {
		t.Fatalf("stub headers wrong: %+v", got)
	}
	if !hmac.Equal([]byte(got.sig), []byte(Sign("stubsec", got.body))) {
		t.Fatal("stub signature does not verify")
	}
}
