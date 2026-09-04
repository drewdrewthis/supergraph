package github

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"time"
)

const (
	backoffInitial = 500 * time.Millisecond
	backoffMax     = 30 * time.Second
)

// runIngest starts the push-ingest paths: the supervised `gh webhook forward` child
// (forward ingress) and, behind the flag, the /notifications poll. Correctness never
// depends on these — reconcile and redelivery heal any gap (EDR §"Correctness
// paths").
func (p *Plugin) runIngest(ctx context.Context) {
	if p.cfg.notifications {
		go p.notificationsPoll(ctx)
	}
	if p.cfg.ingress == "forward" {
		p.forwardSupervisor(ctx)
	}
}

// forwardSupervisor runs `gh webhook forward` and restarts it with exponential
// backoff, running redelivery from the last-seen delivery id on every (re)start so a
// delivery missed while it was down is replayed (AC-GH-FORWARD).
func (p *Plugin) forwardSupervisor(ctx context.Context) {
	backoff := backoffInitial
	for ctx.Err() == nil {
		p.redeliver(ctx)

		if p.cfg.ghPath == "" {
			return
		}
		// ghPath is an operator-configured binary (tests point it at the stub), not
		// attacker input — the same trust boundary as the operator's config file.
		cmd := exec.CommandContext(ctx, p.cfg.ghPath, "webhook", "forward", //nolint:gosec // operator-configured gh binary path, not attacker input
			"--url", p.cfg.selfURL+"/plugins/github/webhook")
		if err := cmd.Run(); err != nil {
			log.Printf("github: gh webhook forward exited: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		p.sleep(ctx, backoff)
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// redeliver replays undelivered deliveries for every created hook via
// POST .../deliveries/{id}/attempts, advancing the per-repo lastDeliveryId cursor.
// The webhook handler dedups by X-GitHub-Delivery, so a replay lands exactly once.
func (p *Plugin) redeliver(ctx context.Context) {
	hooks, err := p.store.hooks(ctx)
	if err != nil {
		return
	}
	for _, h := range hooks {
		owner, repo, hookID := h[0], h[1], h[2]
		cur := "hook:" + owner + "/" + repo + ":lastDeliveryId"
		last, _ := strconv.ParseInt(p.store.cursor(ctx, cur), 10, 64)

		status, body, _, err := p.httpGET(ctx, "/repos/"+owner+"/"+repo+"/hooks/"+hookID+"/deliveries", "")
		if err != nil || status != http.StatusOK {
			continue
		}
		var deliveries []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &deliveries)

		maxID := last
		for _, d := range deliveries {
			if d.ID <= last {
				continue
			}
			if d.Status != "OK" {
				_, _, _ = p.httpPOST(ctx,
					"/repos/"+owner+"/"+repo+"/hooks/"+hookID+"/deliveries/"+strconv.FormatInt(d.ID, 10)+"/attempts", nil)
			}
			if d.ID > maxID {
				maxID = d.ID
			}
		}
		if maxID > last {
			_ = p.store.setCursor(ctx, cur, strconv.FormatInt(maxID, 10))
		}
	}
}

// notificationsPoll runs only when notifications=true. It conditionally GETs
// /notifications with If-Modified-Since; a 304 costs zero quota, a 200 advances the
// cursor. It honours X-Poll-Interval (AC-GH-NOTIFY-304).
func (p *Plugin) notificationsPoll(ctx context.Context) {
	for ctx.Err() == nil {
		poll := p.pollOnce(ctx)
		p.sleep(ctx, time.Duration(poll)*time.Second)
	}
}

// pollOnce performs one conditional notifications poll and returns the server's
// advertised poll interval in seconds.
func (p *Plugin) pollOnce(ctx context.Context) int {
	poll := 60
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.baseURL+"/notifications", nil)
	if err != nil {
		return poll
	}
	p.authorize(req)
	if ims := p.store.cursor(ctx, "notif:lastModified"); ims != "" {
		req.Header.Set("If-Modified-Since", ims)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return poll
	}
	defer func() { _ = resp.Body.Close() }()
	if v := resp.Header.Get("X-Poll-Interval"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			poll = n
		}
	}
	if resp.StatusCode == http.StatusNotModified {
		log.Printf("github: notifications 304, zero quota")
		return poll
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		_ = p.store.setCursor(ctx, "notif:lastModified", lm)
	}
	log.Printf("github: notifications 200, cursor advanced")
	return poll
}
