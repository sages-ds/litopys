// Package runner pulls ready model steps of workflow runs and executes them with registered executors.
// It is a client of the native API only: it never touches the database.
//
// For each executor of its configuration the runner asks step.open_items (filtered by the executor's data class),
// delegates the step to the executor (step.delegate: a derived token bound to that step), and with that token takes
// the step (step.start), reads its package (step.pack), runs the model with the step's tools plus finish_step,
// renews the lease (step.checkpoint) and ends the attempt (step.finish). The model speaks the OpenAI-compatible
// chat completions API, so one adapter serves Ollama, OpenAI, Groq, Mistral and others — they differ in base_url.
//
// Secrets: the orchestrator token and the provider keys come from the environment (OpenBao renders them into the
// unit's environment); the configuration file names the variables, never the values, and nothing logs them.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Executor is one registered executor (an agent / service actor with a profile, 0024) the runner serves.
type Executor struct {
	ActorID     uuid.UUID `json:"actor_id"`
	Name        string    `json:"name,omitempty"` // label in logs
	BaseURL     string    `json:"base_url"`       // OpenAI-compatible endpoint, e.g. http://ollama:11434/v1
	Model       string    `json:"model"`
	APIKeyEnv   string    `json:"api_key_env,omitempty"` // the variable holding the provider key; empty for a local model
	Parallel    int       `json:"parallel,omitempty"`    // steps at once (default 1)
	MaxTurns    int       `json:"max_turns,omitempty"`   // model turns per step (default 12)
	MaxTokens   int       `json:"max_tokens,omitempty"`  // per model reply (default 4096)
	Temperature *float64  `json:"temperature,omitempty"`
	// Adapter: openai (default) — the chat/completions protocol; anthropic is not served yet
	Adapter string `json:"adapter,omitempty"`
	// Key: the provider key the server opened (/v1/runner/config) — never in a file
	Key string `json:"-"`
}

// defaults fills the optional fields and checks the required ones.
func (e *Executor) defaults() error {
	if e.ActorID == uuid.Nil || e.BaseURL == "" || e.Model == "" {
		return errors.New("actor_id, base_url and model are required")
	}
	if e.Adapter != "" && e.Adapter != "openai" {
		return fmt.Errorf("adapter %q is not served by this runner yet", e.Adapter)
	}
	if e.Name == "" {
		e.Name = e.Model
	}
	if e.Parallel <= 0 {
		e.Parallel = 1
	}
	if e.MaxTurns <= 0 {
		e.MaxTurns = 12
	}
	if e.MaxTokens <= 0 {
		e.MaxTokens = 4096
	}
	return nil
}

// envOfSecretRef: the variable a key named by secret_ref is in on the stand — RUNNER_KEY_<last path part>
// (litopys/executors/groq -> RUNNER_KEY_GROQ, as a deployment's secret store puts it).
func envOfSecretRef(ref string) string {
	part := ref[strings.LastIndexAny(ref, "/:")+1:]
	return "RUNNER_KEY_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(part))
}

// Config is the runner's configuration file (JSON, no secrets).
type Config struct {
	LitopysURL   string     `json:"litopys_url"`              // base of the native API, e.g. http://127.0.0.1:8798
	PollS        int        `json:"poll_s,omitempty"`         // queue poll interval (default 15)
	DelegateTTLS int        `json:"delegate_ttl_s,omitempty"` // lifetime of a derived token (default 3600)
	Executors    []Executor `json:"executors"`
	// ServerExecutors: executors come from the server (LITOPYS_RUNNER_SECRET needed); the file may list none
	ServerExecutors bool `json:"server_executors,omitempty"`
}

// LoadConfig reads and checks a configuration file.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.check()
}

func (c *Config) check() error {
	if c.LitopysURL == "" {
		return errors.New("litopys_url is required")
	}
	if len(c.Executors) == 0 && !c.ServerExecutors {
		return errors.New("no executors (list them, or server_executors: true)")
	}
	if c.PollS <= 0 {
		c.PollS = 15
	}
	if c.DelegateTTLS <= 0 {
		c.DelegateTTLS = 3600
	}
	for i := range c.Executors {
		if err := c.Executors[i].defaults(); err != nil {
			return fmt.Errorf("executors[%d]: %w", i, err)
		}
	}
	return nil
}

// Runner executes steps for the configured executors.
type Runner struct {
	Cfg   Config
	Token string // the orchestrator's token (profile core or full): it delegates
	HTTP  *http.Client
	Log   *slog.Logger
	// Secret is LITOPYS_RUNNER_SECRET: with it the runner takes its executors from the server (/v1/runner/config) and
	// reports its state (/v1/runner/heartbeat) for the status page. Name and Version label the report.
	Secret, Name, Version string

	mu     sync.Mutex
	status map[uuid.UUID]*execStatus
}

// execStatus is what the heartbeat reports about one executor.
type execStatus struct {
	Name     string    `json:"name"`
	ActorID  uuid.UUID `json:"actor_id"`
	PolledAt time.Time `json:"polled_at,omitzero"`
	Error    string    `json:"error,omitempty"`
	BackoffS int       `json:"backoff_s,omitempty"`
}

// New builds a runner; the token is the orchestrator's (from the environment).
func New(cfg Config, token string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{Cfg: cfg, Token: token, HTTP: &http.Client{Timeout: 5 * time.Minute}, Log: log, status: map[uuid.UUID]*execStatus{}}
}

// ConfigRefresh is how often the runner asks the server for its executors.
const ConfigRefresh = 5 * time.Minute

// Run polls the queue until ctx ends; each executor has its own loop and at most Parallel steps at once. With the
// runner secret the executors come from the server (and change while it runs); without it, from the file.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	running := map[uuid.UUID]running{}
	start := func(e Executor) {
		lctx, cancel := context.WithCancel(ctx)
		running[e.ActorID] = runningOf(e, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(lctx, e)
		}()
	}
	fromFile := map[uuid.UUID]bool{}
	for _, e := range r.Cfg.Executors {
		fromFile[e.ActorID] = true
		start(e)
	}
	if r.Secret != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.heartbeats(ctx)
		}()
		for {
			if list, err := r.ServerExecutors(ctx); err != nil {
				r.Log.Warn("runner config", "err", err)
			} else {
				stop, add := reconcile(running, fromFile, list)
				for _, id := range stop {
					running[id].cancel() // gone, or its address, model or key changed
					delete(running, id)
					r.forget(id)
				}
				for _, e := range add {
					r.Log.Info("executor started", "executor", e.Name, "model", e.Model)
					start(e)
				}
			}
			t := time.NewTimer(ConfigRefresh)
			select {
			case <-ctx.Done():
				t.Stop()
				wg.Wait()
				return nil
			case <-t.C:
			}
		}
	}
	wg.Wait()
	return nil
}

// reconcile: which running executors to stop and which of the server's list to start. The file's executors are the
// file's — the server neither stops nor doubles them (a runner with both lost its file executor, 2026-11-01).
func reconcile(running map[uuid.UUID]running, fromFile map[uuid.UUID]bool, list []Executor) (stop []uuid.UUID, add []Executor) {
	want := map[uuid.UUID]Executor{}
	for _, e := range list {
		if !fromFile[e.ActorID] {
			want[e.ActorID] = e
		}
	}
	for id, x := range running {
		if fromFile[id] {
			continue
		}
		if e, ok := want[id]; !ok || runningOf(e, nil).sig != x.sig {
			stop = append(stop, id)
		}
	}
	for id, e := range want {
		if x, ok := running[id]; !ok || slices.Contains(stop, id) || runningOf(e, nil).sig != x.sig {
			add = append(add, e)
		}
	}
	return stop, add
}

// running is one executor's loop and what it was started with.
type running struct {
	sig    string
	cancel context.CancelFunc
}

func runningOf(e Executor, cancel context.CancelFunc) running {
	return running{sig: e.BaseURL + "|" + e.Model + "|" + e.Adapter + "|" + e.Key, cancel: cancel}
}

// loop polls one executor's queue until ctx ends.
func (r *Runner) loop(ctx context.Context, e Executor) {
	base := time.Duration(r.Cfg.PollS) * time.Second
	fails, last := 0, ""
	for {
		_, err := r.Once(ctx, e)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// a failing poll backs off and is logged when it starts or changes, not every tick: a removed
			// executor answered invalid_input 228 times an hour (roast r1)
			fails++
			if msg := err.Error(); msg != last {
				r.Log.Warn("poll failed", "executor", e.Name, "err", err, "next_in", pollDelay(base, fails))
				last = msg
			}
		case fails > 0:
			r.Log.Info("poll recovered", "executor", e.Name, "after_failures", fails)
			fails, last = 0, ""
		}
		r.note(e, err, pollDelay(base, fails))
		t := time.NewTimer(pollDelay(base, fails))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (r *Runner) note(e Executor, err error, next time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &execStatus{Name: e.Name, ActorID: e.ActorID, PolledAt: time.Now().UTC()}
	if err != nil {
		st.Error, st.BackoffS = truncate(err.Error(), 300), int(next.Seconds())
	}
	r.status[e.ActorID] = st
}

func (r *Runner) forget(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.status, id)
}

// ServerExecutors asks the server for the owner's live executors (keys opened there).
func (r *Runner) ServerExecutors(ctx context.Context) ([]Executor, error) {
	var out struct {
		Executors []struct {
			ActorID   uuid.UUID      `json:"actor_id"`
			Name      string         `json:"name"`
			Adapter   string         `json:"adapter"`
			BaseURL   string         `json:"base_url"`
			Model     string         `json:"model"`
			Key       string         `json:"key"`
			SecretRef string         `json:"secret_ref"`
			Limits    map[string]int `json:"limits"`
		} `json:"executors"`
	}
	if err := r.runnerCall(ctx, http.MethodGet, "config", nil, &out); err != nil {
		return nil, err
	}
	list := make([]Executor, 0, len(out.Executors))
	for _, x := range out.Executors {
		e := Executor{ActorID: x.ActorID, Name: x.Name, Adapter: x.Adapter, BaseURL: x.BaseURL, Model: x.Model, Key: x.Key}
		if e.Key == "" && x.SecretRef != "" {
			e.APIKeyEnv = envOfSecretRef(x.SecretRef) // the stand: a key from OpenBao in the runner's environment
		}
		if err := e.defaults(); err != nil {
			r.Log.Warn("executor skipped", "executor", x.Name, "err", err)
			continue
		}
		list = append(list, e)
	}
	return list, nil
}

// heartbeats reports the executors' state every minute.
func (r *Runner) heartbeats(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		r.mu.Lock()
		list := make([]*execStatus, 0, len(r.status))
		for _, st := range r.status {
			list = append(list, st)
		}
		r.mu.Unlock()
		if err := r.runnerCall(ctx, http.MethodPost, "heartbeat", map[string]any{"runner": r.Name, "version": r.Version, "executors": list}, nil); err != nil && ctx.Err() == nil {
			r.Log.Warn("heartbeat", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// MaxPollBackoff caps the wait between polls of a failing executor.
const MaxPollBackoff = 15 * time.Minute

// pollDelay is the wait before the next poll: the poll interval, doubled with each consecutive failure up to
// MaxPollBackoff.
func pollDelay(base time.Duration, fails int) time.Duration {
	d := base
	for i := 0; i < fails && d < MaxPollBackoff; i++ {
		d *= 2
	}
	return min(d, MaxPollBackoff)
}

// Once takes the executor's ready model steps (at most Parallel, concurrently) and runs them to the end; it returns
// how many attempts it finished.
func (r *Runner) Once(ctx context.Context, e Executor) (int, error) {
	var open struct {
		Items []struct {
			ExecutionID uuid.UUID `json:"execution_id"`
			StepKey     string    `json:"step_key"`
			Kind        string    `json:"kind"`
		} `json:"items"`
	}
	if err := r.call(ctx, r.Token, "step.open_items", map[string]any{"executor_actor_id": e.ActorID, "limit": 50}, &open); err != nil {
		return 0, err
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
		sem  = make(chan struct{}, e.Parallel)
	)
	taken := 0
	for _, it := range open.Items {
		if it.Kind != "model" || taken >= e.Parallel {
			continue // scripts, people and external systems are not a model's work
		}
		taken++
		wg.Add(1)
		sem <- struct{}{}
		go func(x uuid.UUID, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, err := r.step(ctx, e, x, key)
			if err != nil {
				r.Log.Warn("step failed", "executor", e.Name, "execution", x, "step", key, "err", err)
			}
			if ok {
				mu.Lock()
				done++
				mu.Unlock()
			}
		}(it.ExecutionID, it.StepKey)
	}
	wg.Wait()
	return done, nil
}

// step runs one step: delegate, start, pack, model, finish. It reports whether an attempt was finished.
func (r *Runner) step(ctx context.Context, e Executor, execution uuid.UUID, key string) (bool, error) {
	log := r.Log.With("executor", e.Name, "execution", execution, "step", key)
	var d struct {
		Token string `json:"token"`
	}
	if err := r.call(ctx, r.Token, "step.delegate", map[string]any{"execution_id": execution, "step_key": key,
		"actor_id": e.ActorID, "ttl_s": r.Cfg.DelegateTTLS}, &d); err != nil {
		return false, fmt.Errorf("delegate: %w", err)
	}
	tok := d.Token
	var st struct {
		AttemptID  uuid.UUID `json:"attempt_id"`
		LeaseUntil time.Time `json:"lease_until"`
	}
	if err := r.call(ctx, tok, "step.start", map[string]any{"execution_id": execution, "step_key": key}, &st); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Code == "step_busy" || ae.Code == "step_not_ready") {
			log.Info("step taken by someone else", "code", ae.Code)
			return false, nil
		}
		return false, fmt.Errorf("start: %w", err)
	}
	log = log.With("attempt", st.AttemptID)
	log.Info("step started")

	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.heartbeat(stepCtx, cancel, tok, st.AttemptID, st.LeaseUntil, log)

	finish := func(state string, output any, msg string) (bool, error) {
		in := map[string]any{"attempt_id": st.AttemptID, "state": state}
		if output != nil {
			in["output"] = output
		}
		if msg != "" {
			in["error"] = truncate(msg, 2000)
		}
		if err := r.call(ctx, tok, "step.finish", in, nil); err != nil {
			return false, fmt.Errorf("finish: %w", err)
		}
		log.Info("step finished", "state", state)
		return true, nil
	}

	var pack json.RawMessage
	if err := r.call(stepCtx, tok, "step.pack", map[string]any{"execution_id": execution, "step_key": key}, &pack); err != nil {
		return finish("failed", nil, "step.pack: "+err.Error())
	}
	res, err := r.converse(stepCtx, e, tok, pack, log)
	switch {
	case stepCtx.Err() != nil && ctx.Err() == nil:
		return false, errors.New("lease lost: the attempt was closed by the server")
	case err != nil:
		return finish("failed", nil, err.Error())
	}
	var output any
	if len(res.Output) > 0 && string(res.Output) != "null" {
		output = res.Output
	}
	return finish(res.State, output, res.Error)
}

// heartbeat renews the lease at a third of what is left; a lost lease cancels the step.
func (r *Runner) heartbeat(ctx context.Context, cancel context.CancelFunc, tok string, attempt uuid.UUID, until time.Time, log *slog.Logger) {
	for {
		wait := time.Until(until) / 3
		wait = max(min(wait, 5*time.Minute), 5*time.Second)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		var out struct {
			LeaseUntil time.Time `json:"lease_until"`
		}
		if err := r.call(ctx, tok, "step.checkpoint", map[string]any{"attempt_id": attempt}, &out); err != nil {
			if ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && (ae.Code == "lease_expired" || ae.Code == "attempt_finished" || ae.Status == http.StatusUnauthorized) {
				log.Warn("lease lost", "code", ae.Code)
				cancel()
				return
			}
			log.Warn("heartbeat failed", "err", err)
			continue
		}
		until = out.LeaseUntil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
