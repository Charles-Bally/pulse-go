package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	maxProbeNameChars   = 60
	maxProbeEngineChars = 40
	maxChecks           = 20
)

var (
	probeKinds   = []string{"database", "redis", "cache", "queue", "http", "custom"}
	probeTimeout = 5 * time.Second
)

type Probe struct {
	Name   string
	Kind   string
	Engine string
	Run    func(context.Context) error
}

type checkResult struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Engine    string `json:"engine,omitempty"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

func normalizeProbe(probe Probe) (Probe, bool) {
	probe.Name = truncate(strings.TrimSpace(probe.Name), maxProbeNameChars)
	if probe.Name == "" || probe.Run == nil {
		return probe, false
	}
	if !slices.Contains(probeKinds, probe.Kind) {
		probe.Kind = "custom"
	}
	probe.Engine = truncate(strings.TrimSpace(probe.Engine), maxProbeEngineChars)
	return probe, true
}

func safeRun(fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return fn()
}

func runProbe(parent context.Context, probe Probe) checkResult {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- safeRun(func() error { return probe.Run(ctx) }) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		err = fmt.Errorf("pas de réponse en %d ms", probeTimeout.Milliseconds())
	}
	result := checkResult{
		Name:      probe.Name,
		Kind:      probe.Kind,
		Engine:    probe.Engine,
		OK:        err == nil,
		LatencyMs: time.Since(started).Milliseconds(),
	}
	if err != nil {
		result.Error = sanitizeError(err)
	}
	return result
}

func runProbes(ctx context.Context, probes []Probe) []checkResult {
	results := make([]checkResult, len(probes))
	var wg sync.WaitGroup
	for index, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index] = runProbe(ctx, probe)
		}()
	}
	wg.Wait()
	return results
}

var httpProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func HTTPProbe(name, target string) Probe {
	return Probe{Name: name, Kind: "http", Run: func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		response, err := httpProbeClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		if response.StatusCode >= 500 {
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		return nil
	}}
}
