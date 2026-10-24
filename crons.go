package pulse

import (
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	maxCrons           = 50
	maxCronNameChars   = 80
	maxCronScheduleLen = 80
)

var closureName = regexp.MustCompile(`(^|\.)func\d+`)

type cronSnapshot struct {
	name      string
	schedule  string
	lastRunAt time.Time
	nextRunAt time.Time
	lastRunOK *bool
	err       string
}

type cronSource func() ([]cronSnapshot, error)

type cronEntry struct {
	Name      string `json:"name"`
	Schedule  string `json:"schedule,omitempty"`
	LastRunAt string `json:"lastRunAt,omitempty"`
	NextRunAt string `json:"nextRunAt,omitempty"`
	LastRunOK *bool  `json:"lastRunOk,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s cronSnapshot) entry() cronEntry {
	entry := cronEntry{
		Name:      truncate(s.name, maxCronNameChars),
		Schedule:  truncate(s.schedule, maxCronScheduleLen),
		LastRunOK: s.lastRunOK,
		Error:     truncate(s.err, maxErrorChars),
	}
	if !s.lastRunAt.IsZero() {
		entry.LastRunAt = isoTime(s.lastRunAt)
	}
	if !s.nextRunAt.IsZero() {
		entry.NextRunAt = isoTime(s.nextRunAt)
	}
	return entry
}

func cronSourceFor(value reflect.Value) (cronSource, bool) {
	if source, ok := robfigSource(value); ok {
		return source, true
	}
	return gocronSource(value)
}

func sliceMethod(value reflect.Value, name string) (reflect.Value, reflect.Type, bool) {
	method := value.MethodByName(name)
	if !method.IsValid() {
		return method, nil, false
	}
	t := method.Type()
	if t.NumIn() != 0 || t.NumOut() != 1 || t.Out(0).Kind() != reflect.Slice {
		return method, nil, false
	}
	return method, t.Out(0).Elem(), true
}

func hasField(t reflect.Type, name string, want reflect.Type) bool {
	field, ok := t.FieldByName(name)
	return ok && (want == nil || field.Type == want)
}

func robfigSource(value reflect.Value) (cronSource, bool) {
	method, entry, ok := sliceMethod(value, "Entries")
	if !ok || entry.Kind() != reflect.Struct {
		return nil, false
	}
	if !hasField(entry, "Next", timeType) || !hasField(entry, "Prev", timeType) || !hasField(entry, "Job", nil) {
		return nil, false
	}
	return func() ([]cronSnapshot, error) {
		var snapshots []cronSnapshot
		err := safeRun(func() error {
			list := method.Call(nil)[0]
			for index := range list.Len() {
				snapshots = append(snapshots, robfigEntry(list.Index(index)))
			}
			return nil
		})
		return snapshots, err
	}, true
}

func robfigEntry(entry reflect.Value) cronSnapshot {
	snapshot := cronSnapshot{
		name:      jobName(entry.FieldByName("Job"), entry.FieldByName("ID")),
		nextRunAt: entry.FieldByName("Next").Interface().(time.Time),
		lastRunAt: entry.FieldByName("Prev").Interface().(time.Time),
	}
	if schedule := entry.FieldByName("Schedule"); schedule.IsValid() {
		snapshot.schedule = describeSchedule(schedule)
	}
	return snapshot
}

func jobName(job, id reflect.Value) string {
	if job.IsValid() && job.Kind() == reflect.Interface && !job.IsNil() && job.CanInterface() {
		inner := job.Elem()
		if named, ok := inner.Interface().(interface{ Name() string }); ok && named.Name() != "" {
			return named.Name()
		}
		if inner.Kind() == reflect.Func {
			if name := funcName(inner.Pointer()); name != "" {
				return name
			}
		}
	}
	if id.IsValid() && id.CanInterface() {
		return fmt.Sprintf("cron %v", id.Interface())
	}
	return "cron"
}

func funcName(pc uintptr) string {
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return ""
	}
	name := fn.Name()
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	if index := strings.Index(name, "."); index >= 0 {
		name = name[index+1:]
	}
	name = strings.TrimSuffix(name, "-fm")
	if closureName.MatchString(name) {
		return ""
	}
	return name
}

// robfig/cron ne garde pas l'expression d'origine : on la reconstruit depuis les champs de bits du planning.
func describeSchedule(schedule reflect.Value) string {
	for schedule.Kind() == reflect.Interface || schedule.Kind() == reflect.Pointer {
		if schedule.IsNil() {
			return ""
		}
		schedule = schedule.Elem()
	}
	if schedule.Kind() != reflect.Struct {
		return ""
	}
	if delay := schedule.FieldByName("Delay"); delay.IsValid() && delay.Type() == reflect.TypeOf(time.Duration(0)) {
		return "@every " + time.Duration(delay.Int()).String()
	}
	fields := []struct {
		name     string
		min, max int
	}{{"Minute", 0, 59}, {"Hour", 0, 23}, {"Dom", 1, 31}, {"Month", 1, 12}, {"Dow", 0, 6}}
	parts := []string{}
	for _, field := range fields {
		bits := schedule.FieldByName(field.name)
		if !bits.IsValid() || bits.Kind() != reflect.Uint64 {
			return ""
		}
		parts = append(parts, cronField(bits.Uint(), field.min, field.max))
	}
	if seconds := schedule.FieldByName("Second"); seconds.IsValid() && seconds.Kind() == reflect.Uint64 && seconds.Uint() != 1 {
		parts = append([]string{cronField(seconds.Uint(), 0, 59)}, parts...)
	}
	return strings.Join(parts, " ")
}

func cronField(bits uint64, low, high int) string {
	values := []int{}
	for value := low; value <= high; value++ {
		if bits&(1<<uint(value)) != 0 {
			values = append(values, value)
		}
	}
	if len(values) == 0 || len(values) == high-low+1 {
		return "*"
	}
	if len(values) > 1 {
		step := values[1] - values[0]
		regular := true
		for index := 2; index < len(values); index++ {
			regular = regular && values[index]-values[index-1] == step
		}
		last := values[len(values)-1]
		if regular && step > 1 && values[0] == low && high-last < step {
			return "*/" + strconv.Itoa(step)
		}
		if regular && step == 1 {
			return strconv.Itoa(values[0]) + "-" + strconv.Itoa(last)
		}
	}
	texts := make([]string, len(values))
	for index, value := range values {
		texts[index] = strconv.Itoa(value)
	}
	return strings.Join(texts, ",")
}

func gocronSource(value reflect.Value) (cronSource, bool) {
	method, job, ok := sliceMethod(value, "Jobs")
	if !ok {
		return nil, false
	}
	_, hasName := job.MethodByName("Name")
	_, hasGetName := job.MethodByName("GetName")
	_, hasNext := job.MethodByName("NextRun")
	_, hasLast := job.MethodByName("LastRun")
	if !(hasName || hasGetName) || !hasNext || !hasLast {
		return nil, false
	}
	return func() ([]cronSnapshot, error) {
		var snapshots []cronSnapshot
		err := safeRun(func() error {
			list := method.Call(nil)[0]
			for index := range list.Len() {
				item := list.Index(index)
				nameMethod := item.MethodByName("Name")
				if !nameMethod.IsValid() {
					nameMethod = item.MethodByName("GetName")
				}
				name := strings.TrimSpace(nameMethod.Call(nil)[0].String())
				if name == "" {
					continue
				}
				snapshots = append(snapshots, cronSnapshot{
					name:      name,
					nextRunAt: timeResult(item.MethodByName("NextRun").Call(nil)),
					lastRunAt: timeResult(item.MethodByName("LastRun").Call(nil)),
				})
			}
			return nil
		})
		return snapshots, err
	}, true
}

func timeResult(out []reflect.Value) time.Time {
	if len(out) == 0 || out[0].Type() != timeType {
		return time.Time{}
	}
	if len(out) == 2 && out[1].Type() == errorType && !out[1].IsNil() {
		return time.Time{}
	}
	return out[0].Interface().(time.Time)
}
