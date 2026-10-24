package pulse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const logBatchSize = 200

var (
	logFlushInterval = 10 * time.Second
	logLiveInterval  = time.Second
)

func (r *Reporter) logDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live {
		return logLiveInterval
	}
	return logFlushInterval
}

func (r *Reporter) logLoop() {
	defer r.wg.Done()
	for {
		timer := time.NewTimer(r.logDelay())
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-r.logWake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		r.flushLogs(r.ctx)
		if r.logsStopped.Load() {
			return
		}
	}
}

func (r *Reporter) stopLogs() {
	r.logsStopped.Store(true)
	r.logsOn.Store(false)
	r.collector.clear()
}

func (r *Reporter) flushLogs(ctx context.Context) int {
	if !r.logsOn.Load() || r.collector.size() == 0 {
		return 0
	}
	entries := r.collector.take(logBatchSize)
	path := "/ingest/logs"
	var body any = map[string]any{"entries": entries}
	if r.cfg.publicKey != nil {
		sealed, err := sealJSON(r.cfg.publicKey, "logs", entries)
		if err != nil {
			r.warnOnce("Journaux non envoyés à Pulse : " + sanitizeError(err))
			return 0
		}
		path, body = "/ingest/logs/sealed", map[string]any{"sealed": sealed, "count": len(entries), "at": entries[0].At}
	}
	status, data, err := r.cfg.post(ctx, path, body)
	if err == nil && (status == 401 || status == 409) {
		r.stopLogs()
		return 0
	}
	var reply struct {
		Logs bool `json:"logs"`
		Live bool `json:"live"`
	}
	if err == nil && (status < 200 || status > 299) {
		err = fmt.Errorf("réponse %d", status)
	}
	if err == nil {
		err = json.Unmarshal(data, &reply)
	}
	if err != nil {
		r.collector.putBack(entries)
		if ctx.Err() == nil {
			r.warnOnce("Journaux non envoyés à Pulse : " + sanitizeError(err))
		}
		return 0
	}
	r.adoptState(reply.Logs, reply.Live)
	return len(entries)
}
