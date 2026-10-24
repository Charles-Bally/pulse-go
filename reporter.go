package pulse

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultIntervalSeconds = 30
	minIntervalSeconds     = 10
	maxIntervalSeconds     = 3600
)

var (
	firstReportDelay = 2 * time.Second
	liveInterval     = 5 * time.Second
	processStart     = time.Now()
	secureRefusals   = map[string]string{
		"secure_mode_required": "Mode sécurisé actif pour cette app : ajoute PULSE_PUBLIC_KEY. Les rapports en clair s’arrêtent.",
		"seal_key_mismatch":    "PULSE_PUBLIC_KEY ne correspond plus à la clé de l’app : recopie-la depuis l’app Pulse.",
		"secure_mode_off":      "Le mode sécurisé est coupé pour cette app : retire PULSE_PUBLIC_KEY ou réactive-le dans l’app.",
	}
)

type cronRun struct {
	at  time.Time
	ok  bool
	err string
}

type ack struct {
	Accepted        bool `json:"accepted"`
	IntervalSeconds *int `json:"intervalSeconds"`
	Live            bool `json:"live"`
	Logs            bool `json:"logs"`
}

type Reporter struct {
	cfg              *config
	framework        string
	frameworkVersion string
	collector        *collector
	logsEnabled      bool
	logsOn           atomic.Bool
	logsStopped      atomic.Bool

	mu          sync.Mutex
	probes      []Probe
	queues      []queueSource
	cronSources []cronSource
	cronRuns    map[string]cronRun
	runOrder    []string
	interval    int
	live        bool
	lastFailure string

	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	logWake  chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
	started  atomic.Bool
}

func newReporter(opts Options, env lookupFunc) *Reporter {
	cfg := resolveConfig(opts, env)
	if cfg == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Reporter{
		cfg:         cfg,
		collector:   &collector{minLevel: cfg.selection.minLevel},
		logsEnabled: cfg.selection.sends("logs"),
		cronRuns:    map[string]cronRun{},
		interval:    defaultIntervalSeconds,
		ctx:         ctx,
		cancel:      cancel,
		wake:        make(chan struct{}, 1),
		logWake:     make(chan struct{}, 1),
	}
	r.framework, r.frameworkVersion = currentFramework()
	for _, probe := range opts.Probes {
		r.AddProbe(probe)
	}
	found := detect(opts.Watch)
	for _, probe := range found.probes {
		r.AddProbe(probe)
	}
	r.queues = found.queues
	r.cronSources = found.crons
	return r
}

func (r *Reporter) start() {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	r.wg.Add(1)
	go r.reportLoop()
	if r.logsEnabled {
		r.wg.Add(1)
		go r.logLoop()
	}
}

func (r *Reporter) halt() { r.stopOnce.Do(r.cancel) }

func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.halt()
	r.wg.Wait()
	globalMu.Lock()
	if global == r {
		global = nil
	}
	globalMu.Unlock()
}

func (r *Reporter) AddProbe(probe Probe) {
	if r == nil {
		return
	}
	probe, ok := normalizeProbe(probe)
	if !ok || !r.cfg.selection.allows(probe.Name) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.ContainsFunc(r.probes, func(known Probe) bool { return known.Name == probe.Name }) {
		r.probes = append(r.probes, probe)
	}
}

func (r *Reporter) RecordCronRun(name string, ok bool, err error) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" {
		return
	}
	run := cronRun{at: time.Now(), ok: ok}
	if !ok && err != nil {
		run.err = sanitizeError(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.cronRuns[name]; !known {
		r.runOrder = append(r.runOrder, name)
	}
	r.cronRuns[name] = run
}

func (r *Reporter) reportLoop() {
	defer r.wg.Done()
	timer := time.NewTimer(firstReportDelay)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
			resetTimer(timer, liveInterval)
			continue
		case <-timer.C:
		}
		r.reportNow(r.ctx)
		if r.ctx.Err() != nil {
			return
		}
		resetTimer(timer, r.nextDelay())
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func reportDelay(intervalSeconds int, live bool) time.Duration {
	delay := time.Duration(intervalSeconds) * time.Second
	if live && liveInterval < delay {
		return liveInterval
	}
	return delay
}

func (r *Reporter) nextDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reportDelay(r.interval, r.live)
}

// Ne panique ni ne lève jamais : une panne de Pulse ne doit pas faire tomber l'app surveillée.
func (r *Reporter) reportNow(ctx context.Context) *ack {
	rep := r.buildReport(ctx)
	path := "/ingest/report"
	var body any = rep
	if r.cfg.publicKey != nil {
		sealed, err := sealJSON(r.cfg.publicKey, "report", rep)
		if err != nil {
			r.warnOnce("Rapport non scellé, rien n’est parti : " + sanitizeError(err))
			return nil
		}
		path, body = "/ingest/report/sealed", map[string]any{"sealed": sealed, "clear": clearPart(rep, r.cfg.clear)}
	}
	status, data, err := r.cfg.post(ctx, path, body)
	if err != nil {
		if ctx.Err() == nil {
			r.warnOnce("Pulse injoignable : " + sanitizeError(err))
		}
		return nil
	}
	switch {
	case status == 401:
		r.cfg.fail("Clé Pulse refusée : vérifie PULSE_KEY. Les rapports s’arrêtent.")
		r.halt()
		return nil
	case status == 409:
		var refusal struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &refusal)
		message, known := secureRefusals[refusal.Code]
		if !known {
			message = "Rapport refusé par Pulse."
		}
		r.warnOnce(message)
		if refusal.Code == "secure_mode_required" {
			r.halt()
		}
		return nil
	case status < 200 || status > 299:
		r.warnOnce(fmt.Sprintf("Pulse injoignable : réponse %d", status))
		return nil
	}
	var reply ack
	if err := json.Unmarshal(data, &reply); err != nil {
		r.warnOnce("Pulse injoignable : " + sanitizeError(err))
		return nil
	}
	r.mu.Lock()
	if reply.IntervalSeconds != nil && *reply.IntervalSeconds > 0 {
		r.interval = min(max(*reply.IntervalSeconds, minIntervalSeconds), maxIntervalSeconds)
	}
	r.lastFailure = ""
	r.mu.Unlock()
	r.adoptState(reply.Logs, reply.Live)
	return &reply
}

// Un appareil regarde l'app : l'état part toutes les 5 s et les journaux chaque seconde.
func (r *Reporter) adoptState(logs, live bool) {
	r.mu.Lock()
	becameLive := live && !r.live
	r.live = live
	r.mu.Unlock()
	if r.logsEnabled && !r.logsStopped.Load() {
		r.logsOn.Store(logs)
		if !logs {
			r.collector.clear()
		}
	}
	if becameLive {
		for _, channel := range []chan struct{}{r.wake, r.logWake} {
			select {
			case channel <- struct{}{}:
			default:
			}
		}
	}
}

func (r *Reporter) warnOnce(message string) {
	r.mu.Lock()
	repeated := message == r.lastFailure
	r.lastFailure = message
	r.mu.Unlock()
	if !repeated {
		r.cfg.warn(message)
	}
}
