package pulse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

type slogHandler struct {
	next     slog.Handler
	reporter func() *Reporter
	attrs    []string
	group    string
	context  string
	self     bool
}

func (r *Reporter) SlogHandler(next slog.Handler) slog.Handler {
	if r == nil {
		return next
	}
	return &slogHandler{next: next, reporter: func() *Reporter { return r }}
}

func NewSlogHandler(next slog.Handler) slog.Handler {
	return &slogHandler{next: next, reporter: Default}
}

func (h *slogHandler) wants(level slog.Level) bool {
	r := h.reporter()
	return r.capturing() && levelIndex(slogLevelName(level)) >= r.collector.minLevel
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level) || (!h.self && h.wants(level))
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	var err error
	if h.next.Enabled(ctx, record.Level) {
		err = h.next.Handle(ctx, record)
	}
	if h.self || ctx.Value(selfKey{}) != nil || !h.wants(record.Level) {
		return err
	}
	h.reporter().capture(func() (logEntry, bool) { return h.entry(record) })
	return err
}

func (h *slogHandler) entry(record slog.Record) (logEntry, bool) {
	parts := append([]string{record.Message}, h.attrs...)
	context, self := h.context, false
	record.Attrs(func(attr slog.Attr) bool {
		if isSelfAttr(attr) {
			self = true
			return false
		}
		if name, ok := contextName(attr); ok {
			context = name
			return true
		}
		parts = appendAttr(parts, h.group, attr)
		return true
	})
	if self {
		return logEntry{}, false
	}
	return newEntry(record.Time, slogLevelName(record.Level), context, strings.Join(parts, " ")), true
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.next = h.next.WithAttrs(attrs)
	clone.attrs = append([]string(nil), h.attrs...)
	for _, attr := range attrs {
		clone.self = clone.self || isSelfAttr(attr)
		if name, ok := contextName(attr); ok {
			clone.context = name
			continue
		}
		clone.attrs = appendAttr(clone.attrs, h.group, attr)
	}
	return &clone
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.next = h.next.WithGroup(name)
	clone.group = h.group + name + "."
	return &clone
}

func isSelfAttr(attr slog.Attr) bool {
	return attr.Key == "component" && attr.Value.Kind() == slog.KindString && attr.Value.String() == "pulse"
}

func contextName(attr slog.Attr) (string, bool) {
	if attr.Key != "logger" && attr.Key != "component" {
		return "", false
	}
	return attr.Value.Resolve().String(), true
}

func appendAttr(parts []string, prefix string, attr slog.Attr) []string {
	value := attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return parts
	}
	if value.Kind() == slog.KindGroup {
		inner := prefix
		if attr.Key != "" {
			inner += attr.Key + "."
		}
		for _, member := range value.Group() {
			parts = appendAttr(parts, inner, member)
		}
		return parts
	}
	text := value.String()
	if strings.ContainsAny(text, " \t\n\"=") {
		text = fmt.Sprintf("%q", text)
	}
	return append(parts, prefix+attr.Key+"="+text)
}
