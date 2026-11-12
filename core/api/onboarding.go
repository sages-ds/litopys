package api

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Onboarding channels of the native API. Model-facing texts are English.
// instructions reach ChatGPT, Codex and Claude Code (Claude Code cuts at 2 KB, OpenAI clients weigh the
// first 512 characters most); claude.ai and Claude Desktop rely on the tool descriptions, the resource
// and the prompt instead.

// InstructionsCore and Instructions come from the guide (guide.go): one source with the rules block.

const capabilitiesURI = "litopys://capabilities"

// onboarding adds the capabilities resource and the start-work prompt to a server.
func (a *API) onboarding(s *mcp.Server) {
	s.AddResource(&mcp.Resource{URI: capabilitiesURI, Name: "capabilities", Title: "Litopys capabilities",
		Description: "What this Litopys server offers and what this connection may do: API version, profile, tools, destructive tools.",
		MIMEType:    "application/json"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			var c Caller
			if req.Extra != nil {
				var err error
				if _, c, err = a.Resolve(ctx, req.Extra.TokenInfo, req.Extra.Header); err != nil {
					return nil, err
				}
			}
			v, err := a.capabilities(&c)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(v)
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: capabilitiesURI, MIMEType: "application/json", Text: string(b)}}}, nil
		})
	s.AddPrompt(&mcp.Prompt{Name: "start-work", Title: "Litopys: start or resume work",
		Description: "Start or resume work on a project with its saved state.",
		Arguments:   []*mcp.PromptArgument{{Name: "project", Title: "Project", Description: "Project name or title; empty = choose from the list"}}},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			text := "Call the Litopys tool project_resume without arguments, show me my projects briefly and ask which one we work on."
			if req.Params != nil && req.Params.Arguments["project"] != "" {
				text = "Call the Litopys tool project_resume with project \"" + req.Params.Arguments["project"] +
					"\", then tell me in a few lines where the work stopped and what the next steps are."
			}
			return &mcp.GetPromptResult{Description: "Start or resume work with Litopys",
				Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
		})
}
