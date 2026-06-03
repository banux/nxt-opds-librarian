# librarian

Agent Go autonome qui pilote une ou plusieurs bibliothèques [nxt-opds](https://github.com/banux/nxt-opds)
via leur serveur MCP : enrichissement automatique des métadonnées (tags, résumé,
classification d'âge, intensité du piment, séries…), réponses conversationnelles
sur le catalogue, exécution de gros batches de maintenance.

Un seul binaire `librarian` gère plusieurs instances nxt-opds. Chacune est
configurée via un appairage à code one-time, sans copier-coller de secrets.

---

## Sommaire

- [Sous-commandes](#sous-commandes)
- [Installation rapide](#installation-rapide)
- [Appairage avec une instance nxt-opds](#appairage)
- [Configuration YAML](#configuration-yaml)
- [Backends LLM](#backends-llm)
- [Daemon (`serve`)](#daemon-serve)
- [Maintenance en lot (`batch`)](#maintenance-en-lot-batch)
- [Run one-shot (`run`)](#run-one-shot-run)
- [Docker / docker-compose](#docker--docker-compose)
- [Logs](#logs)
- [Architecture interne](#architecture-interne)
- [Self-update](#self-update)

---

## Sous-commandes

| Commande   | Quand l'utiliser |
|------------|------------------|
| `pair`     | Associer ce librarian à une instance nxt-opds via un code généré dans l'admin UI |
| `unpair`   | Dissocier proprement une instance des deux côtés |
| `serve`    | Faire tourner le daemon longue durée (chat, webhooks book-event, ticker, heartbeat) |
| `batch`    | Itérer une maintenance déterministe (pagination en Go, pas en LLM) — idéal pour « traite tous les 16+ » |
| `run`      | Exécution one-shot pilotée par LLM (recherche par titre, prompt ad-hoc) |
| `update`   | Self-update vers la dernière release GitHub |
| `version`  | Affiche la version installée |

`librarian help` pour l'aide complète, `librarian <cmd> --help` pour les flags d'une commande.

---

## Installation rapide

```bash
go install github.com/banux/librarian-agent/cmd/librarian@latest
# ou télécharger la release pré-compilée :
# https://github.com/banux/nxt-opds-librarian/releases
```

Vérifier :

```bash
librarian version
```

---

## Appairage

L'appairage utilise un code one-time généré depuis l'UI admin nxt-opds.
Aucun secret ne transite par la ligne de commande ; les `chat_secret` et
`webhook_secret` sont négociés entre les deux services pendant l'échange.

### Étapes

1. Dans l'admin nxt-opds : cliquer **« Associer un librarian »**. Un code
   `XXXX-XXXX` valide 10 minutes s'affiche.
2. Sur la machine du librarian :

   ```bash
   librarian pair \
     --nxt-opds https://books.example.com \
     --code     K4Q9-PN2X \
     --name     example \
     --label    "Bibliothèque Example"
   ```

   Le YAML `~/.config/librarian/config.yaml` est créé / mis à jour ; nxt-opds
   stocke `librarian_url` côté DB. La chat box devient active immédiatement.

### Flags utiles

| Flag | Description |
|------|-------------|
| `--librarian-url <url>` | URL publique du librarian que nxt-opds doit appeler. Défaut : déduit du champ `listen` du YAML (`http://localhost:8080`) ou de `public_url`. |
| `--rotate`              | Régénère `chat_secret` + `webhook_secret` sans nouveau code (utilise le chat_secret actuel pour s'authentifier). |
| `--force`               | Écrase une association existante côté nxt-opds (sinon 409). |
| `--print-only`          | N'écrit rien, affiche juste les blocs YAML à copier-coller. |

### Dissocier

```bash
librarian unpair --name example --nxt-opds https://books.example.com
```

Efface l'entrée du YAML local et appelle (best-effort) nxt-opds pour
nettoyer l'association côté DB.

---

## Configuration YAML

Résolution : `--config <path>` > `LIBRARIAN_CONFIG` env > `./librarian.yaml`
> `~/.config/librarian/config.yaml`.

```yaml
listen: ":8080"           # adresse d'écoute HTTP du daemon
public_url: "http://librarian.lan:8080"   # URL annoncée à nxt-opds (sinon dérivé de listen)
interval: "6h"            # cadence du ticker en mode serve
batch_limit: 10           # nb de livres traités par tick
max_steps: 200            # plafond d'étapes par job
backend: "auto"           # auto | ollama | anthropic
model: ""                 # nom du modèle (sinon défaut du backend)
ollama_url: "http://localhost:11434"   # endpoint Ollama ; surchargé par OLLAMA_HOST / --ollama-url
default_instance: "example"   # utilisée quand --instance est omis

# Optionnel : clé Google Books — active l'outil google_books_search et le
# place en priorité 1 (avant Babelio / sites éditeurs) pour la recherche de
# métadonnées. Surchargée par la variable d'env GOOGLE_BOOKS_API_KEY.
# google_books_api_key: "AIza..."

instances:
  - name: "example"                 # slug [a-z0-9-]+, unique
    mcp_url: "https://books.example.com/mcp"
    mcp_token: "<opds_token>"       # injecté par `pair`
    chat_secret: "<64-hex>"         # idem
    webhook_secret: "<64-hex>"      # idem
    label: "Bibliothèque Example"
    locale: "fr"
```

Tout secret est en clair dans le fichier — perms `0600` appliquées
automatiquement. Les variables d'env sont expansées (`${OPDS_TOKEN_EX}`)
au chargement.

---

## Backends LLM

- **Ollama** (défaut) — local ou Ollama Cloud. Modèle par défaut :
  `gemma4:31b-cloud`. Override : `LIBRARIAN_MODEL` ou `--model`.
- **Anthropic** (Claude) — activé si `ANTHROPIC_API_KEY` est défini OU si
  `--backend anthropic`. Plus discipliné sur les boucles longues (recommandé
  pour de très gros batches).

Variables d'env supplémentaires :

| Variable               | Rôle |
|------------------------|------|
| `LIBRARIAN_BACKEND`    | `auto` (défaut) / `ollama` / `anthropic` |
| `LIBRARIAN_MODEL`      | nom de modèle |
| `OLLAMA_HOST`          | endpoint Ollama — l'emporte sur `ollama_url` du YAML (défaut `http://localhost:11434`) |
| `ANTHROPIC_API_KEY`    | clé API Claude |
| `FIRECRAWL_API_KEY`    | clé Firecrawl pour `web_fetch` (override le YAML) |
| `GOOGLE_BOOKS_API_KEY` | clé Google Books — active l'outil `google_books_search` en source de métadonnées **prioritaire** (override le YAML) |
| `LIBRARIAN_CONFIG`     | chemin du YAML |

---

## Daemon (`serve`)

```bash
librarian serve --listen :8080 --interval 6h
```

À démarrage, le daemon :

1. Charge le YAML, instancie un *Registry* d'instances (clients MCP +
   `Agent` paresseusement initialisés).
2. **Announce** : POST `/api/librarian/announce` sur chaque nxt-opds appairé
   avec le `public_url` courant — auto-réparation après changement de
   port/hostname/docker network.
3. **Heartbeat** : ticker 60 s qui POST `/api/librarian/heartbeat` pour que
   l'admin UI nxt-opds montre la fraîcheur de la liaison.
4. **Ticker batch** : toutes les `interval` (défaut 6 h), enqueue un job
   `search_books(not_indexed:true, limit=batch_limit)` sur chaque instance.

### Routes exposées

| Route                                        | Méthode | Description |
|----------------------------------------------|---------|-------------|
| `/healthz`                                   | GET     | OK plaintext |
| `/instances`                                 | GET     | JSON public : `[{name, label, locale}]` |
| `/chat`                                      | POST    | Endpoint de chat appelé par la chat box nxt-opds. Auth : `Authorization: Bearer <chat_secret>`. Body : `{message, history, user_token?}`. Réponse JSON `{reply, error?}`. |
| `/webhooks/{instance}/book-event`            | POST    | Événements catalogue (book.created/updated/deleted/read) émis par nxt-opds. Signature `X-Signature: sha256=<hmac>` validée contre `webhook_secret`. |
| `/trigger/{instance}`                        | POST    | Trigger manuel : body JSON `{prompt}` ou texte brut. Le prompt remplace l'instruction batch par défaut. |
| `/instances/{instance}/forget`               | POST    | Appelé par nxt-opds lors d'un unpair côté UI. Auth : `Authorization: Bearer <chat_secret>`. |

### Flags clés

| Flag | Description |
|------|-------------|
| `--listen :8080`           | Adresse d'écoute (override le YAML) |
| `--interval 6h`            | Cadence du ticker |
| `--batch-limit 10`         | Nb de livres traités par tick |
| `--max-steps 500`          | Étapes max par job (200 par défaut) |
| `--job-timeout 2h`         | Timeout par job (1 h par défaut) — augmenter pour de gros batches |
| `--prompt "…"`             | Prompt remplaçant l'instruction batch par défaut du ticker |
| `--instance <name>`        | Pour `run` / `batch` ; quand plusieurs instances sont configurées |

---

## Maintenance en lot (`batch`)

`batch` est la commande à utiliser pour les **gros chantiers** : « note le
piment de tous les 16+ », « enrichi tous les non-indexés », etc. La
pagination tourne dans Go, pas dans le LLM, donc même un petit modèle ne
peut pas couper court en écrivant FIN prématurément.

```bash
librarian batch --instance example --filter age_rating_min=16
```

Cycle interne :

```
for offset := 0; ; offset += limit {
    ids := search_books(filters, limit, offset)
    if len(ids) < limit { break }
    for id := range ids {
        agent.Run(perBookPrompt(id))   // ~5-10 étapes par livre
    }
}
```

### Flags

| Flag | Description |
|------|-------------|
| `--filter k=v`            | Filtre passé à `search_books`. Répétable. Types coercés (int/bool/string). |
| `--limit 50`              | Taille de page (max 100). |
| `--offset 100`            | Reprend à partir d'un offset arbitraire. |
| `--max-books 50`          | Plafond global (0 = illimité). Utile pour valider sur un échantillon. |
| `--max-steps 60`          | Étapes max par livre (5-10 suffisent en général). |
| `--prompt "…"`            | Template par livre, `{{ID}}` est remplacé. Défaut : enrichissement complet selon le workflow standard. |
| `--retry-wait 1h`         | Pause entre 2 retries après quota / rate-limit / réseau transitoire. |
| `--max-rate-retries 6`    | Nb de pauses+retry par livre avant abandon de ce livre. |
| `--dry-run`               | Liste les IDs candidats sans invoquer l'agent. |

### Exemples

```bash
# Lister les candidats sans rien modifier
librarian batch --instance example --filter age_rating_min=16 --dry-run

# Toute la bibliothèque, en lots de 100, avec une pause d'1 h sur quota
librarian batch --instance example --filter age_rating_min=16 \
                 --limit 100 --retry-wait 1h

# Reprendre un batch interrompu (le log "interrompu" donne la valeur d'offset)
librarian batch --instance example --filter age_rating_min=16 --offset 250

# Tag spécifique
librarian batch --instance example --filter tag="Dark Romance"

# Échantillon de 10 pour valider un prompt custom
librarian batch --instance example --filter age_rating_min=16 \
                 --max-books 10 \
                 --prompt 'Pour {{ID}} : get_book puis web_fetch Babelio puis update_book(spice_rating:N, last_maintenance_at:-1). Termine par FIN.'
```

### Gestion des quotas LLM

Sur une bibliothèque de plusieurs centaines de livres, Ollama Cloud ou
Anthropic finissent par limiter. Le batch détecte automatiquement HTTP 429,
503, `rate limit`, `overloaded`, `quota`, `too many requests`, ainsi que les
glitchs réseau transitoires (`i/o timeout`, `connection reset`, `EOF`). Sur
détection :

1. log : `[batch ex] id=abc rate-limit (…) — pause 1h0m0s, reprise vers 23:42 (retry 1/6)`
2. sleep `--retry-wait` (interruptible Ctrl-C)
3. retry du **même livre**
4. au-delà de `--max-rate-retries`, le livre est abandonné et le loop continue

Ctrl-C pendant une pause : log `interrompu — reprendre avec --offset N`.

---

## Run one-shot (`run`)

Pour les invocations interactives : un livre par titre, un prompt ad-hoc.

```bash
# Cibler un livre par titre (positionnel = recherche de titre)
librarian run --instance example "Le Chevalier et la Phalène"

# Prompt verbatim
librarian run --instance example --prompt "Liste les 10 derniers livres ajoutés."

# Maintenance pilotée par LLM (alternative au `batch` si on tient à laisser
# l'agent décider de la pagination — plus erratique sur les petits modèles)
librarian run --instance example \
    --prompt "Traite TOUS les livres non indexés un par un, sans limite. Termine par FIN." \
    --max-steps 1000
```

Pour un vrai gros chantier, **préférer `batch`**.

---

## Docker / docker-compose

Image distroless ~25 MB, exposée à `:8080`, config sur volume `/config`.

```yaml
# docker-compose.yml fragment
services:
  librarian:
    image: ghcr.io/banux/nxt-opds-librarian:latest
    ports: ["8081:8080"]
    volumes: ["librarian-config:/config"]
    environment:
      LIBRARIAN_CONFIG: /config/config.yaml
      LIBRARIAN_BACKEND: anthropic
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
    restart: unless-stopped

volumes:
  librarian-config:
```

Premier lancement (paire avec un nxt-opds dans le même compose) :

```bash
docker compose run --rm librarian pair \
  --nxt-opds http://nxt-opds:8080 \
  --code XXXX-XXXX --name example \
  --librarian-url http://librarian:8080
docker compose restart librarian
```

Un `docker-compose.yml` complet (nxt-opds + librarian) est fourni dans
`../../docker-compose.yml`.

---

## Logs

Le daemon log toutes les étapes du loop agent (chat et batch/webhook/ticker) :

```
[chat example] ◀ 192.168.1.42 (scope=user, history=4): Quel est mon dernier livre ?
[chat example] tool_call search_books {"limit":1,"sort":"added_desc"}
[chat example] tool_result search_books [ok] Trouvé 689 livre(s)…
[chat example] text: Votre dernier livre ajouté est « Roi Sorcier » de Martha Wells.
[chat example] done in 1.4s (steps=2, tools=1, stop=end_turn)
[chat example] ▶ reply (78 chars, tools=1): Votre dernier livre…

[example job webhook] start: Un nouveau livre vient d'être ajouté…
[example job webhook] tool_call get_book {"id":"abc123"}
[example job webhook] tool_result get_book [ok] **Titre…
[example job webhook] done in 8.2s (tools=4)

[batch example] page offset=100 limit=50 → 50 IDs (total estimé 689)
[batch example] ▶ 101/689 id=d34638c2c8d81822
[batch example] id=d34638c2c8d81822 rate-limit (ollama 429: …) — pause 1h0m0s, reprise vers 23:42:15 (retry 1/6)
```

---

## Architecture interne

```
cmd/librarian/         # CLI : sous-commandes run/serve/batch/pair/unpair/update
internal/config/       # parse YAML, validate slugs, ResolveLibrarianURL, NxtOPDSBaseURL
internal/instances/    # Registry : map nom→Entry{Client, Agent, Lock, Jobs}, lazy init
internal/mcp/          # client MCP HTTP Streamable (JSON-RPC + SSE) ; WithBearer pour le scoping user
internal/llm/          # interface Provider + backends Ollama et Anthropic
internal/agent/        # boucle tool-calling, system prompt batch + chat, Emit hook pour SSE
internal/daemon/       # ticker + serveur HTTP + workers par instance + announce + heartbeat
internal/updater/      # self-update via GitHub releases
```

Points d'attention :

- Une seule goroutine **worker par instance** : les jobs sont sérialisés par
  instance (le `transcript` du modèle n'est pas thread-safe) mais parallèles
  **entre** instances.
- Le `Mode` de l'agent (`ModeBatch` vs `ModeChat`) choisit dynamiquement le
  system prompt pour ne pas confondre l'enrichissement autonome avec le chat
  conversationnel.
- Côté chat, `mcp.WithBearer(ctx, user_token)` injecte le token utilisateur
  du flux nxt-opds → tools per-user (`list_to_read`, `list_wishlist`, etc.)
  scopent automatiquement au compte connecté.

---

## Self-update

```bash
librarian update              # télécharge et installe la dernière release
librarian update --dry-run    # voir la version cible sans rien faire
librarian update --force      # forcer la réinstallation
```

Cible : `api.github.com/repos/banux/nxt-opds-librarian/releases/latest`.
L'asset correspondant à `GOOS-GOARCH` est rename'é atomiquement sur le
binaire courant.

### Auto-update horaire (systemd)

`deploy/systemd/` fournit un timer qui vérifie une nouvelle release **toutes
les heures** et ne redémarre le daemon **que si** une version a réellement été
installée (un `update` sans nouveauté ne coupe pas un batch en cours) :

```bash
sudo install -m 0755 deploy/systemd/librarian-autoupdate /usr/local/bin/
sudo install -m 0644 deploy/systemd/librarian-update.service /etc/systemd/system/
sudo install -m 0644 deploy/systemd/librarian-update.timer   /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now librarian-update.timer
```

Adapter le chemin du binaire ou le nom de l'unité `serve` (défauts
`/usr/local/bin/librarian` et `librarian.service`) via `Environment=` dans
`librarian-update.service`.

Vérifier / tester :

```bash
systemctl list-timers librarian-update.timer   # prochaine échéance
sudo systemctl start librarian-update.service   # forcer un cycle maintenant
journalctl -u librarian-update.service          # logs (version installée ou « déjà à jour »)
```

Le wrapper tourne en root (il écrase le binaire et appelle `systemctl`). La
vérification anonyme de l'API GitHub (60 req/h) suffit largement à une cadence
horaire.

---

## Licence

Voir LICENSE.
