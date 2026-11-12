package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is an error answer of the native API ({"error": {"code", "message"}}).
type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// call POSTs a verb of the native API (domain.verb -> /v1/domain/verb) with a bearer token; out may be nil.
func (r *Runner) call(ctx context.Context, token, verb string, in any, out any) error {
	raw, err := r.post(ctx, token, verb, in)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// post returns the raw answer of a verb, or an *APIError for a non-2xx status.
func (r *Runner) post(ctx context.Context, token, verb string, in any) (json.RawMessage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(r.Cfg.LitopysURL, "/") + "/v1/" + strings.Replace(verb, ".", "/", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Litopys-Client", "litopys-runner")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != nil {
			e.Error.Status = resp.StatusCode
			return nil, e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Code: "http", Message: truncate(string(raw), 300)}
	}
	return raw, nil
}

// runnerCall is the runner's own endpoint of the server (/v1/runner/<what>): the owner token plus the runner secret.
func (r *Runner) runnerCall(ctx context.Context, method, what string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.Cfg.LitopysURL, "/")+"/v1/runner/"+what, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("X-Litopys-Runner", r.Secret)
	req.Header.Set("X-Litopys-Client", "litopys-runner")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Code: "http", Message: truncate(string(raw), 300)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
