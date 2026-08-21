package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// The progress token on notifications/progress must echo the client's
// _meta.progressToken — clients drop progress carrying an unknown token,
// which starved Claude Code's 300s idle timer on long-running calls.
func TestProgressTokenFromRequest(t *testing.T) {
	cases := []struct {
		name string
		meta *mcp.Meta
		want any
	}{
		{"client string token", &mcp.Meta{ProgressToken: "tok-abc"}, "tok-abc"},
		{"client numeric token", &mcp.Meta{ProgressToken: float64(7)}, float64(7)},
		{"meta without token falls back", &mcp.Meta{}, "call-id"},
		{"no meta falls back", nil, "call-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Meta = tc.meta
			if got := progressTokenFromRequest(req, "call-id"); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
