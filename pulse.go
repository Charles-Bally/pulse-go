package pulse

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

const Version = "0.0.1"

const packageName = "pulse-go"

type Options struct {
	Key              string
	Endpoint         string
	PublicKey        string
	Clear            []string
	Send             []string
	Include          []string
	Exclude          []string
	LogLevel         string
	Logs             *bool
	RelayFingerprint string
	AppVersion       string
	Disabled         bool
	Logger           *slog.Logger
	HTTPClient       *http.Client
	Watch            []any
	Probes           []Probe
}

var (
	globalMu sync.Mutex
	global   *Reporter
)

func Start(opts Options) *Reporter {
	globalMu.Lock()
	defer globalMu.Unlock()
	if global != nil {
		return global
	}
	r := newReporter(opts, os.LookupEnv)
	if r == nil {
		return nil
	}
	r.start()
	global = r
	return r
}

func Default() *Reporter {
	globalMu.Lock()
	defer globalMu.Unlock()
	return global
}

func Track(name string, fn func() error) error {
	return TrackContext(context.Background(), name, func(context.Context) error { return fn() })
}

// L'échec part dans Pulse puis revient à l'appelant : une panique est notée puis relancée telle quelle.
func TrackContext(ctx context.Context, name string, fn func(context.Context) error) error {
	r := Default()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.RecordCronRun(name, false, fmt.Errorf("panic: %v", recovered))
			panic(recovered)
		}
	}()
	err := fn(ctx)
	r.RecordCronRun(name, err == nil, err)
	return err
}

type TrackedJob struct {
	name string
	fn   func() error
}

func Job(name string, fn func() error) *TrackedJob {
	return &TrackedJob{name: name, fn: fn}
}

func (j *TrackedJob) Name() string { return j.name }

func (j *TrackedJob) Run() { _ = Track(j.name, j.fn) }
