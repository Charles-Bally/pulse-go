package pulse

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errorType   = reflect.TypeOf((*error)(nil)).Elem()
	sqlDBType   = reflect.TypeOf(&sql.DB{})
	stringsType = reflect.TypeOf([]string(nil))
	timeType    = reflect.TypeOf(time.Time{})
)

var driverEngines = []struct{ fragment, engine string }{
	{"jackc/pgx", "postgresql"},
	{"lib/pq", "postgresql"},
	{"go-sql-driver/mysql", "mysql"},
	{"sqlite", "sqlite"},
	{"mssql", "sqlserver"},
	{"go-ora", "oracle"},
	{"godror", "oracle"},
}

var engineLabels = map[string]string{
	"postgresql":  "PostgreSQL",
	"mysql":       "MySQL",
	"mariadb":     "MariaDB",
	"sqlserver":   "SQL Server",
	"sqlite":      "SQLite",
	"mongodb":     "MongoDB",
	"cockroachdb": "CockroachDB",
	"oracle":      "Oracle",
}

type queueSource func() []Probe

type detection struct {
	probes []Probe
	queues []queueSource
	crons  []cronSource
}

func packagePath(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
}

func engineForPackage(path string) string {
	for _, known := range driverEngines {
		if strings.Contains(path, known.fragment) {
			return known.engine
		}
	}
	return "sql"
}

func engineLabel(engine string) string {
	if label, ok := engineLabels[engine]; ok {
		return label
	}
	return "Base de données"
}

// Lectures sans effet de bord seulement : une sonde ne doit rien écrire chez le client.
func sqlProbe(db *sql.DB) Probe {
	engine := engineForPackage(packagePath(reflect.TypeOf(db.Driver())))
	query := "SELECT 1"
	if engine == "oracle" {
		query = "SELECT 1 FROM DUAL"
	}
	return Probe{Name: engineLabel(engine), Kind: "database", Engine: engine, Run: func(ctx context.Context) error {
		if err := db.PingContext(ctx); err != nil {
			return err
		}
		var one any
		return db.QueryRowContext(ctx, query).Scan(&one)
	}}
}

func embeddedSQL(value reflect.Value) *sql.DB {
	if method := value.MethodByName("DB"); method.IsValid() {
		t := method.Type()
		if t.NumIn() == 0 && t.NumOut() == 2 && t.Out(0) == sqlDBType && t.Out(1) == errorType {
			out := method.Call(nil)
			if out[1].IsNil() && !out[0].IsNil() {
				return out[0].Interface().(*sql.DB)
			}
		}
	}
	inner := reflect.Indirect(value)
	if inner.Kind() == reflect.Struct {
		if field := inner.FieldByName("DB"); field.IsValid() && field.Type() == sqlDBType && field.CanInterface() && !field.IsNil() {
			return field.Interface().(*sql.DB)
		}
	}
	return nil
}

func errorResult(value reflect.Value) error {
	if value.IsNil() {
		return nil
	}
	return value.Interface().(error)
}

func pingMethod(value reflect.Value, inputs int) (reflect.Value, reflect.Type, bool) {
	method := value.MethodByName("Ping")
	if !method.IsValid() {
		return method, nil, false
	}
	t := method.Type()
	if t.NumIn() != inputs || t.In(0) != contextType || t.NumOut() != 1 {
		return method, nil, false
	}
	return method, t, true
}

func redisPing(value reflect.Value) (func(context.Context) error, bool) {
	method, t, ok := pingMethod(value, 1)
	if !ok {
		return nil, false
	}
	errMethod, found := t.Out(0).MethodByName("Err")
	if !found || errMethod.Type.NumOut() != 1 || errMethod.Type.Out(0) != errorType {
		return nil, false
	}
	return func(ctx context.Context) error {
		result := method.Call([]reflect.Value{reflect.ValueOf(ctx)})[0]
		if result.Kind() == reflect.Pointer && result.IsNil() {
			return nil
		}
		return errorResult(result.MethodByName("Err").Call(nil)[0])
	}, true
}

func errorPing(value reflect.Value, inputs int) (func(context.Context) error, bool) {
	method, t, ok := pingMethod(value, inputs)
	if !ok || t.Out(0) != errorType {
		return nil, false
	}
	return func(ctx context.Context) error {
		args := []reflect.Value{reflect.ValueOf(ctx)}
		if inputs == 2 {
			args = append(args, reflect.Zero(t.In(1)))
		}
		return errorResult(method.Call(args)[0])
	}, true
}

func probeFor(item any, value reflect.Value) (Probe, bool) {
	if db, ok := item.(*sql.DB); ok {
		return sqlProbe(db), true
	}
	if db := embeddedSQL(value); db != nil {
		return sqlProbe(db), true
	}
	if run, ok := redisPing(value); ok {
		return Probe{Name: "Redis", Kind: "redis", Engine: "redis", Run: run}, true
	}
	if run, ok := errorPing(value, 2); ok {
		return Probe{Name: "MongoDB", Kind: "database", Engine: "mongodb", Run: run}, true
	}
	if run, ok := errorPing(value, 1); ok && strings.Contains(packagePath(value.Type()), "jackc/pgx") {
		return Probe{Name: "PostgreSQL", Kind: "database", Engine: "postgresql", Run: run}, true
	}
	return Probe{}, false
}

func withTimeout[T any](fn func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		var result outcome
		result.err = safeRun(func() error {
			var err error
			result.value, err = fn()
			return err
		})
		done <- result
	}()
	select {
	case result := <-done:
		return result.value, result.err
	case <-time.After(probeTimeout):
		var zero T
		return zero, fmt.Errorf("pas de réponse en %d ms", probeTimeout.Milliseconds())
	}
}

// Les files d'asynq n'existent qu'une fois servies : la liste est relue à chaque rapport, la dernière connue sert de repli.
func asynqSource(value reflect.Value) (queueSource, bool) {
	list, info := value.MethodByName("Queues"), value.MethodByName("GetQueueInfo")
	if !list.IsValid() || !info.IsValid() {
		return nil, false
	}
	lt, it := list.Type(), info.Type()
	if lt.NumIn() != 0 || lt.NumOut() != 2 || lt.Out(0) != stringsType || lt.Out(1) != errorType {
		return nil, false
	}
	if it.NumIn() != 1 || it.In(0).Kind() != reflect.String || it.NumOut() != 2 || it.Out(1) != errorType {
		return nil, false
	}
	var mu sync.Mutex
	known := []string{"default"}
	return func() []Probe {
		names, err := withTimeout(func() ([]string, error) {
			out := list.Call(nil)
			return out[0].Interface().([]string), errorResult(out[1])
		})
		mu.Lock()
		if err == nil && len(names) > 0 {
			known = append([]string(nil), names...)
			sort.Strings(known)
		}
		names = append([]string(nil), known...)
		mu.Unlock()
		probes := make([]Probe, 0, len(names))
		for _, queue := range names {
			probes = append(probes, Probe{Name: "File " + queue, Kind: "queue", Engine: "asynq", Run: func(context.Context) error {
				out := info.Call([]reflect.Value{reflect.ValueOf(queue).Convert(it.In(0))})
				return errorResult(out[1])
			}})
		}
		return probes
	}, true
}

func detect(items []any) detection {
	var found detection
	seen := map[any]bool{}
	labels := map[string]int{}
	for _, item := range items {
		value := reflect.ValueOf(item)
		if !value.IsValid() || (value.Kind() == reflect.Pointer && value.IsNil()) {
			continue
		}
		if value.Kind() == reflect.Pointer {
			if seen[item] {
				continue
			}
			seen[item] = true
		}
		_ = safeRun(func() error {
			if probe, ok := probeFor(item, value); ok {
				labels[probe.Name]++
				if count := labels[probe.Name]; count > 1 {
					probe.Name = fmt.Sprintf("%s (%d)", probe.Name, count)
				}
				found.probes = append(found.probes, probe)
			} else if source, ok := asynqSource(value); ok {
				found.queues = append(found.queues, source)
			} else if source, ok := cronSourceFor(value); ok {
				found.crons = append(found.crons, source)
			}
			return nil
		})
	}
	return found
}
