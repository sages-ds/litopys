package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"litopys.smart-gw.co.ua/core/api"
)

// The model conversation of one step over the OpenAI-compatible chat completions API.

type message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Tools       []toolDef `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
}

// result is what finish_step carries.
type result struct {
	State  string          `json:"state"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// FinishTool is the name of the runner's own tool that ends a step.
const FinishTool = "finish_step"

// runnerOwned: verbs the runner calls itself; a model never gets them as tools.
var runnerOwned = []string{"step.start", "step.checkpoint", "step.finish", "step.pack", "step.delegate", "execution.read"}

const systemPrompt = `You are an executor of one step of a workflow run in Litopys, a memory of projects.
The user message is the step package: the step definition (title, acceptance, output schema), the run parameters,
the outputs of finished steps and the context the server gathered. Do the step and nothing else.
Use only the tools you are given; they act with a grant limited to this step, its project and its data class —
if something is not visible to you, it is not meant for this step: do not try to work around it.
When done, call finish_step once: state "completed" with the output (it must match the step's output schema when
there is one), "rejected" when the rules forbid the step (say why in error), or "failed" when you cannot do it.`

// tools of a step: the surface tools of its verbs (minus the runner's own) and finish_step.
func stepTools(step stepDef) (map[string]api.Tool, []toolDef, error) {
	doc, err := api.Surface()
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]api.Tool{}
	var defs []toolDef
	for _, t := range doc.Tools {
		if !slices.Contains(step.Verbs, t.Verb) || slices.Contains(runnerOwned, t.Verb) {
			continue
		}
		if step.WriteMode == "candidate" && t.Kind == "command" { // refused anyway (candidate_only): not offered
			continue
		}
		var d toolDef
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description, d.Function.Parameters = t.ForExecutor() // the executor variant
		defs = append(defs, d)
		byName[t.Name] = t
	}
	output := step.Output
	if len(output) == 0 {
		output = json.RawMessage(`{"type":"object"}`)
	}
	var fin toolDef
	fin.Type = "function"
	fin.Function.Name = FinishTool
	fin.Function.Description = "End this step: its state, the output (for completed) or the reason (for rejected / failed)."
	fin.Function.Parameters = json.RawMessage(`{"type":"object","required":["state"],"properties":{` +
		`"state":{"type":"string","enum":["completed","rejected","failed"]},` +
		`"output":` + string(output) + `,` +
		`"error":{"type":"string","maxLength":2000}},"additionalProperties":false}`)
	defs = append(defs, fin)
	return byName, defs, nil
}

type stepDef struct {
	Key        string          `json:"key"`
	Title      string          `json:"title"`
	Verbs      []string        `json:"verbs"`
	WriteMode  string          `json:"write_mode"` // candidate: the step writes nothing but its output
	Output     json.RawMessage `json:"output"`
	Acceptance string          `json:"acceptance"`
}

// converse runs the model on the package until it calls finish_step or runs out of turns.
func (r *Runner) converse(ctx context.Context, e Executor, tok string, pack json.RawMessage, log *slog.Logger) (result, error) {
	var p struct {
		Step stepDef `json:"step"`
	}
	if err := json.Unmarshal(pack, &p); err != nil {
		return result{}, fmt.Errorf("pack: %w", err)
	}
	byName, defs, err := stepTools(p.Step)
	if err != nil {
		return result{}, err
	}
	sys, user := systemPrompt, string(pack)
	msgs := []message{{Role: "system", Content: &sys}, {Role: "user", Content: &user}}
	nudged := false
	for turn := 0; turn < e.MaxTurns; turn++ {
		reply, err := r.chat(ctx, e, chatRequest{Model: e.Model, Messages: msgs, Tools: defs, ToolChoice: "auto",
			MaxTokens: e.MaxTokens, Temperature: e.Temperature})
		if err != nil {
			return result{}, err
		}
		msgs = append(msgs, reply)
		if len(reply.ToolCalls) == 0 {
			if nudged {
				return result{}, errors.New("the model answered without calling finish_step")
			}
			nudged = true
			s := "Call finish_step now with the state and the output of this step."
			msgs = append(msgs, message{Role: "user", Content: &s})
			continue
		}
		for _, c := range reply.ToolCalls {
			if c.Function.Name == FinishTool {
				var res result
				if err := json.Unmarshal([]byte(c.Function.Arguments), &res); err != nil {
					return result{}, fmt.Errorf("finish_step arguments: %w", err)
				}
				if !slices.Contains([]string{"completed", "rejected", "failed"}, res.State) {
					return result{}, fmt.Errorf("finish_step state %q", res.State)
				}
				return res, nil
			}
			content := r.useTool(ctx, tok, byName, c, log)
			msgs = append(msgs, message{Role: "tool", ToolCallID: c.ID, Content: &content})
		}
	}
	return result{}, fmt.Errorf("no finish_step in %d turns", e.MaxTurns)
}

// useTool calls one Litopys tool with the derived token; errors go back to the model as data.
func (r *Runner) useTool(ctx context.Context, tok string, byName map[string]api.Tool, c toolCall, log *slog.Logger) string {
	t, ok := byName[c.Function.Name]
	if !ok {
		return `{"error":{"code":"unknown_tool","message":"not a tool of this step"}}`
	}
	var args json.RawMessage = json.RawMessage(c.Function.Arguments)
	if !json.Valid(args) || len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := r.post(ctx, tok, t.Verb, args)
	log.Info("tool", "name", t.Name, "ok", err == nil)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": ae.Code, "message": ae.Message}})
			return string(b)
		}
		return `{"error":{"code":"transport","message":"the call did not reach Litopys"}}`
	}
	return truncate(string(raw), 16000)
}

// chat is one chat completions request to the executor's endpoint.
func (r *Runner) chat(ctx context.Context, e Executor, in chatRequest) (message, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Key != "" { // opened by the server from the key the person entered
		req.Header.Set("Authorization", "Bearer "+e.Key)
	} else if e.APIKeyEnv != "" {
		key := os.Getenv(e.APIKeyEnv)
		if key == "" {
			return message{}, fmt.Errorf("%s is not set", e.APIKeyEnv)
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return message{}, fmt.Errorf("model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return message{}, err
	}
	if resp.StatusCode/100 != 2 {
		return message{}, fmt.Errorf("model: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return message{}, fmt.Errorf("model: %w", err)
	}
	if len(out.Choices) == 0 {
		return message{}, errors.New("model: no choices")
	}
	m := out.Choices[0].Message
	m.Role = "assistant"
	return m, nil
}
