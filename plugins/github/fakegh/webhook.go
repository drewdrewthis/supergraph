package fakegh

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Hook is a repo webhook registered via POST /repos/{o}/{r}/hooks. Each carries
// its own delivery URL and HMAC secret.
type Hook struct {
	ID     int64
	Owner  string
	Repo   string
	URL    string
	Secret string
	Events []string
}

// Delivery is one recorded webhook delivery for a hook. Delivered is false for a
// delivery introduced as failed/undelivered (the redelivery scenario); a POST to
// its /attempts flips it true.
type Delivery struct {
	ID         int64
	GUID       string
	HookID     int64
	Event      string
	Action     string
	Payload    []byte
	Delivered  bool
	StatusCode int
}

// Sign returns the GitHub X-Hub-Signature-256 value ("sha256="+hex HMAC-SHA256)
// for body under secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SignedPOST delivers a signed webhook to targetURL using the server webhook
// secret and returns the delivery GUID and HTTP status. payload is the raw event
// body the signature covers.
func (s *Server) SignedPOST(targetURL, event string, payload map[string]any) (string, int, error) {
	return s.signedPOST(targetURL, s.WebhookSecret(), event, newGUID(), payload)
}

// EmitWebhook sets payload["action"]=action and delivers it via SignedPOST. This
// is the direct-push path the S3/F1/HMAC scenarios drive.
func (s *Server) EmitWebhook(targetURL, event, action string, payload map[string]any) (string, int, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	if action != "" {
		payload["action"] = action
	}
	return s.SignedPOST(targetURL, event, payload)
}

func (s *Server) signedPOST(targetURL, secret, event, guid string, payload map[string]any) (string, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body)) //nolint:gosec // test fake POSTs to a caller-provided local handler URL
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", guid)
	req.Header.Set("X-Hub-Signature-256", Sign(secret, body))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return guid, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return guid, resp.StatusCode, nil
}

// QueueUndelivered records a delivery for hookID that was NOT sent to the hook URL
// (StatusCode 0), so a redelivery listing exposes it. It returns the delivery id.
func (s *Server) QueueUndelivered(hookID int64, event, action string, payload map[string]any) int64 {
	body, _ := json.Marshal(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDeliveryID++
	d := &Delivery{
		ID:      s.nextDeliveryID,
		GUID:    newGUID(),
		HookID:  hookID,
		Event:   event,
		Action:  action,
		Payload: body,
	}
	s.deliveries = append(s.deliveries, d)
	return d.ID
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		Events []string `json:"events"`
		Config struct {
			URL    string `json:"url"`
			Secret string `json:"secret"`
		} `json:"config"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	s.mu.Lock()
	s.nextHookID++
	h := &Hook{
		ID:     s.nextHookID,
		Owner:  r.PathValue("owner"),
		Repo:   r.PathValue("repo"),
		URL:    in.Config.URL,
		Secret: in.Config.Secret,
		Events: in.Events,
	}
	s.hooks = append(s.hooks, h)
	s.decREST()
	s.setRESTHeaders(w)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(hookJSON(h))
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	s.mu.Lock()
	out := []map[string]any{}
	for _, h := range s.hooks {
		if h.Owner == owner && h.Repo == repo {
			out = append(out, hookJSON(h))
		}
	}
	s.decREST()
	s.setRESTHeaders(w)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	hookID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	out := []map[string]any{}
	for _, d := range s.deliveries {
		if d.HookID == hookID {
			out = append(out, deliveryJSON(d))
		}
	}
	s.decREST()
	s.setRESTHeaders(w)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleRedeliver re-POSTs a stored delivery to its hook URL (signed with the
// hook secret) and flips it Delivered, so it is replayed exactly once.
func (s *Server) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	hookID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	delivID, _ := strconv.ParseInt(r.PathValue("delivery"), 10, 64)

	s.mu.Lock()
	var hook *Hook
	for _, h := range s.hooks {
		if h.ID == hookID {
			hook = h
			break
		}
	}
	var deliv *Delivery
	for _, d := range s.deliveries {
		if d.ID == delivID && d.HookID == hookID {
			deliv = d
			break
		}
	}
	s.decREST()
	s.setRESTHeaders(w)
	s.mu.Unlock()

	if hook == nil || deliv == nil {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	var payload map[string]any
	_ = json.Unmarshal(deliv.Payload, &payload)
	_, status, err := s.signedPOST(hook.URL, hook.Secret, deliv.Event, deliv.GUID, payload)

	s.mu.Lock()
	deliv.Delivered = true
	deliv.StatusCode = status
	s.mu.Unlock()

	if err != nil {
		http.Error(w, `{"message":"delivery failed"}`, http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(deliveryJSON(deliv))
}

func hookJSON(h *Hook) map[string]any {
	return map[string]any{
		"id":     h.ID,
		"name":   "web",
		"active": true,
		"events": h.Events,
		"config": map[string]any{"url": h.URL, "content_type": "json"},
	}
}

func deliveryJSON(d *Delivery) map[string]any {
	status := "pending"
	if d.Delivered {
		status = "OK"
	}
	return map[string]any{
		"id":           d.ID,
		"guid":         d.GUID,
		"event":        d.Event,
		"action":       d.Action,
		"status_code":  d.StatusCode,
		"status":       status,
		"delivered_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// HookID returns the id of the hook created for owner/repo, or 0 if none exists
// yet — the test uses it to queue an undelivered delivery against the right hook.
func (s *Server) HookID(owner, repo string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		if h.Owner == owner && h.Repo == repo {
			return h.ID
		}
	}
	return 0
}
