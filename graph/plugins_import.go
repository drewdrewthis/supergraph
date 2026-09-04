package graph

// Plugins are compiled into the binary by blank-importing their packages here.
// Each plugin self-registers via init() (core.Register) and contributes its
// GraphQL fields from plugins/<name>/schema/*.graphqls. Adding a plugin is one
// blank import below plus the plugin's own files — never an edit under core/ (S5).
// Step 12 adds the template plugin's import here.
import (
	_ "github.com/drewdrewthis/supergraph/plugins/template"
)
