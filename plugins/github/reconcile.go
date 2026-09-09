package github

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// runReconcile is the correctness floor (EDR §"Correctness paths"): once per
// interval it discovers repos, ensures a hook per repo, and runs a since-cursor pull
// that heals any dropped event. It waits one interval before the first run so cold
// start stays read-through only (no eager baseline; AC-GH-COLDSTART).
func (p *Plugin) runReconcile(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce discovers repos, ensures their hooks, and pulls each repo's open
// issues since its cursor (F2/F3). It advances the health cursor at the end.
func (p *Plugin) reconcileOnce(ctx context.Context) {
	if len(p.cfg.hookRepos) == 0 {
		p.hookSkipOnce.Do(func() {
			log.Printf("github: [plugins.github] hookRepos empty — creating no webhooks; set it to an owner/repo allowlist to opt in")
		})
	}
	for _, full := range p.discoverRepos(ctx) {
		owner, repo, ok := strings.Cut(full, "/")
		if !ok {
			continue
		}
		p.ensureHook(ctx, owner, repo)
		p.sinceReconcile(ctx, owner, repo)
	}
	p.revalidate(ctx)
	_ = p.store.pruneDeliveries(ctx, p.now().Add(-7*24*time.Hour)) // S3: bound the dedup table
	_ = p.store.setCursor(ctx, "reconcile:cursor", p.now().Format(time.RFC3339))
}

// revalidate re-checks every cached non-pinned node against upstream via a
// conditional GET, so staleness stays bounded by the reconcile interval for ALL
// mutable kinds, not just open issues (P1). A 304 costs zero body quota; a 200
// re-reads the canonical body and emits github.node.updated only when it changed.
// One INFO line per pass records the tally so a reconcile run is observable.
func (p *Plugin) revalidate(ctx context.Context) {
	nodes, _ := p.store.nonPinnedNodes(ctx)
	var checked, notModified, fetched, changed int
	for _, n := range nodes {
		checked++
		_, outcome, _ := p.fetchNode(ctx, n.Key, n)
		switch outcome {
		case outcomeNotModified:
			notModified++
		case outcomeUnchanged:
			fetched++
		case outcomeChanged:
			fetched++
			changed++
		}
	}
	log.Printf("github: reconcile checked=%d notModified=%d fetched=%d changed=%d", checked, notModified, fetched, changed)
}

// discoverRepos pages GET /user/repos?affiliation=owner (F3 zero-config discovery),
// stopping when a page returns fewer than per_page rows.
func (p *Plugin) discoverRepos(ctx context.Context) []string {
	const perPage = 100
	var repos []string
	for page := 1; ; page++ {
		path := "/user/repos?affiliation=owner&per_page=100&page=" + strconv.Itoa(page)
		status, body, _, err := p.httpGET(ctx, path, "")
		if err != nil || status != http.StatusOK {
			break
		}
		var out []struct {
			FullName string `json:"full_name"`
		}
		_ = json.Unmarshal(body, &out)
		for _, r := range out {
			if r.FullName != "" {
				repos = append(repos, r.FullName)
			}
		}
		if len(out) < perPage {
			break
		}
	}
	return repos
}

// ensureHook creates one webhook per repo (F3) unless one is already recorded,
// signing it with the plugin's webhook secret so redelivered events verify.
func (p *Plugin) ensureHook(ctx context.Context, owner, repo string) {
	if _, ok := p.store.hook(ctx, owner, repo); ok {
		return
	}
	if !p.hookAllowed(owner, repo) {
		return
	}
	req := map[string]any{
		"name":   "web",
		"events": []string{"*"},
		"config": map[string]any{
			"url":          p.cfg.selfURL + "/plugins/github/webhook",
			"secret":       p.cfg.webhookSecret,
			"content_type": "json",
		},
	}
	status, body, err := p.httpPOST(ctx, "/repos/"+owner+"/"+repo+"/hooks", req)
	if err != nil || status != http.StatusCreated {
		return
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	_ = p.store.putHook(ctx, owner, repo, out.ID)
	// Identifiable marker for hooks we create (deletion on shutdown is not required;
	// this log line + the config.url path make ours findable on the repo).
	log.Printf("github: created \"web\" webhook id=%d on %s/%s → %s/plugins/github/webhook", out.ID, owner, repo, p.cfg.selfURL)
}

// hookAllowed reports whether owner/repo is in the hookRepos allowlist. An empty
// allowlist matches nothing, so no hooks are created (owner default).
func (p *Plugin) hookAllowed(owner, repo string) bool {
	full := owner + "/" + repo
	for _, r := range p.cfg.hookRepos {
		if r == full {
			return true
		}
	}
	return false
}

// sinceReconcile pulls a repo's open issues via the openIssues op, sending
// since=<T-60s> (a 60s overlap absorbs clock skew), stores each node, and advances
// the repo's since cursor (AC-GH-CURSOR / AC-GH-STALE / F2).
func (p *Plugin) sinceReconcile(ctx context.Context, owner, repo string) {
	op, ok := p.ops["openIssues"]
	if !ok {
		return
	}
	cur := "since:" + owner + "/" + repo
	endpoint := p.cfg.graphqlURL
	if v := p.store.cursor(ctx, cur); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			endpoint += "?since=" + url.QueryEscape(t.Add(-60*time.Second).UTC().Format(time.RFC3339))
		}
	}
	vars := map[string]any{"owner": owner, "repo": repo}
	data, err := p.graphqlAt(ctx, endpoint, op.query, vars)
	if err != nil {
		return
	}
	for _, kt := range op.keys {
		p.extractAndStore(ctx, kt, vars, data)
	}
	_ = p.store.setCursor(ctx, cur, p.now().Format(time.RFC3339))
}
