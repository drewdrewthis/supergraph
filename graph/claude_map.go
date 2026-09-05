package graph

// claudeSessionModel maps a claude plugin SessionRow to the generated GraphQL model.
// It lives outside claude.resolvers.go so gqlgen's follow-schema regeneration (which
// relocates non-resolver helpers out of a resolvers file) never rewrites it.

import (
	"github.com/drewdrewthis/supergraph/graph/model"
	"github.com/drewdrewthis/supergraph/plugins/claude"
)

// claudeSessionModel renders empty/zero optional fields as GraphQL null.
func claudeSessionModel(row claude.SessionRow) model.ClaudeSession {
	m := model.ClaudeSession{
		HostID: row.HostID, SessionID: row.SessionID, Cwd: row.Cwd, State: row.State,
		ToolCalls: row.ToolCalls, StartedAt: row.StartedAt, LastEventAt: row.LastEventAt,
		StaleSince: row.StaleSince,
	}
	if row.GitBranch != "" {
		m.GitBranch = &row.GitBranch
	}
	if row.IssueNumber != 0 {
		m.IssueNumber = &row.IssueNumber
	}
	if row.Model != "" {
		m.Model = &row.Model
	}
	if row.LastTool != "" {
		m.LastTool = &row.LastTool
	}
	if row.PrNumber != 0 {
		m.PrNumber = &row.PrNumber
	}
	if row.PrURL != "" {
		m.PrURL = &row.PrURL
	}
	return m
}
