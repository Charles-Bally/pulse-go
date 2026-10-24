# pulse-go

Ton app Go surveillée par [Pulse](https://pulse-app.si), sur ton iPhone et ton Mac. Tu démarres Pulse en une ligne, tu lui montres ta base, ton Redis, tes files et ton planificateur, et il te prévient dès que quelque chose cloche.

```
  ton app Go                                              ton iPhone, ton Mac
 ┌─────────────────────────┐        HTTPS        ┌──────────┐       ┌──────────────┐
 │ pulse.Start             │ ──────────────────▶ │  Pulse   │ ────▶ │  app Pulse   │
 │  ├ PostgreSQL  SELECT 1 │   état, tâches,     │  (API)   │ push  │  alertes,    │
 │  ├ Redis       PING     │   journaux          │          │ ◀──── │  journaux,   │
 │  ├ asynq       compteurs│   (chiffrés si tu   │          │  SSE  │  en direct   │
 │  └ cron        passages │    le veux)         └──────────┘       └──────────────┘
 └─────────────────────────┘
```

## En deux minutes

```bash
go get pulse-app.si/go@latest
```

Mets la clé que l'app Pulse t'affiche quand tu ajoutes une app dans l'environnement du processus (`PULSE_KEY=pk_live_…`, par ton `.env`, ton unité systemd ou ton Docker Compose), puis au démarrage :

```go
import pulse "pulse-app.si/go"

func main() {
	db, _ := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	cache := redis.NewClient(&redis.Options{Addr: "localhost:6379"})

	reporter := pulse.Start(pulse.Options{Watch: []any{db, cache}})
	defer reporter.Stop()

	// … ton serveur
}
```

Relance ton app : le premier signal arrive dans Pulse dans les secondes qui suivent. Sans `PULSE_KEY`, `pulse.Start` rend `nil`, prévient une fois et ton app tourne comme avant : toutes les méthodes acceptent un rapporteur `nil`.

### net/http

Rien de plus à faire : `pulse.Start` au début de `main`, `Stop` à l'arrêt. Pulse ne touche ni à tes routes ni à ton serveur.

### gin, echo, chi, fiber

Même ligne, au même endroit. Pulse lit les modules liés à ton binaire et affiche le framework et sa version (`gin 1.10.0`, `echo 4.13.3`…) ; sans framework, il affiche la version de Go.

```go
router := gin.Default()
reporter := pulse.Start(pulse.Options{Watch: []any{db, cache}})
defer reporter.Stop()
router.Run(":8080")
```

Un seul rapporteur tourne par processus : un second `pulse.Start` rend celui qui tourne déjà, et `pulse.Default()` le retrouve depuis n'importe quel paquet.

## Ce que Pulse surveille

Pulse reconnaît les objets que tu lui passes dans `Watch` par leurs méthodes, sans importer leurs bibliothèques.

| Objet passé dans `Watch` | Ce que Pulse fait | Nom affiché |
|---|---|---|
| `*sql.DB` (pq, pgx, go-sql-driver/mysql, sqlite3, modernc sqlite, mssql, go-ora) | `PingContext` puis `SELECT 1` | PostgreSQL, MySQL, SQLite… |
| `*gorm.DB`, `*sqlx.DB` | la même chose sur la base sous-jacente | selon le pilote |
| `*pgxpool.Pool`, `*pgx.Conn` | `Ping` | PostgreSQL |
| go-redis (`*redis.Client`, `*redis.ClusterClient`, `*redis.Ring`) | `PING` | Redis |
| `*mongo.Client` | `Ping` | MongoDB |
| `*asynq.Inspector` | lit chaque file (`GetQueueInfo`) | File emails, File default… |
| robfig/cron v3 (`*cron.Cron`) | planning, dernier et prochain passage | le nom de la tâche |
| gocron (`Scheduler`) | nom, dernier et prochain passage | le nom de la tâche |

Chaque contrôle a 5 secondes pour répondre et ne fait que lire. Une dépendance en échec, une tâche qui a planté ou qui a sauté son passage de plus de 5 minutes, et l'app passe en « dégradée » dans Pulse avec une notification qui dit quoi. Deux bases du même moteur s'affichent `PostgreSQL` et `PostgreSQL (2)`.

Pour une file ou une dépendance que Pulse ne connaît pas, ajoute ta propre sonde :

```go
pulse.Start(pulse.Options{
	Watch: []any{db},
	Probes: []pulse.Probe{
		pulse.HTTPProbe("Route /health", "http://localhost:8080/health"),
		{Name: "File factures", Kind: "queue", Engine: "nats", Run: func(ctx context.Context) error {
			_, err := js.StreamInfo("factures", nats.Context(ctx))
			return err
		}},
	},
})
```

`HTTPProbe` fait un `GET` sans suivre les redirections et échoue à partir d'un statut 500. `Kind` vaut `database`, `redis`, `cache`, `queue`, `http` ou `custom`.

### Tâches planifiées

Pour qu'une tâche en échec remonte (un planificateur ne sait que quand elle a tourné, pas si elle a réussi), donne-lui un nom avec `pulse.Job` :

```go
scheduler := cron.New()
scheduler.AddJob("0 * * * *", pulse.Job("relance-abonnements", relancerAbonnements))
scheduler.Start()

pulse.Start(pulse.Options{Watch: []any{db, scheduler}})
```

Ailleurs (gocron, une boucle à toi, une route appelée par un cron système), enveloppe le travail :

```go
err := pulse.Track("relance-abonnements", relancerAbonnements)
err = pulse.TrackContext(ctx, "export-factures", exporterFactures)
```

L'erreur (ou la panique) est notée dans Pulse puis te revient telle quelle. `reporter.RecordCronRun(nom, ok, err)` fait la même chose à la main.

## Choisir ce qui part

Tu décides de ce que ton app raconte à Pulse. Le signal de vie part toujours, le reste se règle :

```bash
# Seulement l'état des dépendances et les journaux, pas les tâches ni les versions
PULSE_SEND=checks,logs

# Ignorer certaines instances ou certaines tâches (* sert de joker)
PULSE_EXCLUDE=File emails,nettoyage-*

# Ou au contraire, ne suivre que celles-ci
PULSE_INCLUDE=PostgreSQL,Redis,relance-abonnements

# Journaux : seulement les avertissements et les erreurs
PULSE_LOG_LEVEL=WARN
```

Les mêmes réglages existent dans le code, ils passent avant les variables :

```go
pulse.Start(pulse.Options{
	Send:     []string{"checks", "crons", "logs"},
	Exclude:  []string{"File emails"},
	LogLevel: "WARN",
	Watch:    []any{db, cache},
})
```

## Journaux et mode direct (Pro et Team)

Pulse recopie ce que ton app écrit avec `log/slog` ou le paquet `log`, sans rien changer à ce que tu vois dans ton terminal. Le niveau et le contexte (`component` ou `logger`) sont reconnus, les attributs gardés, et les secrets masqués avant de partir (`token=***`, `Bearer ***`, identifiants dans les URL de connexion).

```go
slog.SetDefault(slog.New(pulse.NewSlogHandler(slog.NewJSONHandler(os.Stdout, nil))))

// ou, pour le paquet log
log.SetOutput(reporter.Writer(os.Stderr))
```

```
level=ERROR msg="Paiement refusé token=abc123" component=paiements order=42
                                   │
                                   ▼  dans l'app Pulse
ERROR  paiements  Paiement refusé token=*** order=42
```

`pulse.NewSlogHandler` se branche avant ou après `pulse.Start` ; `reporter.SlogHandler(next)` fait la même chose pour un rapporteur précis. Les messages de Pulse lui-même passent par `slog.Default()` (ou `Options.Logger`) et ne sont jamais renvoyés à Pulse.

Quand tu ouvres l'app dans Pulse, le serveur prévient le paquet qu'on le regarde : l'état part toutes les 5 secondes et les journaux chaque seconde, puis tout revient au calme 30 secondes après que tu as quitté l'écran. Les journaux sont gardés 3 jours en Pro et 7 jours en Team. Sans journaux dans ton offre, rien n'est capturé : ton app ne paie aucun surcoût.

## Mode sécurisé : Pulse ne peut rien lire

C'est l'option pour qui ne veut confier ses données à personne, nous compris. Ton iPhone ou ton Mac fabrique une paire de clés. La clé publique va dans ton environnement, la clé privée ne quitte jamais tes appareils.

```bash
PULSE_PUBLIC_KEY=pulse_pub_…   # copiée depuis l'app, Réglages de l'app > Mode sécurisé
```

```
 ton app ──── rapport chiffré (HPKE, X25519 + AES-256-GCM) ────▶ Pulse ──▶ ton iPhone
                                                                 │          │
                                                    garde un paquet         ouvre avec
                                                    qu'il ne sait pas       la clé privée
                                                    ouvrir                  du trousseau
```

Un serveur qui ne lit rien ne peut pas alerter. C'est pour ça qu'un petit résumé reste en clair par défaut : « tout va bien » ou « 2 soucis », sans nom ni message. La notification dit alors simplement « 2 soucis signalés, le détail est chiffré ». Tu peux en laisser plus, ou rien du tout :

```bash
PULSE_CLEAR=status            # par défaut : un booléen et un nombre
PULSE_CLEAR=status,checks     # Pulse peut dire « Redis ne répond plus »
PULSE_CLEAR=none              # silence complet : Pulse ne sait plus que si l'app est en vie
```

Une fois le mode actif, Pulse refuse tout rapport en clair : un `.env` oublié ne fuit pas. Le chiffrement n'utilise que la bibliothèque standard de Go (`crypto/ecdh`, `crypto/aes`).

## Sécurité, en clair

- **Ton app d'abord.** Le paquet ne panique jamais et ne renvoie aucune erreur. Pulse injoignable : un seul avertissement, ton app continue. Clé refusée : les envois s'arrêtent. Les rapports tournent dans une goroutine qui ne retient pas l'arrêt de ton programme ; `Stop()` attend proprement la fin de l'envoi en cours.
- **Rien n'est écrit chez toi.** Les contrôles ne font que lire (`SELECT 1`, `PING`, compteurs de file), avec 5 secondes au plus.
- **Rien de superflu ne part.** Ni requête, ni donnée métier, ni variable d'environnement. Les messages d'erreur sont nettoyés et coupés à 300 caractères.
- **La clé ne voyage qu'en HTTPS**, dans un en-tête, jamais dans une URL. Elle ne sert qu'à écrire l'état de cette app : elle n'ouvre pas ton compte. Tu peux la remplacer depuis l'app à tout moment.
- **Aucune dépendance** : seulement la bibliothèque standard de Go (1.22 ou plus récent).

## Variables

| Variable | Défaut | Rôle |
|---|---|---|
| `PULSE_KEY` | (obligatoire) | clé de l'app : `pk_live_…` en production, `pk_test_…` en staging |
| `PULSE_SEND` | `checks,crons,logs,versions` | ce qui part en plus du signal de vie |
| `PULSE_INCLUDE` | (tout) | seuls ces contrôles et tâches, noms séparés par des virgules, `*` permis |
| `PULSE_EXCLUDE` | (rien) | contrôles et tâches ignorés |
| `PULSE_LOG_LEVEL` | `DEBUG` | niveau minimal des journaux envoyés |
| `PULSE_LOGS` | `true` | `false` coupe les journaux |
| `PULSE_PUBLIC_KEY` | (aucune) | active le mode sécurisé |
| `PULSE_CLEAR` | `status` | en mode sécurisé, ce qui reste lisible par Pulse |
| `PULSE_ENDPOINT` | `https://api.pulse-app.si` | ton Pulse Relay en mode VPS perso (Team), par exemple `https://relais.helio.ci` |
| `PULSE_RELAY_FINGERPRINT` | (aucune) | empreinte du certificat d'un relais sans domaine : rien ne part si elle ne correspond pas |
| `PULSE_APP_VERSION` | version du module principal | version affichée dans Pulse |
| `PULSE_DISABLED` | `false` | `true` coupe tout (tests, CI) |

Chaque variable a son option dans `pulse.Options` (`Key`, `Send`, `Include`, `Exclude`, `LogLevel`, `Logs`, `PublicKey`, `Clear`, `Endpoint`, `RelayFingerprint`, `AppVersion`, `Disabled`) ; une option vide laisse la variable s'appliquer.

## Si rien n'arrive

| Message | Ce qu'il faut faire |
|---|---|
| `PULSE_KEY absente` | la variable n'est pas dans l'environnement du processus : vérifie ton `.env`, ton unité systemd ou ton Compose |
| `Clé Pulse refusée` | la clé a été remplacée dans l'app : recopie la nouvelle |
| `Mode sécurisé actif pour cette app` | ajoute `PULSE_PUBLIC_KEY` |
| `PULSE_PUBLIC_KEY ne correspond plus` | la clé a changé sur ton téléphone : recopie la clé publique |
| `Pulse injoignable` | réseau ou pare-feu : l'app continue, les envois reprennent seuls |

Le rythme des rapports suit celui de l'app dans Pulse (30 secondes en Pro, 5 minutes en Gratuit).

## Publication

Le chemin `pulse-app.si/go` renvoie la commande go vers le miroir public `github.com/Charles-Bally/pulse-go`. `pnpm release go` monte `pulse.Version` et y pousse la nouvelle version (ADR-081, ADR-083).

## Licence

MIT
