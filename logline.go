package pulse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ansiPattern     = regexp.MustCompile("\x1b\\[[0-9;]*m")
	stdLogPrefix    = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} (\d{2}:\d{2}:\d{2}(\.\d+)? )?`)
	levelPrefix     = regexp.MustCompile(`^\[?(TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|CRITICAL|FATAL|PANIC)([+-]\d+)?\]?:?\s+`)
	textLevel       = regexp.MustCompile(`(?:^|\s)level=("?)([A-Za-z]+(?:[+-]\d+)?)"?(?:\s|$)`)
	textMessage     = regexp.MustCompile(`(?:^|\s)msg=("(?:[^"\\]|\\.)*"|\S+)`)
	textComponent   = regexp.MustCompile(`(?:^|\s)(?:logger|component)=("(?:[^"\\]|\\.)*"|\S+)`)
	selfMarkers     = []string{"component=pulse", `"component":"pulse"`}
	slogBaseLevels  = map[string]slog.Level{"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn, "ERROR": slog.LevelError}
	namedLevelAlias = map[string]string{
		"TRACE": "DEBUG", "NOTICE": "INFO", "WARNING": "WARN", "ERR": "ERROR",
		"FATAL": "FATAL", "CRITICAL": "FATAL", "CRIT": "FATAL", "ALERT": "FATAL", "EMERGENCY": "FATAL", "PANIC": "FATAL", "DPANIC": "FATAL",
	}
	guessFatal   = regexp.MustCompile(`(?i)^(panic|fatal)\b`)
	guessError   = regexp.MustCompile(`(?i)^error\b`)
	guessWarning = regexp.MustCompile(`(?i)^warn(ing)?\b`)
)

func slogLevelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARN"
	case level < slog.LevelError+4:
		return "ERROR"
	default:
		return "FATAL"
	}
}

func levelName(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	base, offset := upper, 0
	if index := strings.IndexAny(upper, "+-"); index > 0 {
		base = upper[:index]
		offset, _ = strconv.Atoi(upper[index:])
	}
	if level, ok := slogBaseLevels[base]; ok {
		return slogLevelName(level + slog.Level(offset))
	}
	if alias, ok := namedLevelAlias[base]; ok {
		return alias
	}
	if number, err := strconv.Atoi(upper); err == nil {
		return numericLevel(number)
	}
	return "INFO"
}

func numericLevel(number int) string {
	switch {
	case number >= 60:
		return "FATAL"
	case number >= 50:
		return "ERROR"
	case number >= 40:
		return "WARN"
	case number >= 30:
		return "INFO"
	}
	return "DEBUG"
}

func unquote(text string) string {
	if unquoted, err := strconv.Unquote(text); err == nil {
		return unquoted
	}
	return text
}

func fromJSON(text string) (logEntry, bool) {
	var line map[string]any
	if err := json.Unmarshal([]byte(text), &line); err != nil {
		return logEntry{}, false
	}
	pick := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := line[key]; ok {
				if str, isString := value.(string); isString {
					return str
				}
				return fmt.Sprint(value)
			}
		}
		return ""
	}
	message := pick("msg", "message")
	if message == "" {
		message = text
	}
	at, _ := time.Parse(time.RFC3339Nano, pick("time", "ts", "timestamp"))
	return newEntry(at, levelName(pick("level", "severity")), pick("logger", "component", "name"), message), true
}

func guessLevel(text string) string {
	switch {
	case guessFatal.MatchString(text):
		return "FATAL"
	case guessError.MatchString(text):
		return "ERROR"
	case guessWarning.MatchString(text):
		return "WARN"
	}
	return "INFO"
}

// Une écriture vaut une entrée : une pile d'erreur reste entière.
func parseLine(raw string) (logEntry, bool) {
	text := strings.TrimSpace(ansiPattern.ReplaceAllString(raw, ""))
	if text == "" {
		return logEntry{}, false
	}
	for _, marker := range selfMarkers {
		if strings.Contains(text, marker) {
			return logEntry{}, false
		}
	}
	if strings.HasPrefix(text, "{") {
		if entry, ok := fromJSON(text); ok {
			return entry, true
		}
	}
	text = stdLogPrefix.ReplaceAllString(text, "")
	if level := textLevel.FindStringSubmatch(text); level != nil {
		message := text
		if found := textMessage.FindStringSubmatch(text); found != nil {
			message = unquote(found[1])
		}
		context := ""
		if found := textComponent.FindStringSubmatch(text); found != nil {
			context = unquote(found[1])
		}
		return newEntry(time.Time{}, levelName(level[2]), context, message), true
	}
	if prefix := levelPrefix.FindStringSubmatch(text); prefix != nil {
		return newEntry(time.Time{}, levelName(prefix[1]+prefix[2]), "", text[len(prefix[0]):]), true
	}
	return newEntry(time.Time{}, guessLevel(text), "", text), true
}
