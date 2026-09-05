package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// client.go is the plugin's shared GitHub HTTP + rate-limit layer: every REST and
// GraphQL call goes through here, so authorization, rate-limit logging (AC-GH-
// RATELOG), and the near-floor pause (AC-GH-FLOOR) live in exactly one place. proxy,
// executor, reconcile and ingest all call these methods.

// floorThresholdGQL is the GraphQL-points remaining at or below which a read pauses
// to resetAt rather than risking a hard rate-limit failure. The REST path has no
// such pause: httpGET/httpPOST only log the x-ratelimit-* headers (logRESTRate) and
// never block, because REST calls are cheap and reconcile is the only bulk caller.
const floorThresholdGQL = 10

// httpGET issues a conditional GET against the REST base, logs the rate-limit
// headers, and returns status, body and the response ETag.
func (p *Plugin) httpGET(ctx context.Context, path, etag string) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.baseURL+path, nil)
	if err != nil {
		return 0, nil, "", err
	}
	p.authorize(req)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	p.logRESTRate(resp.Header)
	return resp.StatusCode, body, resp.Header.Get("ETag"), nil
}

// httpGraphQL runs a GraphQL document against the configured endpoint.
func (p *Plugin) httpGraphQL(ctx context.Context, query string, vars map[string]any) (map[string]any, error) {
	return p.graphqlAt(ctx, p.cfg.graphqlURL, query, vars)
}

// graphqlAt runs a GraphQL document against an explicit URL (reconcile appends a
// ?since= cursor), logs and floor-pauses on the rateLimit block, and returns the
// decoded data object.
func (p *Plugin) graphqlAt(ctx context.Context, url, query string, vars map[string]any) (map[string]any, error) {
	reqBody, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	p.logGraphQLRate(ctx, out.Data)
	return out.Data, nil
}

// httpPOST issues a JSON POST (hook creation, redelivery attempts) and returns the
// status and body.
func (p *Plugin) httpPOST(ctx context.Context, path string, body any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	p.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	p.logRESTRate(resp.Header)
	return resp.StatusCode, rb, nil
}

func (p *Plugin) authorize(req *http.Request) {
	if p.cfg.token != "" {
		req.Header.Set("Authorization", "token "+p.cfg.token)
	}
}

// logRESTRate logs the x-ratelimit-* headers (AC-GH-RATELOG).
func (p *Plugin) logRESTRate(h http.Header) {
	rem := h.Get("X-RateLimit-Remaining")
	if rem == "" {
		return
	}
	log.Printf("github: rate rest limit=%s remaining=%s reset=%s",
		h.Get("X-RateLimit-Limit"), rem, h.Get("X-RateLimit-Reset"))
}

// logGraphQLRate logs rateLimit{remaining,resetAt} and pauses to resetAt near the
// points floor.
func (p *Plugin) logGraphQLRate(ctx context.Context, data map[string]any) {
	rl, ok := data["rateLimit"].(map[string]any)
	if !ok {
		return
	}
	rem := toInt(rl["remaining"])
	resetAt, _ := time.Parse(time.RFC3339, fmt.Sprint(rl["resetAt"]))
	log.Printf("github: rate graphql remaining=%d resetAt=%s", rem, resetAt.Format(time.RFC3339))
	p.floorPause(ctx, rem, resetAt, floorThresholdGQL)
}

// floorPause blocks until reset when remaining is at or below threshold, so the
// plugin backs off instead of hard-failing (AC-GH-FLOOR). The injected sleeper lets
// tests drive it without real time.
func (p *Plugin) floorPause(ctx context.Context, remaining int, reset time.Time, threshold int) {
	if remaining > threshold {
		return
	}
	d := reset.Sub(p.now())
	if d <= 0 {
		return
	}
	log.Printf("github: floor pause %s until %s", d, reset.Format(time.RFC3339))
	p.sleep(ctx, d)
}
