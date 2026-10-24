package pulse

import (
	"regexp"
	"slices"
	"strings"
)

var (
	sendFields  = []string{"checks", "crons", "logs", "versions"}
	clearFields = []string{"status", "checks", "crons", "versions"}
	logLevels   = []string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}
)

type selection struct {
	send     []string
	include  []*regexp.Regexp
	exclude  []*regexp.Regexp
	minLevel int
}

func splitList(raw string) []string {
	items := []string{}
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

func canonical(allowed, wanted []string) []string {
	normalized := make([]string, 0, len(wanted))
	for _, item := range wanted {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(item)))
	}
	kept := []string{}
	for _, field := range allowed {
		if slices.Contains(normalized, field) {
			kept = append(kept, field)
		}
	}
	return kept
}

func levelIndex(level string) int {
	return slices.Index(logLevels, strings.ToUpper(strings.TrimSpace(level)))
}

func patterns(option []string, env lookupFunc, name string) []*regexp.Regexp {
	items := option
	if items == nil {
		raw, _ := env(name)
		items = splitList(raw)
	}
	compiled := []*regexp.Regexp{}
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		escaped := strings.ReplaceAll(regexp.QuoteMeta(trimmed), `\*`, ".*")
		compiled = append(compiled, regexp.MustCompile("(?i)^"+escaped+"$"))
	}
	return compiled
}

func resolveSelection(opts Options, env lookupFunc) selection {
	wanted := opts.Send
	if wanted == nil {
		if raw, ok := env("PULSE_SEND"); ok {
			wanted = splitList(raw)
		} else {
			wanted = sendFields
		}
	}
	logsOff := (opts.Logs != nil && !*opts.Logs) || (opts.Logs == nil && envValue(env, "PULSE_LOGS") == "false")
	send := []string{}
	for _, field := range canonical(sendFields, wanted) {
		if field != "logs" || !logsOff {
			send = append(send, field)
		}
	}
	level := opts.LogLevel
	if level == "" {
		level = envValue(env, "PULSE_LOG_LEVEL")
	}
	return selection{
		send:     send,
		include:  patterns(opts.Include, env, "PULSE_INCLUDE"),
		exclude:  patterns(opts.Exclude, env, "PULSE_EXCLUDE"),
		minLevel: max(levelIndex(level), 0),
	}
}

func (s selection) sends(field string) bool { return slices.Contains(s.send, field) }

// Un nom passe s'il est dans la liste blanche (vide = tout) et dans aucune exclusion ; « * » sert de joker.
func (s selection) allows(name string) bool {
	matches := func(list []*regexp.Regexp) bool {
		return slices.ContainsFunc(list, func(pattern *regexp.Regexp) bool { return pattern.MatchString(name) })
	}
	return (len(s.include) == 0 || matches(s.include)) && !matches(s.exclude)
}
