# jacred-go

Go-based multi-tracker torrent aggregator. Port of C# project jacred (mainly from https://github.com/jacred-fdb/jacred, web interface 100% from)

Collects torrent metadata from 20 Russian/Ukrainian trackers into a unified flat-file database with search, sync, and stats APIs.

## Table of Contents

- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Parser Endpoints](#parser-endpoints)
- [Search API](#search-api)
- [Stats API](#stats-api)
- [Sync API](#sync-api)
- [Database Admin Endpoints](#database-admin-endpoints)
- [Dev/Maintenance Endpoints](#devmaintenance-endpoints)
- [Config Hot-Reload](#config-hot-reload)
- [Search Result Caching](#search-result-caching)
- [FDB Audit Log](#fdb-audit-log)
- [Parser Logging](#parser-logging)
- [Cron Examples](#cron-examples)
- [Database Structure](#database-structure)

---

## Quick Start

```bash
# Build for current platform
go build -o ./Dist/jacred ./cmd
./Dist/jacred
# Listens on :9117 by default

# Health check
curl http://127.0.0.1:9117/health

# Parse first page of rutor
curl http://127.0.0.1:9117/cron/rutor/parse

# Search
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Interstellar"
```

## Build for all platforms

```bash
chmod +x build_all.sh
./build_all.sh
```

Builds binaries for all supported platforms into `Dist/` and creates a release archive:

```
Dist/
  jacred-linux-amd64
  jacred-linux-arm64
  jacred-linux-arm
  jacred-linux-386
  jacred-darwin-amd64
  jacred-darwin-arm64
  jacred-windows-amd64.exe
  jacred-windows-arm64.exe
  jacred-windows-386.exe
  jacred-freebsd-amd64
  jacred-freebsd-arm64

jacred-{version}-{gitSHA}.tar.gz   ← binaries + wwwroot + init.yaml + init.yaml.example
```

Requires Go 1.21+. All binaries are statically linked (`CGO_ENABLED=0`), no external dependencies.

---

## Configuration

Configuration file: `init.yaml` in working directory.

### Global Settings

```yaml
listenip: "any"              # IP to bind ("any" = 0.0.0.0)
listenport: 9117             # Listening port
apikey: ""                   # API key required for /api/v1.0/* (empty = no auth)
devkey: ""                   # Key for dev endpoints (empty = local IP only)
log: true                    # Enable logging
logParsers: false            # Global gate for per-tracker parser logs (both must be true)
logFdb: false                # FDB audit log: JSON Lines per bucket change
logFdbRetentionDays: 7       # Delete FDB log files older than N days
logFdbMaxSizeMb: 0           # Max total FDB log size in MB (0 = unlimited)
logFdbMaxFiles: 0            # Max number of FDB log files (0 = unlimited)
fdbPathLevels: 2             # Directory nesting depth for bucket files
mergeduplicates: true        # Merge duplicate torrents from different trackers
mergenumduplicates: true     # Merge numeric ID variations
openstats: true              # Enable /stats/* endpoints (no auth)
opensync: true               # Enable /sync/fdb/torrents (V2 protocol, no auth)
opensync_v1: false           # Enable /sync/torrents (V1 protocol, no auth)
web: true                    # Serve web UI (index.html, stats.html)
timeStatsUpdate: 90          # Rebuild stats.json every N minutes
memlimit: 0                  # Hard cap on Go heap in MB (0 = no limit)
gcpercent: 50                # GC frequency: lower = more GC, less peak RAM (default 50)
```

### Memory Limits

Control Go runtime memory usage. Essential on VPS with limited RAM.

```yaml
memlimit: 1500   # Hard cap on Go heap in MB; GC becomes very aggressive near the limit
gcpercent: 50    # Go's GOGC knob: 50 = GC at +50% heap growth (default 50, Go default is 100)
```

**Recommended settings by RAM:**

| VPS RAM | `memlimit` | `gcpercent` | `evercache.maxOpenWriteTask` |
|---------|-----------|-------------|------------------------------|
| 1 GB    | `700`     | `20`        | `200`                        |
| 2 GB    | `1500`    | `30`        | `300`                        |
| 4 GB+   | `0`       | `50`        | `500`                        |

`memlimit: 0` disables the hard cap (Go default behaviour).

### CloudFlare Bypass (flaresolverr-go + cfclient)

For trackers protected by CloudFlare (megapeer, bitru, anistar, anifilm, torrentby, mazepa), the system uses two built-in components (no external Docker services required):

1. **flaresolverr-go** — embedded Chromium-based CF challenge solver. Runs via Xvfb virtual display. Cookies are cached for 30 minutes per domain.
2. **cfclient** — TLS fingerprint spoofing (tls-client with Chrome profile). Used for HTTP requests after cookies are obtained.

The flow: flaresolverr-go solves the CF challenge and obtains cookies → cfclient uses those cookies with Chrome TLS fingerprint → falls back to standard HTTP if cfclient fails.

```yaml
# Embedded flaresolverr-go settings (no external service needed)
flaresolverr_go:
  headless: true              # true = headless Chrome (default), false = visible (needs Xvfb)
  browser_path: ""            # Custom Chromium path (empty = auto-detect)

# TLS fingerprint for CF-protected requests
cfclient:
  profile: "chrome_146"       # TLS profile: chrome_133, chrome_144, chrome_146, firefox_117, etc.
```

Enable per tracker via `fetchmode`:

```yaml
Megapeer:
  fetchmode: "flaresolverr"   # "standard" (default) or "flaresolverr"
  host: "https://megapeer.vip"
```

If the response is a CF challenge page (403 or missing content marker), the session is automatically invalidated and re-solved.

### Evercache (In-Memory Bucket Cache)

Keeps recently opened buckets in RAM to reduce disk reads on repeated searches.
The cache is hard-capped at `maxOpenWriteTask` entries; when full the oldest
`dropCacheTake` entries are evicted immediately. Stale entries (older than
`validHour`) are swept every 10 minutes by a background goroutine.

```yaml
evercache:
  enable: true               # Enable in-memory caching of buckets
  validHour: 1               # Cache TTL in hours; entries older than this are evicted
  maxOpenWriteTask: 500      # Max buckets held in memory (hard cap)
  dropCacheTake: 100         # How many to evict when the cap is hit
```

### Sync (Multi-Instance)

```yaml
syncapi: "http://other-instance:9117"   # URL of remote jacred to sync from
timeSync: 60                            # Sync interval in seconds
synctrackers:                           # Sync only these trackers (empty = all)
  - "Rutor"
  - "Kinozal"
disable_trackers:                       # Never sync these trackers
  - "Mazepa"
syncsport: true                         # Sync sport torrents
syncspidr: true                         # Enable spider (metadata-only) sync
timeSyncSpidr: 60                       # Spider sync interval in seconds
```

### Per-Tracker Settings

Each tracker section is optional — defaults are used if omitted.

```yaml
Kinozal:
  host: "https://kinozal.tv"   # Override default host
  cookie: "uid=abc123; pass=..." # Session cookie (required for login-only trackers)
  login:
    u: "username"
    p: "password"
  reqMinute: 8                  # Max requests per minute (rate limiting)
  parseDelay: 7000              # Delay between category/page requests in ms
  log: false                    # Log this tracker's requests
  useproxy: false               # Route requests through globalproxy
```

**Default hosts:**

| Tracker | Default Host |
|---------|-------------|
| Rutor | `https://rutor.is` |
| Megapeer | `https://megapeer.vip` |
| TorrentBy | `https://torrent.by` |
| Kinozal | `https://kinozal.tv` |
| NNMClub | `https://nnmclub.to` |
| Bitru | `https://bitru.org` |
| Toloka | `https://toloka.to` |
| Mazepa | `https://mazepa.to` |
| Rutracker | `https://rutracker.org` |
| Selezen | `https://use.selezen.club` |
| Lostfilm | `https://www.lostfilm.tv` |
| Animelayer | `https://animelayer.ru` |
| Anidub | `https://tr.anidub.com` |
| Aniliberty | `https://aniliberty.top` |
| Knaben | `https://api.knaben.org` |
| Anistar | `https://anistar.org` |
| Anifilm | `https://anifilm.pro` |
| Leproduction | `https://www.le-production.tv` |
| Baibako | `http://baibako.tv` |

### Proxy

```yaml
globalproxy:
  - pattern: "\.onion"           # Regex: apply proxy when URL matches
    list:
      - "socks5://127.0.0.1:9050"
      - "http://proxy.example.com:8080"
    useAuth: false
    username: ""
    password: ""
    BypassOnLocal: true          # Skip proxy for 127.x / 192.168.x / 10.x
```

---

## Parser Endpoints

All parser endpoints return:

```json
{
  "status": "ok",
  "fetched": 150,
  "added": 12,
  "updated": 5,
  "skipped": 133,
  "failed": 0
}
```

Multi-category parsers also include `"by_category": [...]`.

### Parsing Strategies

There are five distinct parsing strategies across the 20 trackers:

#### 1. Single-page (`page=N`)

Parse exactly one page. Default is page 0 (most recent) for most trackers; Bitru defaults to page 1.

**Trackers:** Rutor, Selezen, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka

> Note: Rutor, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka also support task-based parsing (see §4 below). The `parse?page=N` endpoint is available as a single-page fallback.

```bash
# Parse the latest page (default)
curl "http://127.0.0.1:9117/cron/rutor/parse"

# Parse a specific page
curl "http://127.0.0.1:9117/cron/kinozal/parse?page=3"
```

#### 2. Multi-page (`maxpage=N` or `limit_page=N`)

Parse N pages starting from the first. `0` means unlimited (all pages).

**Trackers:** Megapeer (`maxpage`, default 1), Animelayer (`maxpage`, default 1), Baibako (`maxpage`, default 10), Anistar (`limit_page`, default 0 = all), Leproduction (`limit_page`, default 0 = all)

```bash
# Megapeer/Animelayer/Baibako: default = 1 page
curl "http://127.0.0.1:9117/cron/megapeer/parse"

# Parse up to 5 pages
curl "http://127.0.0.1:9117/cron/megapeer/parse?maxpage=5"

# Anistar/Leproduction: default = all pages (limit_page=0)
curl "http://127.0.0.1:9117/cron/anistar/parse"

# Limit to 3 pages
curl "http://127.0.0.1:9117/cron/anistar/parse?limit_page=3"
curl "http://127.0.0.1:9117/cron/leproduction/parse?limit_page=3"
```

#### 3. Page range (`parseFrom=N&parseTo=M`)

Parse pages from N to M inclusive.

**Trackers:** Anidub, Aniliberty, Selezen

```bash
# Parse pages 1 to 5
curl "http://127.0.0.1:9117/cron/anidub/parse?parseFrom=1&parseTo=5"

# Parse only page 1 (single page)
curl "http://127.0.0.1:9117/cron/selezen/parse?parseFrom=1&parseTo=1"

# Aniliberty also returns lastPage in response
curl "http://127.0.0.1:9117/cron/aniliberty/parse?parseFrom=1&parseTo=3"
```

#### 4. Task-based (incremental, recommended for large trackers)

For large trackers with hundreds of category pages. Works in three steps:

1. **Discover** all pages by category and year → stores tasks in `Data/{tracker}_tasks.json`
2. **Parse all** discovered tasks (can be interrupted and resumed)
3. **Parse latest** — shortcut to parse only the most recent N pages

**Trackers:** Rutor, Selezen, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka

```bash
# Step 1: Discover all pages and build task list (run once or periodically)
curl "http://127.0.0.1:9117/cron/kinozal/updatetasksparse"

# Step 2: Parse all discovered tasks (can take a long time)
curl "http://127.0.0.1:9117/cron/kinozal/parsealltask"

# Or: Parse only the latest 5 pages (quick daily update)
curl "http://127.0.0.1:9117/cron/kinozal/parselatest"
curl "http://127.0.0.1:9117/cron/kinozal/parselatest?pages=10"

# Fallback: parse a single known page
curl "http://127.0.0.1:9117/cron/kinozal/parse?page=0"
```

Task state is persisted — interrupted `parsealltask` resumes from where it stopped.

#### 5. Full vs. incremental (`fullparse=true/false`)

**Tracker:** Anifilm only

```bash
# Incremental: only new/updated since last run (default)
curl "http://127.0.0.1:9117/cron/anifilm/parse"
curl "http://127.0.0.1:9117/cron/anifilm/parse?fullparse=false"

# Full re-parse: all pages
curl "http://127.0.0.1:9117/cron/anifilm/parse?fullparse=true"
```

---

### Per-Tracker Parse Reference

#### Rutor
```
GET /cron/rutor/parse
  page=N   (default 0) — parse page N across all 11 categories

GET /cron/rutor/updatetasksparse      — discover all pages per category
GET /cron/rutor/parsealltask          — parse all discovered tasks
GET /cron/rutor/parselatest
  pages=N   (default 5) — parse latest N pages per category
```
Parses all 11 categories: movies, music, serials, documentaries, cartoons, anime, sport, Ukrainian content.
Without parameters `parse` fetches one page (page 0) from all categories simultaneously.

#### Megapeer
```
GET /cron/megapeer/parse
  maxpage=N   (default 1) — parse up to N pages
```

#### Anidub
```
GET /cron/anidub/parse
  parseFrom=N   (default 0) — page range start
  parseTo=M     (default 0) — page range end
```
Without parameters: parses only page 0.

#### Aniliberty
```
GET /cron/aniliberty/parse
  parseFrom=N   (default 0) — page range start
  parseTo=M     (default 0) — page range end

Response includes: { ..., "lastPage": 42 }
```
Without parameters: parses only page 0.

#### Animelayer
```
GET /cron/animelayer/parse
  maxpage=N   (default 1)
```

#### Anistar
```
GET /cron/anistar/parse
  limit_page=N   (default 0 = parse all pages)
  limitPage=N    (alias)
```

#### Anifilm
```
GET /cron/anifilm/parse
  fullparse=false   (default) — only new/updated since last run
  fullparse=true    — re-parse all pages
```

#### Baibako
```
GET /cron/baibako/parse
  maxpage=N   (default 10)
```

#### Bitru
```
GET /cron/bitru/parse
  page=N   (default 1) — parse single page

GET /cron/bitru/updatetasksparse      — discover all category pages
GET /cron/bitru/parsealltask          — parse all discovered tasks
GET /cron/bitru/parselatest
  pages=N   (default 5) — parse latest N pages only
```

#### BitruAPI
```
GET /cron/bitruapi/parse
  limit=N   (default 100) — number of recent items to fetch via API

GET /cron/bitruapi/parsefromdate
  lastnewtor=YYYY-MM-DD   — fetch items newer than this date
  limit=N                  (default 100)
```

#### Kinozal
```
GET /cron/kinozal/parse
  page=N   (default 0) — parse single page

GET /cron/kinozal/updatetasksparse
GET /cron/kinozal/parsealltask
GET /cron/kinozal/parselatest
  pages=N   (default 5)
```

#### Knaben
```
GET /cron/knaben/parse
  from=N            (default 0) — offset in results
  size=N            (default 300) — results per page
  pages=N           (default 1) — number of pages to fetch
  query=string      — search query
  hours=N           (0 = ignore time filter) — only items from last N hours
  orderBy=string    (default "date") — sort order
  categories=a,b,c  — comma-separated category filters
```

#### Leproduction
```
GET /cron/leproduction/parse
  limit_page=N   (default 0 = parse all pages)
```

#### Lostfilm
```
GET /cron/lostfilm/parse            — parse main catalog (latest releases)

GET /cron/lostfilm/parsepages
  pageFrom=N   (default 1)
  pageTo=N     (default 1)

GET /cron/lostfilm/parseseasonpacks
  series=SeriesName   — parse all season packs for a specific series

GET /cron/lostfilm/verifypage
  series=SeriesName   — verify parsed data for a series

GET /cron/lostfilm/stats            — Lostfilm-specific stats
```

#### Mazepa
```
GET /cron/mazepa/parse   — no parameters, parses current page
```

#### NNMClub
```
GET /cron/nnmclub/parse
  page=N   (default 0)

GET /cron/nnmclub/updatetasksparse
GET /cron/nnmclub/parsealltask
GET /cron/nnmclub/parselatest
  pages=N   (default 5)
```

#### RuTracker
```
GET /cron/rutracker/parse
  page=N   (default 0)

GET /cron/rutracker/updatetasksparse
GET /cron/rutracker/parsealltask
GET /cron/rutracker/parselatest
  pages=N   (default 5)
```

#### Selezen
```
GET /cron/selezen/parse
  parseFrom=N   (default 0) — page range start
  parseTo=M     (default 0) — page range end

GET /cron/selezen/updatetasksparse      — discover all pages
GET /cron/selezen/parsealltask          — parse all discovered tasks
GET /cron/selezen/parselatest
  pages=N   (default 5) — parse latest N pages only
```
Without parameters `parse` parses only page 1.

#### Toloka
```
GET /cron/toloka/parse
  page=N   (default 0)

GET /cron/toloka/updatetasksparse
GET /cron/toloka/parsealltask
GET /cron/toloka/parselatest
  pages=N   (default 5)
```

#### TorrentBy
```
GET /cron/torrentby/parse
  page=N   (default 0)

GET /cron/torrentby/updatetasksparse
GET /cron/torrentby/parsealltask
GET /cron/torrentby/parselatest
  pages=N   (default 5)
```

---

## Search API

### `GET /api/v1.0/torrents`

Full-text torrent search.

**Query Parameters:**

| Parameter | Aliases | Type | Description |
|-----------|---------|------|-------------|
| `search` | `q` | string | Full-text search in title and name fields |
| `altname` | `altName` | string | Search in original/alternative name |
| `exact` | — | bool | Exact match instead of fuzzy |
| `type` | — | string | Content type: `фильм`, `сериал`, `аниме`, `музыка`, etc. |
| `tracker` | `trackerName` | string | Filter by tracker name (e.g. `Kinozal`) |
| `voice` | `voices` | string | Filter by dubbing studio or voice |
| `videotype` | `videoType` | string | Video format filter (e.g. `hdr`, `sdr`) |
| `relased` | `released` | int | Release year (e.g. `2023`) |
| `quality` | — | int | Quality code: `480`, `720`, `1080`, `2160` |
| `season` | — | int | Season number |
| `sort` | — | string | Sort order: `date`, `size`, `sid` |

```bash
# Basic search
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Interstellar"

# Filter by tracker and quality
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Dune&tracker=Kinozal&quality=1080"

# Anime by season
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Naruto&type=аниме&season=5"

# Exact title match
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Inception&exact=true"
```

**Response:**

```json
[
  {
    "tracker": "Kinozal",
    "url": "https://kinozal.tv/details.php?id=123456",
    "title": "Дюна / Dune (2021) BDRip 1080p",
    "size": 15032385536,
    "sizeName": "14.0 GB",
    "createTime": "2021-11-15 12:30:00",
    "updateTime": "2021-11-15 12:30:00",
    "sid": 350,
    "pir": 12,
    "magnet": "magnet:?xt=urn:btih:...",
    "name": "дюна",
    "originalname": "dune",
    "relased": 2021,
    "videotype": "sdr",
    "quality": 1080,
    "voices": "Дублированный",
    "seasons": "",
    "types": ["фильм", "зарубежный"]
  }
]
```

### `GET /api/v1.0/qualitys`

Paginated listing of quality metadata.

| Parameter | Default | Description |
|-----------|---------|-------------|
| `name` | — | Filter by name |
| `originalname` / `originalName` | — | Filter by original name |
| `type` | — | Filter by type |
| `page` | `1` | Page number |
| `take` | `1000` | Items per page |

### `GET /api/v2.0/indexers/[indexer]/results`

Jackett-compatible API for use with Sonarr, Radarr, etc.

| Parameter | Aliases | Description |
|-----------|---------|-------------|
| `query` | `q` | Search query |
| `title` | — | Title to match |
| `title_original` | — | Original title |
| `year` | — | Release year |
| `is_serial` | — | `1` for series, `0` for movies |
| `category` | — | Category prefix: `mov_`, `tv_`, `anime_`, etc. |
| `apikey` | `apiKey` | API key (if configured) |

```bash
curl "http://127.0.0.1:9117/api/v2.0/indexers/jacred/results?q=Dune&year=2021"
```

Response: `{ "Results": [...], "jacred": true }`

### `GET /api/v1.0/conf`

Validate API key.

```bash
curl "http://127.0.0.1:9117/api/v1.0/conf?apikey=your-key"
# Response: {"apikey": true}
```

---

## Stats API

Stats endpoints are open (no API key required) when `openstats: true`.

### `GET /stats/refresh`

Refresh `Data/temp/stats.json`

### `GET /stats/torrents`

Returns pre-computed stats from `Data/temp/stats.json` (rebuilt every `timeStatsUpdate` seconds).

| Parameter | Aliases | Default | Description |
|-----------|---------|---------|-------------|
| `trackerName` | — | — | If set: compute on-demand for this tracker |
| `newtoday` | `newToday` | `0` | `1` = only torrents added today |
| `updatedtoday` | `updatedToday` | `0` | `1` = only torrents updated today |
| `limit` | `take` | `200` | Max items to return |

```bash
# Full stats (from cache)
curl "http://127.0.0.1:9117/stats/torrents"

# Today's new torrents from Kinozal
curl "http://127.0.0.1:9117/stats/torrents?trackerName=Kinozal&newtoday=1"
```

### `GET /stats/trackers`

Per-tracker statistics summary.

| Parameter | Default | Description |
|-----------|---------|-------------|
| `newtoday` / `newToday` | `0` | Filter to today's new torrents |
| `updatedtoday` / `updatedToday` | `0` | Filter to today's updated torrents |
| `limit` / `take` | `200` | Max items |

```bash
curl "http://127.0.0.1:9117/stats/trackers"
curl "http://127.0.0.1:9117/stats/trackers?newtoday=1&limit=50"
```

### `GET /stats/trackers/{trackerName}`

Stats for a specific tracker.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Rutor"
curl "http://127.0.0.1:9117/stats/trackers/Rutor?newtoday=1"
```

### `GET /stats/trackers/{trackerName}/new`

Torrents added today from the specified tracker.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Kinozal/new?limit=100"
```

### `GET /stats/trackers/{trackerName}/updated`

Torrents updated today from the specified tracker.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Kinozal/updated"
```

---

## Sync API

Multi-instance synchronization. Enabled by `opensync: true` in config.

### `GET /sync/conf`

Discovery endpoint — check protocol version.

```json
{ "fbd": true, "spidr": true, "version": 2 }
```

### V2 Protocol

#### `GET /sync/fdb`

List database bucket keys (low-level, for replication).

| Parameter | Default | Description |
|-----------|---------|-------------|
| `key` | — | Substring filter on bucket key (e.g. `matrix`) |
| `limit` / `take` | `20` | Max entries to return |

```bash
curl "http://127.0.0.1:9117/sync/fdb?key=matrix&limit=5"
```

#### `GET /sync/fdb/torrents`

Incremental sync — returns torrents modified after a timestamp.

| Parameter | Aliases | Required | Description |
|-----------|---------|----------|-------------|
| `time` | `fileTime` | **Yes** | Return only buckets with fileTime > this value (Windows FILETIME format) |
| `start` | `startTime` | No | Return only torrents with updateTime > this value |
| `spidr` | — | No | `true` = return only url/sid/pir metadata (lighter payload) |
| `take` | `limit` | No | Batch size (default 2000) |

```bash
# Initial sync (time=0 = get everything)
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=0&take=2000"

# Incremental sync using fileTime from previous response
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=133476543210000000"

# Spider mode (metadata only, faster)
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=0&spidr=true"
```

**Response:**

```json
{
  "nextread": true,
  "countread": 2000,
  "take": 2000,
  "collections": [
    {
      "Key": "матрица:the matrix",
      "Value": {
        "time": "2024-01-15 10:30:45",
        "fileTime": 133476543210000000,
        "torrents": {
          "https://kinozal.tv/details.php?id=123": { ... }
        }
      }
    }
  ]
}
```

When `nextread: true`, call again with the last received `fileTime` to get more data.

### V1 Protocol (Legacy)

Enabled with `opensync_v1: true`.

#### `GET /sync/torrents`

| Parameter | Aliases | Required | Description |
|-----------|---------|----------|-------------|
| `time` | `fileTime` | **Yes** | Timestamp filter |
| `trackerName` | `tracker` | No | Filter by tracker |
| `take` | `limit` | No | Batch size (default 2000) |

```bash
curl "http://127.0.0.1:9117/sync/torrents?time=0&trackerName=Rutor"
```

Response: flat array of `{ "key": "name:originalname", "value": {...} }`

---

## Database Admin Endpoints

### `GET /jsondb/save`

Manually flush in-memory database to disk (`Data/masterDb.bz`).

```bash
curl "http://127.0.0.1:9117/jsondb/save"
# "work" — already saving
# "ok"   — saved successfully
```

A daily backup `Data/masterDb_DD-MM-YYYY.bz` is created on each save. Backups older than 3 days are auto-deleted.

---

## Dev/Maintenance Endpoints

Available from **local IP only** (127.0.0.1, ::1, fe80::/10 link-local, fc00::/7 ULA, IPv4-mapped IPv6).

### Data Integrity

#### `GET /dev/findcorrupt`

Scans all buckets for corrupted entries.

| Parameter | Aliases | Default | Description |
|-----------|---------|---------|-------------|
| `sampleSize` | `sample`, `limit` | `20` | Max examples per issue type |

```bash
curl "http://127.0.0.1:9117/dev/findcorrupt"
curl "http://127.0.0.1:9117/dev/findcorrupt?sampleSize=50"
```

**Response:**
```json
{
  "ok": true,
  "totalFdbKeys": 12500,
  "totalTorrents": 480000,
  "corrupt": {
    "nullValue":           { "count": 0, "sample": [] },
    "missingName":         { "count": 12, "sample": [...] },
    "missingOriginalname": { "count": 3, "sample": [...] },
    "missingTrackerName":  { "count": 0, "sample": [] }
  }
}
```

#### `GET /dev/removeNullValues`

Deletes entries where torrent value is `null`.

```bash
curl "http://127.0.0.1:9117/dev/removeNullValues"
# Response: { "ok": true, "removed": 5, "files": 3 }
```

#### `GET /dev/findDuplicateKeys`

Finds bucket keys where name == originalname (potential duplicates).

| Parameter | Aliases | Default | Description |
|-----------|---------|---------|-------------|
| `tracker` | `trackerName` | — | Filter: only keys containing this tracker's torrents |
| `excludeNumeric` | — | `true` | Exclude purely numeric keys |

```bash
curl "http://127.0.0.1:9117/dev/findDuplicateKeys"
curl "http://127.0.0.1:9117/dev/findDuplicateKeys?tracker=Kinozal&excludeNumeric=false"
```

#### `GET /dev/findEmptySearchFields`

Finds torrents with empty `_sn` (search name) or `_so` (search original) fields.

| Parameter | Aliases | Default | Description |
|-----------|---------|---------|-------------|
| `sampleSize` | `sample`, `limit` | `20` | Max examples per category |

```bash
curl "http://127.0.0.1:9117/dev/findEmptySearchFields"
```

### Data Updates

#### `GET /dev/updateDetails`

Recomputes `quality`, `videotype`, `voices`, `languages`, `seasons` for all stored torrents.

```bash
curl "http://127.0.0.1:9117/dev/updateDetails"
```

#### `GET /dev/updateSearchName`

Update search fields for a specific bucket.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `bucket` | Yes | Bucket key in format `name:originalname` |
| `fieldName` | No | Field to update |
| `value` | No | New value |

```bash
curl "http://127.0.0.1:9117/dev/updateSearchName?bucket=матрица:the+matrix&fieldName=_sn&value=матрица"
```

#### `GET /dev/updateSize`

Update torrent size in a bucket.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `bucket` | Yes | Bucket key |
| `value` | Yes | Size with suffix: `500MB`, `14.7GB`, `1.2TB`, or bytes |

```bash
curl "http://127.0.0.1:9117/dev/updateSize?bucket=матрица:the+matrix&value=14.7GB"
```

#### `GET /dev/resetCheckTime`

Resets `checkTime` to yesterday for all torrents (forces re-check on next parse).

```bash
curl "http://127.0.0.1:9117/dev/resetCheckTime"
```

### Tracker-Specific Fixes

#### `GET /dev/fixKnabenNames`

Normalizes Knaben torrent names/years/titles and migrates to correct bucket keys.

```bash
curl "http://127.0.0.1:9117/dev/fixKnabenNames"
```

#### `GET /dev/fixBitruNames`

Cleans Bitru torrent titles (strips quality tags, codec info, release groups).

```bash
curl "http://127.0.0.1:9117/dev/fixBitruNames"
```

#### `GET /dev/fixEmptySearchFields`

Auto-populates empty `_sn`/`_so` search fields and migrates torrents to correct buckets.

```bash
curl "http://127.0.0.1:9117/dev/fixEmptySearchFields"
```

#### `GET /dev/migrateAnilibertyUrls`

Appends `?hash=<btih>` to Aniliberty URLs using magnet link hashes.

```bash
curl "http://127.0.0.1:9117/dev/migrateAnilibertyUrls"
```

#### `GET /dev/removeDuplicateAniliberty`

Deduplicates Aniliberty torrents by magnet hash, keeping the most recent.

```bash
curl "http://127.0.0.1:9117/dev/removeDuplicateAniliberty"
```

#### `GET /dev/fixAnimelayerDuplicates`

Normalizes http→https for Animelayer URLs and removes duplicates by hex ID.

```bash
curl "http://127.0.0.1:9117/dev/fixAnimelayerDuplicates"
```

### Bucket Management

#### `GET /dev/removeBucket`

Delete an entire bucket (all torrents under a key).

| Parameter | Required | Description |
|-----------|----------|-------------|
| `key` | Yes | Bucket key in format `name:originalname` |
| `migrateName` | No | If set: move torrents to this new name instead of deleting |
| `migrateOriginalname` | No | New originalname for migration target |

```bash
# Delete bucket
curl "http://127.0.0.1:9117/dev/removeBucket?key=матрица:the+matrix"

# Rename/migrate bucket to new key
curl "http://127.0.0.1:9117/dev/removeBucket?key=матрица:the+matrix&migrateName=the+matrix&migrateOriginalname=the+matrix"
```

---

## Config Hot-Reload

The `init.yaml` file is checked for changes every 10 seconds. When the file modification time changes, the config is reloaded automatically — no restart needed.

Hot-reloadable settings include: API keys, logging flags, sync settings, stats update interval, tracker hosts/cookies/credentials, rate limits.

## Search Result Caching

Search endpoints (`/api/v1.0/torrents` and `/api/v2.0/indexers/*/results`) cache results in memory with a 5-minute TTL. Cache hits return an `X-Cache: HIT` header. The cache is keyed by the full query string.

## FDB Audit Log

When `logFdb: true`, every bucket change is logged to `Data/log/fdb.YYYY-MM-dd.log` in JSON Lines format. Each line records the incoming and existing torrent data for the changed entry.

Retention is controlled by `logFdbRetentionDays`, `logFdbMaxSizeMb`, and `logFdbMaxFiles`. Cleanup runs automatically after each masterDb save.

## Parser Logging

Two-level logging control:
1. **Global gate:** `logParsers: true` must be set to enable any parser logging
2. **Per-tracker:** each tracker's `log: true` enables logging for that specific tracker

Both must be `true` for logs to be written. Log files are stored in `Data/log/{tracker}.log`.

---

## Cron Examples

Typical external crontab (`/etc/cron.d/jacred` or `Data/crontab`):

```cron
# Lightweight daily update — latest pages only
0  6 * * *  root  curl -s "http://127.0.0.1:9117/cron/rutor/parselatest?pages=3" >/dev/null
5  6 * * *  root  curl -s "http://127.0.0.1:9117/cron/kinozal/parselatest?pages=3" >/dev/null
10 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/rutracker/parselatest?pages=3" >/dev/null
15 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/nnmclub/parselatest?pages=3" >/dev/null
20 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/torrentby/parselatest?pages=3" >/dev/null

# Anime trackers — multiple pages
30 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/animelayer/parse?maxpage=3" >/dev/null
35 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anidub/parse?parseFrom=1&parseTo=3" >/dev/null
40 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anistar/parse?limit_page=3" >/dev/null
45 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anifilm/parse" >/dev/null

# Weekly full re-parse (task-based trackers)
0  2 * * 0  root  curl -s "http://127.0.0.1:9117/cron/kinozal/updatetasksparse" >/dev/null
30 2 * * 0  root  curl -s "http://127.0.0.1:9117/cron/kinozal/parsealltask" >/dev/null

# Force DB save after heavy parse
0  8 * * *  root  curl -s "http://127.0.0.1:9117/jsondb/save" >/dev/null
```

---

## Database Structure

```
Data/
  masterDb.bz               # Gzipped JSON index: key → {fileTime, updateTime, path}
  masterDb_DD-MM-YYYY.bz    # Daily backups (auto-created, kept 3 days)
  fdb/
    ab/cdef012...            # Bucket files (gzipped JSON): url → torrent object
  temp/
    stats.json               # Pre-computed stats cache
  log/
    YYYY-MM-DD.log           # Application log (when log: true)
    kinozal.log              # Per-tracker add/update/skip/fail logs
    fdb.YYYY-MM-dd.log       # FDB audit log: JSON Lines per bucket change
  {tracker}_tasks.json       # Task state for incremental parsers
```

Each bucket file key is derived from MD5 of the bucket key (`name:originalname`) split into 2-char prefix directory.

### Torrent Object Fields

| Field | Type | Description |
|-------|------|-------------|
| `url` | string | Tracker page URL (primary key within bucket) |
| `title` | string | Full torrent title as shown on tracker |
| `name` | string | Normalized Russian/English name |
| `originalname` | string | Original (usually English) name |
| `trackerName` | string | Source tracker name |
| `relased` | int | Release year |
| `size` | int64 | Size in bytes |
| `sizeName` | string | Human-readable size (e.g. `14.0 GB`) |
| `sid` | int | Seeders |
| `pir` | int | Peers/leechers |
| `magnet` | string | Magnet link |
| `btih` | string | Info hash |
| `quality` | int | `480`, `720`, `1080`, `2160` |
| `videotype` | string | `sdr`, `hdr` |
| `voices` | string | Dubbing studios |
| `languages` | string | `rus`, `ukr` |
| `seasons` | string | Season list, e.g. `1-5` or `1,3,7` |
| `types` | []string | Content type tags |
| `createTime` | string | First seen timestamp |
| `updateTime` | string | Last updated timestamp |
| `checkTime` | string | Last checked/parsed timestamp |
| `_sn` | string | Search-normalized name |
| `_so` | string | Search-normalized original name |

---

## Utility Endpoints

```
GET /health          → {"status": "OK"}
GET /version         → {"version": "...", "gitSha": "...", "gitBranch": "...", "buildDate": "..."}
GET /lastupdatedb    → last database write timestamp
GET /                → index.html (if web: true)
GET /stats           → stats.html (if web: true)
```
