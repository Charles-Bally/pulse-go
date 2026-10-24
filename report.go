package pulse

import (
	"context"
	"slices"
)

type report struct {
	PackageName      string        `json:"packageName"`
	PackageVersion   string        `json:"packageVersion"`
	Framework        string        `json:"framework"`
	FrameworkVersion string        `json:"frameworkVersion,omitempty"`
	AppVersion       string        `json:"appVersion,omitempty"`
	DeployedAt       string        `json:"deployedAt,omitempty"`
	Checks           []checkResult `json:"checks"`
	Crons            []cronEntry   `json:"crons"`
}

func (r *Reporter) buildReport(ctx context.Context) report {
	selection := r.cfg.selection
	rep := report{
		PackageName:    packageName,
		PackageVersion: Version,
		Framework:      r.framework,
		Checks:         []checkResult{},
		Crons:          []cronEntry{},
	}
	if selection.sends("versions") {
		rep.FrameworkVersion = r.frameworkVersion
		rep.AppVersion = r.cfg.appVersion
		rep.DeployedAt = isoTime(processStart)
	}
	if selection.sends("checks") {
		rep.Checks = runProbes(ctx, r.currentProbes())
	}
	if selection.sends("crons") {
		rep.Crons = r.crons()
	}
	return rep
}

func (r *Reporter) currentProbes() []Probe {
	r.mu.Lock()
	probes := slices.Clone(r.probes)
	queues := slices.Clone(r.queues)
	r.mu.Unlock()
	for _, source := range queues {
		for _, probe := range source() {
			if !r.cfg.selection.allows(probe.Name) {
				continue
			}
			if !slices.ContainsFunc(probes, func(known Probe) bool { return known.Name == probe.Name }) {
				probes = append(probes, probe)
			}
		}
	}
	if len(probes) > maxChecks {
		probes = probes[:maxChecks]
	}
	return probes
}

func (r *Reporter) crons() []cronEntry {
	r.mu.Lock()
	sources := slices.Clone(r.cronSources)
	order := slices.Clone(r.runOrder)
	runs := make(map[string]cronRun, len(r.cronRuns))
	for name, run := range r.cronRuns {
		runs[name] = run
	}
	r.mu.Unlock()
	var list []cronSnapshot
	index := map[string]int{}
	put := func(snapshot cronSnapshot) {
		if position, known := index[snapshot.name]; known {
			list[position] = snapshot
			return
		}
		index[snapshot.name] = len(list)
		list = append(list, snapshot)
	}
	for _, source := range sources {
		snapshots, err := source()
		if err != nil {
			r.warnOnce("Lecture des tâches planifiées impossible : " + sanitizeError(err))
			continue
		}
		for _, snapshot := range snapshots {
			put(snapshot)
		}
	}
	for _, name := range order {
		run := runs[name]
		snapshot := cronSnapshot{name: name}
		if position, known := index[name]; known {
			snapshot = list[position]
		}
		ok := run.ok
		snapshot.lastRunAt, snapshot.lastRunOK, snapshot.err = run.at, &ok, run.err
		put(snapshot)
	}
	entries := []cronEntry{}
	for _, snapshot := range list {
		if len(entries) == maxCrons {
			break
		}
		if r.cfg.selection.allows(snapshot.name) {
			entries = append(entries, snapshot.entry())
		}
	}
	return entries
}
