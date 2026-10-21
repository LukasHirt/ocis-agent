// Package app builds the sidecar's services from the configuration and runs
// the selected roles: the API listener ("api") and the job workers with the
// maintenance scheduler ("worker"), plus an optional debug listener with the
// liveness and readiness probes.
//
// Logs carry ids, counts and fixed categories only, never the text of an
// error from oCIS, the store or the model provider, nor a panic value.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/api"
	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/config"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/scheduler"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// The roles of config.ProcessConfig.Roles.
const (
	RoleAPI    = "api"
	RoleWorker = "worker"
)

// retrySettle is how long after an attempt started a "retry" verdict of a
// reconciled call is honoured (approvals.Config.RetrySettle, also given to
// the scheduler). A job cut off by shutdown is requeued after the same delay
// (agent.Config.ShutdownRequeueDelay), so that the next claim retries an
// approved call that was interrupted instead of reporting it unknown.
const retrySettle = 2 * time.Minute

// Options replace defaults of Build; every field is optional.
type Options struct {
	// HTTPClient is the client for oCIS (default
	// ocisclient.NewHTTPClient(cfg.Insecure)). The model provider never uses it.
	HTTPClient *http.Client
	// Provider replaces the provider built from cfg.LLM; the model catalog
	// still comes from cfg.LLM.
	Provider llm.Provider
	// Now is the clock of the services (default time.Now).
	Now func() time.Time
	// Log is the logger (default slog.Default()).
	Log *slog.Logger

	tune tuning // tests only
}

// tuning shortens fixed delays in tests.
type tuning struct {
	queuePoll            time.Duration // queue poll interval
	eventsPoll           time.Duration // api.Deps.EventsPollForTest
	shutdownRequeueDelay time.Duration // agent.Config.ShutdownRequeueDelay
	// wrapWaker wraps the queue the API wakes (to observe the wakes)
	wrapWaker func(api.Waker) api.Waker
}

// App holds the built services of one process.
type App struct {
	cfg config.Config
	st  store.Store
	log *slog.Logger
	now func() time.Time
	hc  *http.Client

	client    *ocisclient.Client
	secrets   *secrets.Service
	memory    *memory.Service
	auth      *auth.Service
	validator *auth.Validator
	registry  *tools.Registry
	gate      *approvals.Gate
	executor  *approvals.Executor
	queue     *queue.Queue
	env       agent.EnvBuilder
	runner    *agent.Runner        // nil without the worker role
	workers   []*agent.Worker      // empty without the worker role
	workerIDs []string             // ids of workers, in order
	scheduler *scheduler.Scheduler // nil without the worker role
	api       *api.Server          // nil without the api role
	// apiStop is the API's Deps.Stop: Run closes it at the start of the
	// shutdown, so the event streams end before the listener's graceful stop
	apiStop chan struct{}

	// the context the workers' jobs run under; Run cancels it in the last
	// shutdown phase
	jobCtx   context.Context
	stopJobs context.CancelFunc

	secretsReady atomic.Bool
	stopping     atomic.Bool  // set when the shutdown begins
	alive        atomic.Int32 // running worker goroutines

	ran     atomic.Bool
	started chan struct{} // closed once every role is started

	mu        sync.Mutex
	apiAddr   string
	debugAddr string

	// beforeWorkers, if set, runs in Run after the listeners are up and
	// before the workers start (tests only).
	beforeWorkers func()
	// apiHandler, if set, replaces the API server's handler (tests only).
	apiHandler http.Handler

	retrySettle          time.Duration
	shutdownRequeueDelay time.Duration
}

// Build constructs the services in dependency order. st stays the caller's:
// neither Build nor Run closes it. It fails when the worker role is selected
// without a model endpoint (and no Options.Provider) or a model catalog, and
// when any dependency is invalid.
func Build(ctx context.Context, cfg config.Config, st store.Store, opts Options) (*App, error) {
	if st == nil {
		return nil, errors.New("app: a store is required")
	}
	worker := cfg.Process.Has(RoleWorker)
	if worker {
		if opts.Provider == nil && !cfg.LLM.Enabled() {
			return nil, errors.New("app: the worker role needs a model endpoint: set AGENT_LLM_BASE_URL and AGENT_LLM_MODEL, or run only the api role (AGENT_ROLES=api)")
		}
		if !llm.ValidModelID(cfg.LLM.Catalog.Default) || !cfg.LLM.Catalog.Has(cfg.LLM.Catalog.Default) {
			return nil, errors.New("app: the worker role needs a model catalog: set AGENT_LLM_MODEL")
		}
	}

	a := &App{cfg: cfg, st: st, log: opts.Log, now: opts.Now, hc: opts.HTTPClient, started: make(chan struct{})}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.hc == nil {
		a.hc = ocisclient.NewHTTPClient(cfg.Insecure)
	}

	var err error
	if a.client, err = ocisclient.New(cfg.OCISURL, ocisclient.WithHTTPClient(a.hc)); err != nil {
		return nil, fmt.Errorf("app: oCIS client: %w", err)
	}
	if a.secrets, err = secrets.New(st, cfg.MasterKey); err != nil {
		return nil, fmt.Errorf("app: secrets: %w", err)
	}
	// creates the first data-encryption key and checks the master key
	// against the stored one, so a wrong AGENT_MASTER_KEY fails fast
	if err := a.secrets.Init(ctx); err != nil {
		return nil, err
	}
	a.secretsReady.Store(true)
	a.memory = memory.New(a.client, st, memory.DefaultLimits())
	if a.auth, err = auth.New(ctx, auth.Deps{
		Store: st, Secrets: a.secrets, Client: a.client, Memory: a.memory,
		TokenTTL: cfg.Process.TokenTTL, DormancyDays: cfg.Process.DormancyDays, Now: a.now,
	}); err != nil {
		return nil, fmt.Errorf("app: auth: %w", err)
	}
	a.validator = auth.NewValidator(a.client, auth.WithValidatorClock(a.now))
	if a.registry, err = tools.NewDefault(); err != nil {
		return nil, fmt.Errorf("app: tools: %w", err)
	}
	a.retrySettle, a.shutdownRequeueDelay = retrySettle, retrySettle
	if opts.tune.shutdownRequeueDelay > 0 {
		a.shutdownRequeueDelay = opts.tune.shutdownRequeueDelay
	}
	if a.shutdownRequeueDelay < a.retrySettle {
		return nil, errors.New("app: the shutdown requeue delay must be at least the retry settle time")
	}
	acfg := approvals.Config{ApprovalTTL: cfg.Process.ApprovalTTL, RetrySettle: a.retrySettle, Now: a.now}
	if a.gate, err = approvals.NewGate(st, a.registry, acfg); err != nil {
		return nil, fmt.Errorf("app: approvals gate: %w", err)
	}
	if a.executor, err = approvals.NewExecutor(st, a.registry, acfg); err != nil {
		return nil, fmt.Errorf("app: approvals executor: %w", err)
	}
	qopts := []queue.Option{queue.WithLeaseTTL(cfg.Agent.LeaseTTL)}
	if opts.tune.queuePoll > 0 {
		qopts = append(qopts, queue.WithPollInterval(opts.tune.queuePoll))
	}
	a.queue = queue.New(st, qopts...)
	a.env = agent.EnvBuilder{Creds: a.auth, Store: st, Files: a.client, Memory: a.memory}
	if cfg.Process.Has(RoleAPI) {
		var waker api.Waker = a.queue
		if opts.tune.wrapWaker != nil {
			waker = opts.tune.wrapWaker(waker)
		}
		// without a model endpoint the catalog is empty: no model can be
		// chosen and models.available is empty
		a.apiStop = make(chan struct{})
		if a.api, err = api.NewServer(api.Deps{
			Store: st, Validator: a.validator, Auth: a.auth, Gate: a.gate, Queue: waker, Memory: a.memory,
			Catalog: cfg.LLM.Catalog,
			Cfg: api.Settings{
				AllowedOrigin: cfg.AllowedOrigin, KillSwitchEnv: cfg.Agent.KillSwitch,
				UserDailyTokens: cfg.API.UserDailyTokens, RatePerSecond: cfg.API.Rate, Burst: cfg.API.Burst,
				StreamsPerUser: cfg.API.StreamsPerUser, StreamsTotal: cfg.API.StreamsTotal,
				Proactive: proactiveLimits(cfg),
			},
			Log: a.log, Now: a.now, Stop: a.apiStop, EventsPollForTest: opts.tune.eventsPoll,
		}); err != nil {
			return nil, fmt.Errorf("app: api: %w", err)
		}
	}

	a.jobCtx, a.stopJobs = context.WithCancel(context.Background())
	if !worker {
		return a, nil
	}
	if err := a.buildWorkers(opts); err != nil {
		a.stopJobs()
		return nil, err
	}
	return a, nil
}

// buildWorkers builds the provider, the runner, the workers and the
// scheduler of the worker role.
func (a *App) buildWorkers(opts Options) error {
	cfg := a.cfg
	provider := opts.Provider
	if provider == nil {
		p, err := llm.NewOpenAI(openAIConfig(cfg.LLM))
		if err != nil {
			return fmt.Errorf("app: model provider: %w", err)
		}
		provider = p
	}
	rcfg := agent.Config{
		MaxSteps: cfg.Agent.MaxSteps, MaxRuns: cfg.Agent.MaxRuns, MaxTokens: cfg.Agent.MaxTokens,
		KillSwitch: cfg.Agent.KillSwitch, ShutdownRequeueDelay: a.shutdownRequeueDelay,
		UserDailyTokens: cfg.API.UserDailyTokens, Now: a.now,
	}
	own, err := scheduler.NewOwnWrites(a.st)
	if err != nil {
		return fmt.Errorf("app: own writes: %w", err)
	}
	if a.runner, err = agent.NewRunner(agent.Deps{
		Store: a.st, Queue: a.queue, Provider: provider, Catalog: cfg.LLM.Catalog, Registry: a.registry,
		Gate: a.gate, Executor: a.executor, Env: a.env, Creds: a.auth, Cfg: rcfg, OwnWrites: own,
	}); err != nil {
		return fmt.Errorf("app: runner: %w", err)
	}
	prefix := workerPrefix()
	for i := range cfg.Agent.Workers {
		id := prefix + "-" + strconv.Itoa(i)
		w, err := agent.NewWorker(a.queue, a.st, a.runner, agent.WorkerConfig{ID: id, Log: a.log, JobContext: a.jobCtx})
		if err != nil {
			return fmt.Errorf("app: worker: %w", err)
		}
		a.workers = append(a.workers, w)
		a.workerIDs = append(a.workerIDs, id)
	}
	if a.scheduler, err = scheduler.New(scheduler.Deps{
		Store: a.st, Queue: a.queue, Gate: a.gate, Executor: a.executor, Env: a.env.EnvFunc(),
		Auth: a.auth, Log: a.log, Now: a.now,
	}, scheduler.Config{
		MaxAttempts: cfg.Agent.MaxAttempts, LeaseTTL: cfg.Agent.LeaseTTL, RetrySettle: a.retrySettle,
		SweepInterval: cfg.Process.SweepInterval,
		Proactive:     scheduler.ProactiveConfig{Limits: proactiveLimits(cfg)},
		Watch: scheduler.WatchConfig{
			PollInterval: cfg.Process.WatchPollInterval, Debounce: cfg.Process.WatchDebounce, PerTick: cfg.Process.WatchPollsPerTick,
		},
	}); err != nil {
		return fmt.Errorf("app: scheduler: %w", err)
	}
	return nil
}

// proactiveLimits are the operator's caps of proactive work, the same for the
// scheduled sweep and the manual run.
func proactiveLimits(cfg config.Config) store.ProactiveLimits {
	return store.ProactiveLimits{
		DailyJobs: cfg.Agent.ProactiveDailyJobs, DailyTokens: cfg.API.UserDailyTokens, KillSwitchEnv: cfg.Agent.KillSwitch,
	}
}

// workerPrefix is "<hostname>-<pid>" ("agent" without a usable host name):
// with the worker's index it makes worker ids unique across processes.
func workerPrefix() string {
	host, err := os.Hostname()
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return -1
	}, host)
	if err != nil || host == "" {
		host = "agent"
	}
	if len(host) > 64 {
		host = host[:64]
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}

// APIAddr is the bound address of the API listener while Run serves it, ""
// otherwise.
func (a *App) APIAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apiAddr
}

// DebugAddr is the bound address of the debug listener while Run serves it,
// "" otherwise (or when it is disabled).
func (a *App) DebugAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.debugAddr
}

// openAIConfig is the client configuration for the operator's model endpoint.
func openAIConfig(l config.LLMConfig) llm.OpenAIConfig {
	return llm.OpenAIConfig{
		BaseURL: l.BaseURL, APIKey: l.APIKey, Model: l.Catalog.Default,
		AllowedHosts: l.AllowedHosts, AllowHTTP: l.AllowHTTP,
		Timeout: l.Timeout, ReasoningEffort: l.ReasoningEffort,
	}
}
