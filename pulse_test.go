package pulse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "pk_live_8f2aQx3f9aB7cD1eF5gH9iJ2kL4mN6oP8qR"

func envOf(values map[string]string) lookupFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

type messages struct {
	mu    sync.Mutex
	lines []string
}

func (m *messages) Enabled(context.Context, slog.Level) bool { return true }
func (m *messages) WithAttrs([]slog.Attr) slog.Handler       { return m }
func (m *messages) WithGroup(string) slog.Handler            { return m }
func (m *messages) Handle(_ context.Context, record slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, record.Message)
	return nil
}

func (m *messages) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}

func (m *messages) last() string {
	lines := m.all()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

type received struct {
	path   string
	header http.Header
	body   map[string]any
}

type fakePulse struct {
	*httptest.Server
	mu       sync.Mutex
	requests []received
	status   int
	reply    string
}

func newFakePulse(t *testing.T, status int, reply string) *fakePulse {
	fake := &fakePulse{status: status, reply: reply}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakePulse) serve(w http.ResponseWriter, request *http.Request) {
	data, _ := io.ReadAll(request.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	f.mu.Lock()
	f.requests = append(f.requests, received{path: request.URL.Path, header: request.Header.Clone(), body: body})
	status, reply := f.status, f.reply
	f.mu.Unlock()
	w.WriteHeader(status)
	_, _ = w.Write([]byte(reply))
}

func (f *fakePulse) received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.requests...)
}

func (f *fakePulse) set(status int, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.reply = status, reply
}

func reporterFor(t *testing.T, endpoint string, opts Options, env map[string]string) (*Reporter, *messages) {
	t.Helper()
	said := &messages{}
	opts.Endpoint, opts.Logger = endpoint, slog.New(said)
	values := map[string]string{"PULSE_KEY": testKey}
	for key, value := range env {
		values[key] = value
	}
	r := newReporter(opts, envOf(values))
	if r == nil {
		t.Fatalf("rapporteur absent : %v", said.all())
	}
	t.Cleanup(r.Stop)
	return r, said
}

func okProbe(name, kind string) Probe {
	return Probe{Name: name, Kind: kind, Engine: kind, Run: func(context.Context) error { return nil }}
}

func TestConfiguration(t *testing.T) {
	cfg := resolveConfig(Options{}, envOf(map[string]string{"PULSE_KEY": " " + testKey + " ", "PULSE_APP_VERSION": "2.3.0"}))
	if cfg == nil || cfg.key != testKey || cfg.endpoint != defaultEndpoint || cfg.appVersion != "2.3.0" {
		t.Fatalf("configuration lue : %+v", cfg)
	}
	said := &messages{}
	logger := slog.New(said)
	cases := []struct {
		env     map[string]string
		message string
	}{
		{map[string]string{}, "PULSE_KEY absente : Pulse ne surveille pas cette app."},
		{map[string]string{"PULSE_KEY": "sk_live_xxx"}, "PULSE_KEY mal formée : elle commence par pk_live_ ou pk_test_."},
		{map[string]string{"PULSE_KEY": testKey, "PULSE_ENDPOINT": "http://api.exemple.ci"}, "PULSE_ENDPOINT doit être en https."},
		{map[string]string{"PULSE_KEY": testKey, "PULSE_PUBLIC_KEY": "pulse_pub_abc"}, "PULSE_PUBLIC_KEY mal formée : copie-la depuis l’app Pulse, elle commence par pulse_pub_."},
		{map[string]string{"PULSE_KEY": testKey, "PULSE_RELAY_FINGERPRINT": "AB:CD"}, "PULSE_RELAY_FINGERPRINT mal formée : copie l’empreinte SHA-256 affichée par ton relais."},
	}
	for _, c := range cases {
		if resolveConfig(Options{Logger: logger}, envOf(c.env)) != nil || said.last() != c.message {
			t.Errorf("attendu %q, reçu %q", c.message, said.last())
		}
	}
	before := len(said.all())
	if resolveConfig(Options{Logger: logger}, envOf(map[string]string{"PULSE_KEY": testKey, "PULSE_DISABLED": "true"})) != nil || len(said.all()) != before {
		t.Error("PULSE_DISABLED doit tout couper sans message")
	}
	if strings.Contains(strings.Join(said.all(), "\n"), testKey) {
		t.Error("la clé ne doit jamais apparaître dans un message")
	}
	local := resolveConfig(Options{Endpoint: "http://localhost:3200/chemin?x=1"}, envOf(map[string]string{"PULSE_KEY": testKey}))
	if local == nil || local.endpoint != "http://localhost:3200" {
		t.Errorf("adresse locale : %+v", local)
	}
	public := "pulse_pub_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	env := map[string]string{"PULSE_KEY": testKey, "PULSE_PUBLIC_KEY": public}
	if cfg := resolveConfig(Options{}, envOf(env)); cfg == nil || strings.Join(cfg.clear, ",") != "status" {
		t.Errorf("PULSE_CLEAR par défaut : %+v", cfg)
	}
	env["PULSE_CLEAR"] = " crons, checks, inconnu"
	if cfg := resolveConfig(Options{}, envOf(env)); strings.Join(cfg.clear, ",") != "checks,crons" {
		t.Errorf("PULSE_CLEAR lu : %v", cfg.clear)
	}
	env["PULSE_CLEAR"] = "none"
	if cfg := resolveConfig(Options{}, envOf(env)); len(cfg.clear) != 0 {
		t.Errorf("PULSE_CLEAR=none : %v", cfg.clear)
	}
}

func TestSelection(t *testing.T) {
	fake := newFakePulse(t, 202, `{"accepted":true}`)
	r, _ := reporterFor(t, fake.URL, Options{AppVersion: "2.3.0"}, map[string]string{
		"PULSE_SEND": "logs, checks", "PULSE_EXCLUDE": "File *", "PULSE_LOG_LEVEL": "warn",
	})
	r.AddProbe(okProbe("File emails", "queue"))
	r.AddProbe(okProbe("Redis", "redis"))
	r.AddProbe(okProbe("Redis", "redis"))
	r.RecordCronRun("relance", true, nil)
	rep := r.buildReport(context.Background())
	if len(rep.Checks) != 1 || rep.Checks[0].Name != "Redis" || len(rep.Crons) != 0 || rep.AppVersion != "" || rep.DeployedAt != "" {
		t.Fatalf("rapport filtré : %+v", rep)
	}
	if strings.Join(r.cfg.selection.send, ",") != "checks,logs" {
		t.Errorf("ordre canonique : %v", r.cfg.selection.send)
	}
	r.collector.push(newEntry(time.Time{}, "INFO", "", "bavard"))
	r.collector.push(newEntry(time.Time{}, "ERROR", "", "utile"))
	if taken := r.collector.take(10); len(taken) != 1 || taken[0].Message != "utile" {
		t.Errorf("niveau minimal : %+v", taken)
	}
	include := resolveSelection(Options{Include: []string{"postgre*", "relance-*"}}, envOf(nil))
	if !include.allows("PostgreSQL") || !include.allows("relance-abonnements") || include.allows("Redis") {
		t.Error("liste blanche avec joker, insensible à la casse")
	}
	off := false
	if resolveSelection(Options{Logs: &off}, envOf(nil)).sends("logs") || resolveSelection(Options{}, envOf(map[string]string{"PULSE_LOGS": "false"})).sends("logs") {
		t.Error("logs=false retire les journaux")
	}
}

type codedError struct{}

func (codedError) Error() string {
	return "connect failed postgres://helio:motdepasse@db:5432/helio?sslmode=require"
}
func (codedError) Code() string { return "ECONNREFUSED" }

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		sanitizeError(codedError{}): "ECONNREFUSED connect failed postgres://***@db:5432/helio?sslmode=require",
		sanitizeError(errors.New("GET https://api.exemple.ci/v1?token=abc failed")): "GET https://api.exemple.ci/v1 failed",
		sanitizeError(errors.New("auth password=hunter2 refused")):                  "auth password=*** refused",
		sanitizeError(errors.New("Authorization: Bearer abcdefgh12345")):            "Authorization: Bearer ***",
		sanitizeError(errors.New("clé " + testKey)):                                 "clé pk_live_***",
		sanitizeError(errors.New("  ")):                                             "erreur inconnue",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("attendu %q, reçu %q", want, got)
		}
	}
	if got := sanitizeError(errors.New(strings.Repeat("é", 400))); len([]rune(got)) != 300 {
		t.Errorf("coupé à 300 caractères : %d", len([]rune(got)))
	}
	if got := truncate(strings.Repeat("😀", 3), 5); got != "😀😀" {
		t.Errorf("compte UTF-16 : %q", got)
	}
}

func TestProbes(t *testing.T) {
	defer func(previous time.Duration) { probeTimeout = previous }(probeTimeout)
	probeTimeout = 20 * time.Millisecond
	ctx := context.Background()
	if result := runProbe(ctx, okProbe("Redis", "redis")); !result.OK || result.Error != "" {
		t.Errorf("sonde réussie : %+v", result)
	}
	failed := runProbe(ctx, Probe{Name: "PostgreSQL", Kind: "database", Run: func(context.Context) error { return errors.New("down") }})
	if failed.OK || failed.Error != "down" {
		t.Errorf("sonde en échec : %+v", failed)
	}
	slow := runProbe(ctx, Probe{Name: "Lente", Kind: "custom", Run: func(context.Context) error { time.Sleep(time.Second); return nil }})
	if slow.OK || slow.Error != "pas de réponse en 20 ms" || slow.LatencyMs < 20 {
		t.Errorf("délai dépassé : %+v", slow)
	}
	panicked := runProbe(ctx, Probe{Name: "Fragile", Kind: "custom", Run: func(context.Context) error { panic("boum") }})
	if panicked.OK || panicked.Error != "panic: boum" {
		t.Errorf("panique contenue : %+v", panicked)
	}
	probeTimeout = 5 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/moved":
			http.Redirect(w, request, "/down", http.StatusFound)
		default:
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	for path, want := range map[string]string{"/health": "", "/moved": "", "/down": "HTTP 503"} {
		if result := runProbe(ctx, HTTPProbe("Route", server.URL+path)); result.Error != want || result.Kind != "http" {
			t.Errorf("%s : %+v", path, result)
		}
	}
	r, _ := reporterFor(t, "http://localhost:1", Options{}, nil)
	r.AddProbe(Probe{Name: "  ", Run: func(context.Context) error { return nil }})
	r.AddProbe(Probe{Name: "Bizarre", Kind: "inconnu", Run: func(context.Context) error { return nil }})
	if len(r.probes) != 1 || r.probes[0].Kind != "custom" {
		t.Errorf("sondes normalisées : %+v", r.probes)
	}
}

func TestTrackAndCrons(t *testing.T) {
	fake := newFakePulse(t, 202, `{"accepted":true}`)
	r, _ := reporterFor(t, fake.URL, Options{}, nil)
	globalMu.Lock()
	global = r
	globalMu.Unlock()
	next := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	r.cronSources = append(r.cronSources, func() ([]cronSnapshot, error) {
		return []cronSnapshot{{name: "relance-abonnements", schedule: "0 * * * *", nextRunAt: next}}, nil
	})
	boom := errors.New("timeout token=abc")
	if err := Track("relance-abonnements", func() error { return boom }); err != boom {
		t.Fatalf("Track relance l'erreur : %v", err)
	}
	if err := Track("nettoyage", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("la panique doit être relancée")
			}
		}()
		_ = Track("export", func() error { panic("disque plein") })
	}()
	Job("rapport", func() error { return nil }).Run()
	crons := r.crons()
	if len(crons) != 4 {
		t.Fatalf("tâches : %+v", crons)
	}
	first := crons[0]
	if first.Name != "relance-abonnements" || first.Schedule != "0 * * * *" || first.NextRunAt != "2026-10-22T09:00:00.000Z" ||
		first.LastRunOK == nil || *first.LastRunOK || first.Error != "timeout token=***" || first.LastRunAt == "" {
		t.Errorf("tâche fusionnée : %+v", first)
	}
	if crons[1].Name != "nettoyage" || !*crons[1].LastRunOK || crons[1].Error != "" || crons[2].Error != "panic: disque plein" || crons[3].Name != "rapport" {
		t.Errorf("passages : %+v", crons[1:])
	}
	r.Stop()
	if Default() != nil {
		t.Error("Stop retire le rapporteur global")
	}
	if err := Track("sans-pulse", func() error { return nil }); err != nil {
		t.Error("Track sans rapporteur exécute quand même la tâche")
	}
}

func TestReport(t *testing.T) {
	fake := newFakePulse(t, 202, `{"accepted":true,"intervalSeconds":60,"keyId":null,"live":false,"logs":false}`)
	r, _ := reporterFor(t, fake.URL, Options{AppVersion: "2.3.0"}, nil)
	r.AddProbe(okProbe("Redis", "redis"))
	reply := r.reportNow(context.Background())
	if reply == nil || r.interval != 60 {
		t.Fatalf("intervalle adopté : %+v %d", reply, r.interval)
	}
	request := fake.received()[0]
	if request.path != "/ingest/report" || request.header.Get("x-pulse-key") != testKey ||
		request.header.Get("user-agent") != "pulse-go/"+Version || request.header.Get("content-type") != "application/json" {
		t.Errorf("requête : %s %v", request.path, request.header)
	}
	body := request.body
	if body["packageName"] != "pulse-go" || body["packageVersion"] != Version || body["framework"] == "" || body["appVersion"] != "2.3.0" {
		t.Errorf("corps : %v", body)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(body["deployedAt"].(string)) {
		t.Errorf("date ISO : %v", body["deployedAt"])
	}
	if checks := body["checks"].([]any); len(checks) != 1 || checks[0].(map[string]any)["name"] != "Redis" {
		t.Errorf("contrôles : %v", body["checks"])
	}
	if crons, ok := body["crons"].([]any); !ok || len(crons) != 0 {
		t.Errorf("tâches vides en tableau : %v", body["crons"])
	}
	for _, step := range []struct {
		reply string
		want  int
	}{{`{"intervalSeconds":3}`, 10}, {`{"intervalSeconds":99999}`, 3600}, {`{"intervalSeconds":null}`, 3600}} {
		fake.set(202, step.reply)
		r.reportNow(context.Background())
		if r.interval != step.want {
			t.Errorf("%s : intervalle %d", step.reply, r.interval)
		}
	}
	bare, _ := reporterFor(t, fake.URL, Options{Send: []string{"logs"}}, nil)
	bare.AddProbe(okProbe("Redis", "redis"))
	bare.reportNow(context.Background())
	last := fake.received()[len(fake.received())-1].body
	if _, has := last["frameworkVersion"]; has || len(last["checks"].([]any)) != 0 {
		t.Errorf("sans versions ni contrôles : %v", last)
	}
}

func TestRefusals(t *testing.T) {
	fake := newFakePulse(t, 401, `{}`)
	r, said := reporterFor(t, fake.URL, Options{}, nil)
	if r.reportNow(context.Background()) != nil || said.last() != "Clé Pulse refusée : vérifie PULSE_KEY. Les rapports s’arrêtent." || r.ctx.Err() == nil {
		t.Errorf("clé refusée : %q", said.last())
	}
	for code, want := range map[string]string{
		"seal_key_mismatch": "PULSE_PUBLIC_KEY ne correspond plus à la clé de l’app : recopie-la depuis l’app Pulse.",
		"secure_mode_off":   "Le mode sécurisé est coupé pour cette app : retire PULSE_PUBLIC_KEY ou réactive-le dans l’app.",
		"autre":             "Rapport refusé par Pulse.",
	} {
		refused := newFakePulse(t, 409, `{"code":"`+code+`"}`)
		r, said := reporterFor(t, refused.URL, Options{}, nil)
		if r.reportNow(context.Background()) != nil || said.last() != want || r.ctx.Err() != nil {
			t.Errorf("%s : %q", code, said.last())
		}
	}
	required := newFakePulse(t, 409, `{"code":"secure_mode_required"}`)
	r, said = reporterFor(t, required.URL, Options{}, nil)
	r.reportNow(context.Background())
	if !strings.HasSuffix(said.last(), "Les rapports en clair s’arrêtent.") || r.ctx.Err() == nil {
		t.Errorf("mode sécurisé exigé : %q", said.last())
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	offline, said := reporterFor(t, closed.URL, Options{}, nil)
	offline.reportNow(context.Background())
	offline.reportNow(context.Background())
	if lines := said.all(); len(lines) != 1 || !strings.HasPrefix(lines[0], "Pulse injoignable : dial tcp") {
		t.Errorf("réseau coupé, un seul message : %v", lines)
	}
	broken := newFakePulse(t, 500, `{}`)
	failing, said := reporterFor(t, broken.URL, Options{}, nil)
	failing.reportNow(context.Background())
	if said.last() != "Pulse injoignable : réponse 500" {
		t.Errorf("erreur serveur : %q", said.last())
	}
}

func TestRhythm(t *testing.T) {
	if reportDelay(60, false) != time.Minute || reportDelay(60, true) != 5*time.Second || reportDelay(10, true) != 5*time.Second {
		t.Error("rythme normal et en direct")
	}
	defer func(first, live time.Duration) { firstReportDelay, liveInterval = first, live }(firstReportDelay, liveInterval)
	firstReportDelay, liveInterval = 10*time.Millisecond, 30*time.Millisecond
	fake := newFakePulse(t, 202, `{"accepted":true,"intervalSeconds":60,"live":true}`)
	r, _ := reporterFor(t, fake.URL, Options{Logs: new(bool)}, nil)
	r.start()
	deadline := time.Now().Add(2 * time.Second)
	for len(fake.received()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	r.Stop()
	count := len(fake.received())
	if count < 3 {
		t.Fatalf("en direct, les rapports se suivent : %d", count)
	}
	time.Sleep(80 * time.Millisecond)
	if len(fake.received()) != count {
		t.Error("plus rien ne part après Stop")
	}
}
