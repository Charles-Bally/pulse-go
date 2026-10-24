package pulse

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	queriesMu sync.Mutex
	queries   []string
)

type fakeDriver struct{}
type fakeConn struct{}
type fakeStmt struct{}
type fakeRows struct{ done bool }

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }
func (fakeConn) Prepare(query string) (driver.Stmt, error) {
	queriesMu.Lock()
	defer queriesMu.Unlock()
	queries = append(queries, query)
	return fakeStmt{}, nil
}
func (fakeConn) Close() error                               { return nil }
func (fakeConn) Begin() (driver.Tx, error)                  { return nil, errors.New("lecture seule") }
func (fakeStmt) Close() error                               { return nil }
func (fakeStmt) NumInput() int                              { return 0 }
func (fakeStmt) Exec([]driver.Value) (driver.Result, error) { return nil, errors.New("lecture seule") }
func (fakeStmt) Query([]driver.Value) (driver.Rows, error)  { return &fakeRows{}, nil }
func (*fakeRows) Columns() []string                         { return []string{"one"} }
func (*fakeRows) Close() error                              { return nil }
func (rows *fakeRows) Next(dest []driver.Value) error {
	if rows.done {
		return io.EOF
	}
	rows.done, dest[0] = true, int64(1)
	return nil
}

func init() { sql.Register("pulse-fake", fakeDriver{}) }

type fakeStatus struct{ err error }
type fakeRedis struct{ err error }
type fakeReadPref struct{}
type fakeMongo struct{}
type fakeGorm struct{ db *sql.DB }
type fakeQueueInfo struct{ Pending int }
type fakeInspector struct{}

func (s *fakeStatus) Err() error                             { return s.err }
func (f *fakeRedis) Ping(context.Context) *fakeStatus        { return &fakeStatus{f.err} }
func (*fakeMongo) Ping(context.Context, *fakeReadPref) error { return nil }
func (g *fakeGorm) DB() (*sql.DB, error)                     { return g.db, nil }
func (*fakeInspector) Queues() ([]string, error)             { return []string{"emails", "default"}, nil }
func (*fakeInspector) GetQueueInfo(queue string) (*fakeQueueInfo, error) {
	if queue == "emails" {
		return nil, errors.New("redis down")
	}
	return &fakeQueueInfo{Pending: 3}, nil
}

type fakeSpec struct{ Second, Minute, Hour, Dom, Month, Dow uint64 }
type fakeDelay struct{ Delay time.Duration }
type fakeFuncJob func()
type fakeEntry struct {
	ID       int
	Schedule any
	Next     time.Time
	Prev     time.Time
	Job      any
}
type fakeCron struct{ entries []fakeEntry }
type fakeGocronJob interface {
	Name() string
	NextRun() (time.Time, error)
	LastRun() (time.Time, error)
}
type fakeTask struct {
	name string
	next time.Time
}
type fakeScheduler struct{ jobs []fakeGocronJob }

func (f fakeFuncJob) Run()                     { f() }
func (c *fakeCron) Entries() []fakeEntry       { return c.entries }
func (j fakeTask) Name() string                { return j.name }
func (j fakeTask) NextRun() (time.Time, error) { return j.next, nil }
func (j fakeTask) LastRun() (time.Time, error) { return time.Time{}, errors.New("jamais lancée") }
func (s *fakeScheduler) Jobs() []fakeGocronJob { return s.jobs }
func nettoyageNuit()                           {}

func bits(low, high, step int) uint64 {
	var mask uint64
	for value := low; value <= high; value += step {
		mask |= 1 << uint(value)
	}
	return mask
}

func openFake(t *testing.T) *sql.DB {
	db, err := sql.Open("pulse-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDetection(t *testing.T) {
	queriesMu.Lock()
	queries = nil
	queriesMu.Unlock()
	db := openFake(t)
	star := uint64(1) << 63
	next := time.Date(2026, 10, 22, 9, 5, 0, 0, time.UTC)
	cron := &fakeCron{entries: []fakeEntry{
		{ID: 1, Next: next, Job: Job("relance-abonnements", func() error { return nil }),
			Schedule: &fakeSpec{Second: 1, Minute: bits(0, 59, 5), Hour: bits(0, 23, 1), Dom: bits(1, 31, 1) | star, Month: bits(1, 12, 1), Dow: bits(0, 6, 1) | star}},
		{ID: 2, Job: fakeFuncJob(nettoyageNuit), Schedule: fakeDelay{Delay: time.Minute}},
		{ID: 3, Job: fakeFuncJob(func() {}), Schedule: &fakeSpec{Second: 1, Minute: 1, Hour: bits(8, 18, 1), Dom: bits(1, 31, 1), Month: bits(1, 12, 1), Dow: bits(1, 5, 1)}},
	}}
	scheduler := &fakeScheduler{jobs: []fakeGocronJob{fakeTask{name: "export-factures", next: next}}}
	found := detect([]any{db, db, openFake(t), &fakeGorm{db: openFake(t)}, &fakeRedis{}, &fakeMongo{}, &fakeInspector{},
		cron, scheduler, struct{}{}, nil, (*fakeRedis)(nil)})
	names := []string{}
	for _, probe := range found.probes {
		names = append(names, probe.Name+"/"+probe.Kind+"/"+probe.Engine)
	}
	want := "Base de données/database/sql,Base de données (2)/database/sql,Base de données (3)/database/sql,Redis/redis/redis,MongoDB/database/mongodb"
	if strings.Join(names, ",") != want || len(found.queues) != 1 || len(found.crons) != 2 {
		t.Fatalf("détection : %v, %d files, %d planificateurs", names, len(found.queues), len(found.crons))
	}
	if err := found.probes[0].Run(context.Background()); err != nil || joinedQueries() != "SELECT 1" {
		t.Errorf("SELECT 1 seulement : %v %v", err, joinedQueries())
	}
	if err := detect([]any{&fakeRedis{err: errors.New("NOAUTH")}}).probes[0].Run(context.Background()); err == nil || err.Error() != "NOAUTH" {
		t.Errorf("erreur de Redis : %v", err)
	}
	for path, engine := range map[string]string{
		"github.com/jackc/pgx/v5/stdlib": "postgresql", "github.com/lib/pq": "postgresql", "github.com/go-sql-driver/mysql": "mysql",
		"github.com/mattn/go-sqlite3": "sqlite", "modernc.org/sqlite": "sqlite", "github.com/microsoft/go-mssqldb": "sqlserver",
	} {
		if got := engineForPackage(path); got != engine || engineLabel(got) == "Base de données" {
			t.Errorf("%s : %s", path, got)
		}
	}

	fake := newFakePulse(t, 202, `{"accepted":true}`)
	r, _ := reporterFor(t, fake.URL, Options{Watch: []any{&fakeInspector{}, cron, scheduler}}, nil)
	rep := r.buildReport(context.Background())
	checks := []string{}
	for _, check := range rep.Checks {
		checks = append(checks, fmt.Sprintf("%s:%v:%s", check.Name, check.OK, check.Error))
	}
	if strings.Join(checks, ",") != "File default:true:,File emails:false:redis down" {
		t.Errorf("files asynq : %v", checks)
	}
	crons := []string{}
	for _, entry := range rep.Crons {
		crons = append(crons, entry.Name+"|"+entry.Schedule+"|"+entry.NextRunAt+"|"+entry.LastRunAt)
	}
	wantCrons := "relance-abonnements|*/5 * * * *|2026-10-22T09:05:00.000Z|,nettoyageNuit|@every 1m0s||,cron 3|0 8-18 * * 1-5||,export-factures||2026-10-22T09:05:00.000Z|"
	if strings.Join(crons, ",") != wantCrons {
		t.Errorf("tâches planifiées :\n%v", strings.Join(crons, ","))
	}
}

func TestFramework(t *testing.T) {
	info := &debug.BuildInfo{Deps: []*debug.Module{{Path: "github.com/redis/go-redis/v9", Version: "v9.7.0"}, {Path: "github.com/labstack/echo/v4", Version: "v4.13.3"}}}
	if name, version := detectFramework(info, "go1.25.3"); name != "echo" || version != "4.13.3" {
		t.Errorf("echo : %s %s", name, version)
	}
	if name, version := detectFramework(&debug.BuildInfo{}, "go1.25.3"); name != "go" || version != "1.25.3" {
		t.Errorf("net/http seul : %s %s", name, version)
	}
}

func TestLogLines(t *testing.T) {
	cases := []struct{ line, level, context, message string }{
		{`{"time":"2026-10-22T08:00:00Z","level":"WARN","msg":"lent","logger":"http"}`, "WARN", "http", "lent"},
		{`{"level":50,"msg":"pino"}`, "ERROR", "", "pino"},
		{`time=2026-10-22T08:00:00.000Z level=ERROR msg="Paiement refusé token=abc" component=paiements`, "ERROR", "paiements", "Paiement refusé token=***"},
		{"\x1b[31mERROR+4\x1b[0m arrêt brutal", "FATAL", "", "arrêt brutal"},
		{"2026/10/22 10:00:00 WARN disque presque plein", "WARN", "", "disque presque plein"},
		{"2026/10/22 10:00:00 connexion postgres://helio:secret@db/helio", "INFO", "", "connexion postgres://***@db/helio"},
		{"panic: runtime error", "FATAL", "", "panic: runtime error"},
		{"warning: certificat bientôt expiré", "WARN", "", "warning: certificat bientôt expiré"},
	}
	for _, c := range cases {
		entry, ok := parseLine(c.line)
		if !ok || entry.Level != c.level || entry.Context != c.context || entry.Message != c.message {
			t.Errorf("%q : %+v", c.line, entry)
		}
	}
	if entry, _ := parseLine(cases[0].line); entry.At != "2026-10-22T08:00:00.000Z" {
		t.Errorf("heure lue : %s", entry.At)
	}
	for _, line := range []string{"  \n", "time=x level=WARN msg=\"Pulse injoignable\" component=pulse"} {
		if _, ok := parseLine(line); ok {
			t.Errorf("ligne ignorée : %q", line)
		}
	}
	buffer := &collector{}
	for index := range 1005 {
		buffer.push(newEntry(time.Time{}, "INFO", "", fmt.Sprint(index)))
	}
	if buffer.size() != 1000 || buffer.take(1)[0].Message != "5" {
		t.Error("le tampon garde les 1000 entrées les plus récentes")
	}
}

func TestLogCapture(t *testing.T) {
	fake := newFakePulse(t, 202, `{"accepted":true,"logs":true}`)
	r, _ := reporterFor(t, fake.URL, Options{}, map[string]string{"PULSE_LOG_LEVEL": "info"})
	var appOutput, stdOutput bytes.Buffer
	logger := slog.New(r.SlogHandler(slog.NewTextHandler(&appOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r.cfg.logger = logger
	std := log.New(r.Writer(&stdOutput), "", log.LstdFlags)
	logger.Error("avant la réponse")
	if r.collector.size() != 0 {
		t.Fatal("rien n'est capturé avant logs: true")
	}
	r.reportNow(context.Background())
	logger.With("component", "paiements").Error("Paiement refusé token=abc123", "order", 42)
	logger.WithGroup("http").Warn("lent", "ms", 900)
	logger.Debug("bavard")
	r.cfg.warn("Pulse injoignable : essai")
	std.Print("WARN disque presque plein")
	if !strings.Contains(appOutput.String(), "token=abc123") || !strings.Contains(appOutput.String(), "bavard") ||
		!strings.Contains(stdOutput.String(), "disque presque plein") {
		t.Error("la sortie de l'app reste intacte")
	}
	if sent := r.flushLogs(context.Background()); sent != 3 {
		t.Fatalf("envoyées : %d", sent)
	}
	request := fake.received()[1]
	entries := []string{}
	for _, raw := range request.body["entries"].([]any) {
		entry := raw.(map[string]any)
		entries = append(entries, fmt.Sprintf("%v|%v|%v", entry["level"], entry["context"], entry["message"]))
	}
	want := "ERROR|paiements|Paiement refusé token=*** order=42,WARN|<nil>|lent http.ms=900,WARN|<nil>|disque presque plein"
	if request.path != "/ingest/logs" || strings.Join(entries, ",") != want {
		t.Errorf("journaux envoyés : %s %v", request.path, entries)
	}
	fake.set(202, `{"logs":false,"live":false}`)
	logger.Error("dernier")
	r.flushLogs(context.Background())
	logger.Error("après")
	if r.capturing() || r.collector.size() != 0 {
		t.Error("logs: false coupe la capture")
	}
	r.adoptState(true, false)
	fake.set(500, `{}`)
	logger.Error("réessayé")
	if r.flushLogs(context.Background()) != 0 || r.collector.size() != 1 {
		t.Error("un échec remet les entrées dans le tampon")
	}
	fake.set(401, `{}`)
	r.flushLogs(context.Background())
	r.adoptState(true, false)
	if r.capturing() {
		t.Error("une clé refusée arrête les journaux")
	}
	var nilReporter *Reporter
	if nilReporter.Writer(&stdOutput) != &stdOutput || nilReporter.SlogHandler(slog.Default().Handler()) != slog.Default().Handler() {
		t.Error("sans Pulse, rien n'est enveloppé")
	}
}

func joinedQueries() string {
	queriesMu.Lock()
	defer queriesMu.Unlock()
	return strings.Join(queries, ",")
}

func TestSecureMode(t *testing.T) {
	unhex := func(text string) []byte {
		raw, err := hex.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	recipient := unhex("3948cfe0ad1ddb695d780e59077195da6c56506b027329794ab02bca80815c4d")
	info, aad := unhex("4f6465206f6e2061204772656369616e2055726e"), unhex("436f756e742d30")
	plaintext := unhex("4265617574792069732074727574682c20747275746820626561757479")
	ephemeral, _ := ecdh.X25519().NewPrivateKey(unhex("52c4a758a802cd8b936eceea314432798d5baf2d7e9235dc084ab1b9cfa2f736"))
	enc, ct, err := hpkeSeal(recipient, info, aad, plaintext, aes128GCM, ephemeral)
	if err != nil || hex.EncodeToString(enc) != "37fda3567bdbd628e88668c3c8d7e97d1d1253b6d4ea6d44c150f741f1bf4431" ||
		hex.EncodeToString(ct) != "f938558b5d72f1a23810b4be2ab4f84331acc02fc97babc53a52ae8218a355a96d8770ac83d07bea87e13c512a" {
		t.Fatalf("vecteurs RFC 9180 A.1 : %x %x %v", enc, ct, err)
	}
	opened, err := hpkeOpen(unhex("4612c550263fc8ad58375df3f557aac531d26850903e55a9f23f21d8534e8ac8"), info, aad, enc, ct, aes128GCM)
	if err != nil || string(opened) != string(plaintext) {
		t.Fatalf("ouverture A.1 : %v", err)
	}

	private, _ := ecdh.X25519().GenerateKey(rand.Reader)
	public := private.PublicKey().Bytes()
	fake := newFakePulse(t, 202, `{"accepted":true,"logs":true}`)
	r, _ := reporterFor(t, fake.URL, Options{PublicKey: "pulse_pub_" + base64.RawURLEncoding.EncodeToString(public)}, nil)
	r.AddProbe(Probe{Name: "PostgreSQL", Kind: "database", Run: func(context.Context) error { return errors.New("down") }})
	r.reportNow(context.Background())
	request := fake.received()[0]
	raw, _ := json.Marshal(request.body)
	if request.path != "/ingest/report/sealed" || strings.Contains(string(raw), "PostgreSQL") ||
		string(mustJSON(t, request.body["clear"])) != `{"status":{"ok":false,"problems":1}}` {
		t.Fatalf("rapport scellé : %s %s", request.path, raw)
	}
	var envelope sealedEnvelope
	_ = json.Unmarshal(mustJSON(t, request.body["sealed"]), &envelope)
	var opened256 report
	if err := openJSON(private.Bytes(), public, "report", envelope, &opened256); err != nil || opened256.Checks[0].Name != "PostgreSQL" {
		t.Fatalf("aller-retour AES-256-GCM : %v", err)
	}
	if envelope.V != 1 || envelope.Kid != keyID(public) || len(envelope.Kid) != 16 {
		t.Errorf("enveloppe : %+v", envelope)
	}
	if err := openJSON(private.Bytes(), public, "logs", envelope, &opened256); err == nil {
		t.Error("un rapport ne s'ouvre pas comme des journaux")
	}
	r.capture(func() (logEntry, bool) { return newEntry(time.Time{}, "ERROR", "", "secret"), true })
	r.flushLogs(context.Background())
	logs := fake.received()[1]
	if logs.path != "/ingest/logs/sealed" || logs.body["count"] != float64(1) || logs.body["at"] == "" || strings.Contains(string(mustJSON(t, logs.body)), "secret") {
		t.Errorf("journaux scellés : %v", logs.body)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRelayPinning(t *testing.T) {
	relay := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	relay.Config.ErrorLog = log.New(io.Discard, "", 0)
	relay.StartTLS()
	defer relay.Close()
	good := formatFingerprint(relay.Certificate().Raw)
	r, _ := reporterFor(t, relay.URL, Options{RelayFingerprint: strings.ToLower(good)}, nil)
	if r.reportNow(context.Background()) == nil {
		t.Fatal("le relais épinglé répond")
	}
	wrong := strings.TrimSuffix(strings.Repeat("00:", 32), ":")
	pinned, said := reporterFor(t, relay.URL, Options{RelayFingerprint: wrong}, nil)
	if pinned.reportNow(context.Background()) != nil || !strings.Contains(said.last(), "certificat du relais inattendu : vérifie PULSE_RELAY_FINGERPRINT") {
		t.Errorf("empreinte différente : %q", said.last())
	}
}
