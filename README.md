# librarian-agent

Agent Go autonome qui reproduit le skill `/librarian` : maintenance et enrichissement
d'une bibliothèque personnelle via le serveur MCP OPDS.

L'agent peut s'exécuter dans plusieurs modes :

- **`run`** — une exécution one-shot (équivalent du skill original)
- **`serve`** — daemon qui combine un **ticker périodique** et un **webhook HTTP**
  pour réagir aux nouveaux livres en temps réel
- **`update`** — télécharge la dernière release GitHub correspondant à la
  plateforme et remplace le binaire en place
- **`version`** — affiche la version installée

## Architecture

```
cmd/librarian        # CLI : sous-commandes run et serve
internal/mcp         # client MCP HTTP Streamable (JSON-RPC + SSE)
internal/llm         # interface Provider + backends Ollama et Anthropic
internal/agent       # boucle tool-calling + system prompt + web_fetch local
internal/daemon      # ticker + serveur HTTP + queue serialisée
```

## Backends LLM

- **Ollama** (défaut, local) — qwen3.5, qwen2.5:7b, llama3.1, etc.
- **Anthropic** (Claude) — sélectionné si `ANTHROPIC_API_KEY` est défini

Forcer via `--backend ollama|anthropic`.

## Configuration

| Variable                    | Rôle |
|-----------------------------|------|
| `OPDS_MCP_URL`              | URL MCP (défaut : https://books.jerinn.com/mcp) |
| `OPDS_MCP_TOKEN`            | bearer MCP (**obligatoire**) |
| `LIBRARIAN_BACKEND`         | `auto` / `ollama` / `anthropic` |
| `LIBRARIAN_MODEL`           | nom de modèle |
| `OLLAMA_HOST`               | endpoint Ollama (défaut localhost:11434) |
| `ANTHROPIC_API_KEY`         | clé API Claude |
| `LIBRARIAN_WEBHOOK_SECRET`  | secret HMAC pour valider le webhook |

## Build

```bash
go build -o librarian ./cmd/librarian
```

## Mode `update` (self-update)

```bash
# Mise à jour vers la dernière release
./librarian update

# Voir la version cible sans rien télécharger
./librarian update --dry-run

# Forcer la réinstallation
./librarian update --force

# Voir la version courante
./librarian version
```

L'updater interroge `api.github.com/repos/banux/nxt-opds-librarian/releases/latest`,
télécharge l'asset correspondant à `GOOS-GOARCH`, et remplace le binaire courant
atomiquement (rename direct sur Linux ; backup `.old` en fallback).

## Mode `run` (one-shot)

```bash
# Maintenance batch (5 livres non indexés)
OPDS_MCP_TOKEN=xxx ./librarian run

# Cibler un livre par titre
OPDS_MCP_TOKEN=xxx ./librarian run "Le Chevalier et la Phalène"

# Passer un prompt complet
OPDS_MCP_TOKEN=xxx ./librarian run --prompt "Liste les 10 derniers livres ajoutés"

# Forcer Claude
ANTHROPIC_API_KEY=sk-… OPDS_MCP_TOKEN=xxx ./librarian run --backend anthropic
```

## Mode `serve` (daemon)

```bash
OPDS_MCP_TOKEN=xxx ./librarian serve \
    --listen :8080 \
    --interval 6h \
    --batch-limit 10
```

### Endpoints exposés

| Route                        | Méthode | Description |
|------------------------------|---------|-------------|
| `/healthz`                   | GET     | OK |
| `/webhook/book-added`        | POST    | Notification d'ajout de livre |
| `/trigger`                   | POST    | Déclenche un job manuellement |

### Webhook : payload attendu

`POST /webhook/book-added` — JSON :

```json
{ "book_id": "abc123" }
```

Ou si on n'a pas l'id :

```json
{ "title": "Le Chevalier et la Phalène", "author": "X. Y." }
```

Si `LIBRARIAN_WEBHOOK_SECRET` est défini, l'appel doit fournir un en-tête
`X-Signature: sha256=<HMAC-SHA256 du body en hex>`.

```bash
curl -X POST http://localhost:8080/webhook/book-added \
     -H 'Content-Type: application/json' \
     -d '{"book_id":"abc123"}'
```

### Trigger manuel avec prompt custom

`POST /trigger` accepte un prompt arbitraire :

```bash
# Déclenche la maintenance batch par défaut
curl -X POST http://localhost:8080/trigger

# Prompt JSON
curl -X POST http://localhost:8080/trigger \
     -H 'Content-Type: application/json' \
     -d '{"prompt":"Traite La Boussole dans les Étoiles"}'

# Prompt brut
curl -X POST http://localhost:8080/trigger \
     -H 'Content-Type: text/plain' \
     --data 'Liste les 5 dernières séries incomplètes'
```

### Prompt personnalisé permanent

Le flag `--prompt` remplace l'instruction batch utilisée par le **ticker**
(et par `/trigger` sans body) :

```bash
./librarian serve --prompt "Traite les 3 livres les plus anciennement modifiés"
```

### Queue

Tous les jobs (tick, webhook, trigger) passent par une **file unique** :
l'agent ne traite qu'un job à la fois. Si la file dépasse 16 jobs en attente,
les nouveaux sont rejetés (log warning).

## Exemple systemd

```ini
# /etc/systemd/system/librarian.service
[Unit]
Description=Librarian agent
After=network.target

[Service]
Type=simple
User=banux
Environment=OPDS_MCP_TOKEN=xxx
Environment=LIBRARIAN_WEBHOOK_SECRET=yyy
Environment=ANTHROPIC_API_KEY=sk-...
ExecStart=/usr/local/bin/librarian serve --listen :8080 --interval 6h
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## System prompt

Le prompt principal vit dans `internal/agent/prompt.go`. Il reprend les règles
du `CLAUDE.md` du projet : capitalisation Title Case des tags, nettoyage HTML
des résumés, classification d'âge numérique, normalisation des titres et
séries, mise à jour de `last_maintenance_at: -1`, vérification de la wishlist.
