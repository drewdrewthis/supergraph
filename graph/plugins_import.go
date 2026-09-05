package graph

// Plugins are compiled into the binary by blank-importing their packages here.
// Each plugin self-registers via init() (core.Register) and contributes its
// GraphQL fields from plugins/<name>/schema/*.graphqls. Adding a plugin is one
// blank import below plus the plugin's own files — never an edit under core/ (S5).
// Harness-only plugins live in plugins_import_harness.go (build tag `harness`).
import (
	// github is the event-invalidated caching proxy plugin (S5, EDR docs/edr/github.md).
	_ "github.com/drewdrewthis/supergraph/plugins/github"
	// peer is the multi-box federation plugin (EDR docs/edr/peer.md).
	_ "github.com/drewdrewthis/supergraph/plugins/peer"
	// template is the reference plugin (PRD §6 template plugin).
	_ "github.com/drewdrewthis/supergraph/plugins/template"
<<<<<<< HEAD

	// claude ingests Claude Code session state (docs/edr/claude.md).
	_ "github.com/drewdrewthis/supergraph/plugins/claude"
=======
	// tmux is the local tmux-server read model (docs/edr/tmux.md).
	_ "github.com/drewdrewthis/supergraph/plugins/tmux"
>>>>>>> 5409128 (feat(tmux): control-mode + reconcile-poll pane plugin, real-tmux godog steps, 15 scenarios; 770 LOC (cap 800))
)
