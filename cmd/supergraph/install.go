package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
)

// claudeHookEvents are the 7 Claude Code lifecycle events the claude plugin ingests
// (EDR docs/edr/claude.md §"Measured facts"). Install wires one `supergraph
// claude-hook` command per event.
var claudeHookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"Notification", "Stop", "SessionEnd",
}

// claudeHookCommand is the settings.json command string, deduped on. The forwarder
// self-configures from the box's config (listen + token), so the token never lands in
// settings.json — it stays in config only.
const claudeHookCommand = "supergraph claude-hook"

// installCmd is opt-in hook registration (owner decision C1). Bare `supergraph
// install` PRINTS the paste-ready hook block and touches nothing; `--install-hook`
// merges it idempotently into `--settings <path>`.
func installCmd() *cobra.Command {
	var doInstall bool
	var settingsPath string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Print (or, with --install-hook, merge) the Claude Code hook block",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !doInstall {
				return printHookBlock(cmd.OutOrStdout())
			}
			if settingsPath == "" {
				settingsPath = defaultClaudeSettingsPath()
			}
			return mergeHookBlock(cmd.ErrOrStderr(), settingsPath)
		},
	}
	cmd.Flags().BoolVar(&doInstall, "install-hook", false,
		"merge the hook block into the settings file (default: off, print only)")
	cmd.Flags().StringVar(&settingsPath, "settings", "",
		"Claude Code settings.json to merge into (default ~/.claude/settings.json)")
	return cmd
}

// hookBlock builds the {"hooks": {...}} object wiring every event to our command.
func hookBlock() map[string]any {
	hooks := map[string]any{}
	for _, ev := range claudeHookEvents {
		hooks[ev] = []any{hookEntry()}
	}
	return map[string]any{"hooks": hooks}
}

func hookEntry() map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": claudeHookCommand}}}
}

func printHookBlock(w io.Writer) error {
	b, _ := json.MarshalIndent(hookBlock(), "", "  ")
	out := "# Paste this into your ~/.claude/settings.json (hook install is opt-in).\n" +
		"# Or run: supergraph install --install-hook --settings ~/.claude/settings.json\n" +
		string(b) + "\n"
	_, err := io.WriteString(w, out)
	return err
}

// mergeHookBlock additively + idempotently merges our command into each event array
// of settingsPath, deduped by command string, preserving every existing hook
// (AC-CLAUDE-INSTALL-IDEMPOTENT). An existing settings file that does not parse is a
// hard stop — we NEVER overwrite a file we could not read, which would silently
// discard the user's own settings; we print a hint and write nothing.
func mergeHookBlock(hintW io.Writer, settingsPath string) error {
	settings := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil { //nolint:gosec // G304: settingsPath is an explicit user-provided --settings flag
		if err := json.Unmarshal(data, &settings); err != nil {
			_, _ = fmt.Fprintf(hintW, "refusing to overwrite %s: it is not valid JSON (%v).\n"+
				"Fix or remove it, or run `supergraph install` to print the block and paste it manually.\n",
				settingsPath, err)
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", settingsPath, err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range claudeHookEvents {
		arr, _ := hooks[ev].([]any)
		if !containsHookCommand(arr, claudeHookCommand) {
			arr = append(arr, hookEntry())
		}
		hooks[ev] = arr
	}
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(settingsPath, append(out, '\n'), 0o600)
}

// writeFileAtomic writes data to a temp file in the target dir then renames it over
// path, so a crash mid-write can never leave a truncated settings.json — the reader
// sees either the old file or the complete new one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// containsHookCommand reports whether any entry in a settings event array already
// wires cmd, so a re-run adds no duplicate.
func containsHookCommand(arr []any, cmd string) bool {
	for _, e := range arr {
		em, _ := e.(map[string]any)
		inner, _ := em["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if s, _ := hm["command"].(string); s == cmd {
				return true
			}
		}
	}
	return false
}

func defaultClaudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// claudeHookCmd is the thin stdin→POST forwarder registered in settings.json. It
// reads the hook JSON on stdin, adds TMUX_PANE from the environment (the only
// session→pane source, absent from the transcript), POSTs to the local hook route
// with the configured bearer token, and ALWAYS exits 0 — a nonzero PreToolUse hook
// would deny the tool call (measured constraint).
func claudeHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "claude-hook",
		Short:  "Forward a Claude Code lifecycle hook to the local claude plugin",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			forwardClaudeHook()
			return nil // always succeed: never block the tool call
		},
	}
}

func forwardClaudeHook() {
	in, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	var payload map[string]any
	if json.Unmarshal(in, &payload) != nil || payload == nil {
		payload = map[string]any{}
	}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		payload["tmux_pane"] = pane
	}
	payload["pid"] = os.Getppid()
	body, _ := json.Marshal(payload)

	url, token := hookEndpoint(configPath)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// hookEndpoint resolves the local hook URL and bearer token from config, falling back
// to the loopback default when config is absent or invalid.
func hookEndpoint(cfgPath string) (string, string) {
	listen := "127.0.0.1:7788"
	token := ""
	if cfg, err := core.LoadConfig(cfgPath); err == nil {
		if cfg.Listen != "" {
			listen = cfg.Listen
		}
		// Pick the token deterministically (first caller by sorted name) so every
		// forwarder on the box presents the same bearer instead of a random
		// map-iteration one.
		names := make([]string, 0, len(cfg.Tokens))
		for name := range cfg.Tokens {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			token = cfg.Tokens[names[0]]
		}
	}
	return "http://" + listen + "/plugins/claude/hook", token
}
