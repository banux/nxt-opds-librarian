# Installation propre du stack nxt-opds + librarian — runbook agent

> Procédure destinée à un **agent** (Claude Code ou équivalent) qui doit
> installer le stack de zéro de façon idempotente. Chaque phase se termine par
> un **contrôle** : ne passe à la suite que si le contrôle passe. En cas
> d'échec, lis la section [Pièges connus](#pièges-connus) avant de réessayer.

## 1. Ce que tu installes

Deux services Go distincts, couplés par un **appairage** :

| Service | Rôle | LLM ? | Go | Repo |
|---------|------|-------|----|------|
| **nxt-opds** | Catalogue eBook + UI Vue + OPDS + endpoint **MCP** `/mcp` | non | ≥ 1.24 | `git@github.com:banux/nxt-opds.git` |
| **librarian** | Agent autonome : chat, enrichissement métadonnées, webhooks | **oui** (Anthropic ou Ollama) | ≥ 1.26.1 | `git@github.com:banux/nxt-opds-librarian.git` |

- nxt-opds **n'a pas de LLM** : le chat de l'UI est un proxy vers le librarian.
- Le librarian n'agit sur le catalogue qu'**après appairage** : il reçoit alors
  un `mcp_url` + `mcp_token` et deux secrets HMAC (`chat_secret`,
  `webhook_secret`).
- Le couplage est **obligatoire** et se fait en une fois (phase 5). Tout le
  reste s'auto-répare au redémarrage (heartbeat + announce).

```
navigateur ─▶ nxt-opds :8080 ──(/chat proxy, X-Signature HMAC)──▶ librarian :8080
                  ▲                                                     │
                  └──────────── MCP /mcp (Bearer mcp_token) ◀───────────┘
```

## 2. Choisir un mode d'installation

- **Mode A — Docker Compose** (recommandé pour un poste/serveur unique) :
  tout est dans `nxt-opds-org/docker-compose.yml`. Voir phase 4A.
- **Mode B — binaires + systemd** (prod, multi-hôtes) : nxt-opds et librarian
  tournent comme services systemd séparés, éventuellement sur des machines
  différentes. Voir phase 4B.

Les phases 3 (préflight) et 5 (appairage) → 8 sont communes.

## 3. Préflight (commun)

```bash
# Disposition de référence : les deux repos côte à côte sous nxt-opds-org/
ls nxt-opds-org/nxt-opds nxt-opds-org/nxt-opds-librarian   # doivent exister
```

**Contrôle :** les deux répertoires existent et sont des clones git à jour
(`git -C <repo> pull --ff-only`). Pour le mode A, `docker` + `docker compose`
répondent. Pour le mode B, `go version` ≥ 1.26.1.

> ⚠️ **Bloquant Docker connu** : `nxt-opds-org/docker-compose.yml` déclare
> `librarian.build.context: ./librarian/agent`, qui **n'existe pas** (le repo
> est `./nxt-opds-librarian`). Corrige avant tout `up` :
> ```bash
> sed -i 's#context: ./librarian/agent#context: ./nxt-opds-librarian#' \
>   nxt-opds-org/docker-compose.yml
> ```
> Vérifie : `grep -n context nxt-opds-org/docker-compose.yml` → `./nxt-opds-librarian`.

## 4A. Mode Docker Compose

```bash
cd nxt-opds-org
cp .env.example .env            # puis ÉDITE .env (voir ci-dessous)
mkdir -p data/books             # volume monté sur /data/books
docker compose up -d --build
```

`.env` à renseigner **avant** le `up` :

| Clé | Obligatoire | Note |
|-----|-------------|------|
| `AUTH_PASSWORD` | **oui** | remplace `changeme` — mot de passe admin nxt-opds |
| `LIBRARIAN_BACKEND` | oui | `anthropic` \| `ollama` \| `auto` |
| `ANTHROPIC_API_KEY` | si `anthropic` | clé Claude |
| `OLLAMA_HOST` | si `ollama` | défaut `http://host.docker.internal:11434` (Ollama de l'hôte) |
| `NXT_OPDS_PORT` / `LIBRARIAN_PORT` | non | défauts `8080` / `8081` |

**Contrôle :**
```bash
docker compose ps                          # nxt-opds + librarian = running
curl -fsS http://localhost:8080/api/config | grep -o '"librarianEnabled":[a-z]*'
# → "librarianEnabled":false  (normal : pas encore appairé)
```

## 4B. Mode binaires + systemd

### nxt-opds

```bash
cd nxt-opds-org/nxt-opds && go build -o nxt-opds .
sudo useradd --system --no-create-home nxt-opds 2>/dev/null || true
sudo install -d -o nxt-opds -g nxt-opds /opt/nxt-opds /var/lib/nxt-opds/books
sudo install -m 0755 nxt-opds /opt/nxt-opds/
sudo install -m 0644 nxt-opds.service /etc/systemd/system/
# Édite l'unité pour définir AUTH_PASSWORD (décommente la ligne Environment)
sudo systemctl daemon-reload && sudo systemctl enable --now nxt-opds
```

L'unité fournie (`nxt-opds.service`) utilise `BACKEND=sqlite`,
`BOOKS_DIR=/var/lib/nxt-opds/books`, durcissement systemd (`ProtectSystem`,
`ReadWritePaths=/var/lib/nxt-opds`). nxt-opds sait aussi se mettre à jour en un
clic depuis l'UI admin (releases GitHub).

### librarian

```bash
cd nxt-opds-org/nxt-opds-librarian && go build -o librarian ./cmd/librarian
sudo install -m 0755 librarian /usr/local/bin/
```

Définis le backend via une unité serve (à créer) ou des variables d'env :
`LIBRARIAN_BACKEND`, `ANTHROPIC_API_KEY` **ou** `OLLAMA_HOST`. Le YAML de config
est créé par l'appairage (phase 5) ; chemin par défaut
`~/.config/librarian/config.yaml` (ou `LIBRARIAN_CONFIG`).

**Contrôle :** `nxt-opds` répond sur `:8080`, `librarian version` affiche une
version.

## 5. Appairage (commun — étape de couplage obligatoire)

1. Ouvre l'UI nxt-opds → connexion admin (`AUTH_PASSWORD`) →
   **Administration → Librarian → Associer un librarian** → copie le code
   `XXXX-XXXX` (TTL 10 min, usage unique).
2. Lance `pair` côté librarian :

   **Docker :**
   ```bash
   cd nxt-opds-org
   docker compose run --rm librarian pair \
     --nxt-opds http://nxt-opds:8080 \
     --code XXXX-XXXX \
     --name jerinn --label "Bibliothèque Jerinn" \
     --librarian-url http://librarian:8080
   docker compose restart librarian
   ```

   **Binaire :**
   ```bash
   librarian pair --nxt-opds https://books.example \
     --code XXXX-XXXX --name jerinn --label "Bibliothèque Jerinn" \
     --librarian-url https://librarian.example
   # puis redémarre le service serve du librarian
   ```

   - `--nxt-opds` : URL **joignable depuis le librarian** (en Docker, le nom de
     service `http://nxt-opds:8080`, pas `localhost`).
   - `--librarian-url` : URL **par laquelle nxt-opds joindra le librarian** pour
     `/chat` et les webhooks (en Docker, `http://librarian:8080`).
   - `--name` : slug `[a-z0-9-]+` unique de l'instance.

**Contrôle :**
```bash
curl -fsS http://localhost:8080/api/config | grep -o '"librarianEnabled":[a-z]*'
# → "librarianEnabled":true   ➜ le chat apparaît dans l'UI
```
Pour inspecter le YAML généré (Docker) :
`docker compose exec librarian cat /config/config.yaml`.

## 6. Vérification de bout en bout (smoke test)

1. UI nxt-opds : uploade un EPUB → il apparaît dans la grille.
2. Chat de l'UI : pose « Quel est mon dernier livre ajouté ? » → réponse
   pertinente ⇒ le proxy `/chat`, l'appairage, le MCP et le LLM fonctionnent.
3. Logs librarian : tu dois voir `tool_call search_books …` puis
   `done in … (steps=…)`.

Si le chat renvoie une erreur : vérifie que `--librarian-url` est joignable
**depuis nxt-opds**, et que le backend LLM a sa clé/endpoint.

## 7. Auto-update

- **nxt-opds** : bouton de mise à jour dans l'UI admin (release GitHub).
- **librarian** : installe le timer systemd horaire (ne redémarre que si une
  nouvelle version est réellement installée — voir
  [`deploy/systemd/`](systemd/)) :
  ```bash
  cd nxt-opds-org/nxt-opds-librarian
  sudo install -m 0755 deploy/systemd/librarian-autoupdate /usr/local/bin/
  sudo install -m 0644 deploy/systemd/librarian-update.{service,timer} /etc/systemd/system/
  sudo systemctl daemon-reload && sudo systemctl enable --now librarian-update.timer
  ```
  Adapte `LIBRARIAN_SERVICE=` si l'unité serve ne s'appelle pas
  `librarian.service`. (En Docker, n'utilise pas ce timer : fais plutôt
  `docker compose pull && up -d`.)

## 8. Pièges connus

- **Contexte de build du compose** : `./librarian/agent` → `./nxt-opds-librarian`
  (corrigé en phase 3). Sans ça, `up --build` échoue.
- **Précédence Ollama** : `OLLAMA_HOST` / `--ollama-url` l'emportent **toujours**
  sur `ollama_url` du YAML (depuis v0.36 le YAML est enfin pris en compte, mais
  reste sous l'env). En Docker, `OLLAMA_HOST` pointe l'Ollama de l'**hôte** via
  `host.docker.internal` (mappé par `extra_hosts`).
- **URLs d'appairage** : en Docker, utilise les **noms de service** réseau
  (`http://nxt-opds:8080`, `http://librarian:8080`), jamais `localhost`.
- **Volume `librarian-config`** : volume nommé Docker ; l'utilisateur distroless
  nonroot (uid 65532) y écrit le YAML de `pair`. Ne le bind-mount pas sur un
  dossier hôte non inscriptible par cet uid.
- **`AUTH_PASSWORD=changeme`** : à changer impérativement avant exposition.
- **Versions Go différentes** (nxt-opds 1.24, librarian 1.26.1) : compile chaque
  repo avec son propre toolchain.

## 9. Désinstallation / rollback

```bash
# Docker
cd nxt-opds-org && docker compose down            # ajoute -v pour effacer les volumes (config + perte de l'appairage)

# systemd
sudo systemctl disable --now nxt-opds librarian-update.timer
# Désappairer proprement : UI admin → « Désappairer » (notifie les deux côtés),
# ou côté librarian : librarian unpair --instance <slug>
```
