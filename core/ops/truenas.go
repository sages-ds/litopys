package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Target is the adapter of an installation's platform: only the reads status and doctor need.
type Target interface {
	Apps(ctx context.Context, names []string) (map[string]App, error)
	// Images maps every local image tag (repo:tag) to its registry digests (repo@sha256:…).
	Images(ctx context.Context) (map[string][]string, error)
	Pool(ctx context.Context, name string) (Pool, error)
}

// App is one service of the installation as the platform reports it.
type App struct {
	Name       string      `json:"name"`
	State      string      `json:"state"` // RUNNING, STOPPED, DEPLOYING, CRASHED… (TrueNAS app states)
	Containers []Container `json:"containers,omitempty"`
}

// Container is one compose service of an app.
type Container struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"` // running, exited…
}

// Pool is the free space of a storage pool.
type Pool struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Free int64  `json:"free"`
}

// Caller runs one TrueNAS middleware method and returns its JSON result.
type Caller interface {
	Call(ctx context.Context, method string, args ...any) (json.RawMessage, error)
}

// TrueNAS is the truenas adapter over the middleware API.
type TrueNAS struct{ C Caller }

func (t TrueNAS) Apps(ctx context.Context, names []string) (map[string]App, error) {
	raw, err := t.C.Call(ctx, "app.query", []any{[]any{"name", "in", names}},
		map[string]any{"extra": map[string]any{"retrieve_config": false}})
	if err != nil {
		return nil, err
	}
	var got []struct {
		Name            string `json:"name"`
		State           string `json:"state"`
		ActiveWorkloads struct {
			ContainerDetails []struct {
				ServiceName string `json:"service_name"`
				Image       string `json:"image"`
				State       string `json:"state"`
			} `json:"container_details"`
		} `json:"active_workloads"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("app.query: %w", err)
	}
	out := map[string]App{}
	for _, a := range got {
		app := App{Name: a.Name, State: a.State}
		for _, c := range a.ActiveWorkloads.ContainerDetails {
			app.Containers = append(app.Containers, Container{Service: c.ServiceName, Image: c.Image, State: c.State})
		}
		out[a.Name] = app
	}
	return out, nil
}

func (t TrueNAS) Images(ctx context.Context) (map[string][]string, error) {
	raw, err := t.C.Call(ctx, "app.image.query", []any{}, map[string]any{"select": []string{"repo_tags", "repo_digests"}})
	if err != nil {
		return nil, err
	}
	var got []struct {
		RepoTags    []string `json:"repo_tags"`
		RepoDigests []string `json:"repo_digests"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("app.image.query: %w", err)
	}
	out := map[string][]string{}
	for _, img := range got {
		for _, tag := range img.RepoTags {
			out[tag] = append(out[tag], img.RepoDigests...)
		}
	}
	return out, nil
}

func (t TrueNAS) Pool(ctx context.Context, name string) (Pool, error) {
	raw, err := t.C.Call(ctx, "pool.query", []any{[]any{"name", "=", name}},
		map[string]any{"select": []string{"name", "size", "free"}})
	if err != nil {
		return Pool{}, err
	}
	var got []Pool
	if err := json.Unmarshal(raw, &got); err != nil {
		return Pool{}, fmt.Errorf("pool.query: %w", err)
	}
	if len(got) == 0 {
		return Pool{}, fmt.Errorf("pool %q not found", name)
	}
	return got[0], nil
}

// SSHMidclt calls `midclt call` on the TrueNAS host over ssh with a key (the login the deploy scripts
// use today). No secret travels: the key file stays where it is, the arguments are public filters.
// Later transport: the JSON-RPC API with an API key from TRUENAS_API_KEY — same Caller.
type SSHMidclt struct {
	Host string // user@host
	Key  string // private key path; "" — ssh's own defaults
}

func (s SSHMidclt) Call(ctx context.Context, method string, args ...any) (json.RawMessage, error) {
	return s.call(ctx, false, method, args...)
}

func (s SSHMidclt) call(ctx context.Context, job bool, method string, args ...any) (json.RawMessage, error) {
	remote := []string{"midclt", "call"}
	if job {
		remote = append(remote, "-j")
	}
	remote = append(remote, shellQuote(method))
	for _, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		remote = append(remote, shellQuote(string(b)))
	}
	argv := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if s.Key != "" {
		argv = append(argv, "-i", s.Key)
	}
	argv = append(argv, s.Host, strings.Join(remote, " "))
	cmd := exec.CommandContext(ctx, "ssh", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %v: %s", method, err, strings.TrimSpace(stderr.String()))
	}
	return json.RawMessage(bytes.TrimSpace(stdout.Bytes())), nil
}

// shellQuote makes s one word for the remote POSIX shell ssh hands the command to.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
