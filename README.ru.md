# jacred-go

Мульти-трекерный торрент-агрегатор на Go. Порт C# проекта jacred (основа — https://github.com/jacred-fdb/jacred, веб-интерфейс полностью оттуда)

Собирает метаданные торрентов с 20 русскоязычных/украинских трекеров в единую файловую базу данных с API для поиска, синхронизации и статистики.

## Содержание

- [Быстрый старт](#быстрый-старт)
- [Конфигурация](#конфигурация)
- [Эндпоинты парсеров](#эндпоинты-парсеров)
- [API поиска](#api-поиска)
- [API статистики](#api-статистики)
- [API синхронизации](#api-синхронизации)
- [Управление базой данных](#управление-базой-данных)
- [Dev/Maintenance эндпоинты](#devmaintenance-эндпоинты)
- [Горячая перезагрузка конфига](#горячая-перезагрузка-конфига)
- [Кэширование результатов поиска](#кэширование-результатов-поиска)
- [FDB аудит-лог](#fdb-аудит-лог)
- [Логирование парсеров](#логирование-парсеров)
- [Примеры cron](#примеры-cron)
- [Структура базы данных](#структура-базы-данных)

---

## Быстрый старт

```bash
# Сборка под текущую платформу
go build -o ./Dist/jacred ./cmd
./Dist/jacred
# Слушает на :9117 по умолчанию

# Проверка здоровья
curl http://127.0.0.1:9117/health

# Парсинг первой страницы rutor
curl http://127.0.0.1:9117/cron/rutor/parse

# Поиск
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Interstellar"
```

## Сборка под все платформы

```bash
chmod +x build_all.sh
./build_all.sh
```

Собирает бинарники для всех поддерживаемых платформ в `Dist/` и создаёт архив релиза:

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

jacred-{version}-{gitSHA}.tar.gz   ← бинарники + wwwroot + init.yaml + init.yaml.example
```

Требуется Go 1.21+. Все бинарники статически слинкованы (`CGO_ENABLED=0`), без внешних зависимостей.

---

## Конфигурация

Файл конфигурации: `init.yaml` в рабочей директории.

### Глобальные настройки

```yaml
listenip: "any"              # IP для привязки ("any" = 0.0.0.0)
listenport: 9117             # Порт прослушивания
apikey: ""                   # API-ключ для /api/v1.0/* (пусто = без авторизации)
devkey: ""                   # Ключ для dev-эндпоинтов (пусто = только локальный IP)
log: true                    # Включить логирование
logParsers: false            # Глобальный гейт для логирования парсеров (оба флага должны быть true)
logFdb: false                # FDB аудит-лог: JSON Lines при каждом изменении bucket
logFdbRetentionDays: 7       # Удалять FDB лог-файлы старше N дней
logFdbMaxSizeMb: 0           # Макс. общий размер FDB логов в МБ (0 = без лимита)
logFdbMaxFiles: 0            # Макс. количество FDB лог-файлов (0 = без лимита)
fdbPathLevels: 2             # Глубина вложенности директорий для bucket-файлов
mergeduplicates: true        # Объединять дубли торрентов с разных трекеров
mergenumduplicates: true     # Объединять числовые вариации ID
openstats: true              # Открыть /stats/* эндпоинты (без авторизации)
opensync: true               # Открыть /sync/fdb/torrents (V2 протокол, без авторизации)
opensync_v1: false           # Открыть /sync/torrents (V1 протокол, без авторизации)
web: true                    # Раздавать веб-интерфейс (index.html, stats.html)
timeStatsUpdate: 90          # Пересчитывать stats.json каждые N минут
memlimit: 0                  # Жёсткий лимит Go-кучи в МБ (0 = без лимита)
gcpercent: 50                # Частота GC: ниже = чаще GC, меньше пик RAM (по умолчанию 50)
```

### Лимиты памяти

Управление использованием памяти Go runtime. Важно на VPS с ограниченной RAM.

```yaml
memlimit: 1500   # Жёсткий лимит Go-кучи в МБ; GC становится очень агрессивным вблизи лимита
gcpercent: 50    # Go GOGC: 50 = GC при +50% роста кучи (по умолчанию 50, Go default 100)
```

**Рекомендуемые настройки по объёму RAM:**

| RAM VPS | `memlimit` | `gcpercent` | `evercache.maxOpenWriteTask` |
|---------|-----------|-------------|------------------------------|
| 1 ГБ   | `700`     | `20`        | `200`                        |
| 2 ГБ   | `1500`    | `30`        | `300`                        |
| 4 ГБ+  | `0`       | `50`        | `500`                        |

`memlimit: 0` отключает жёсткий лимит (поведение Go по умолчанию).

### Обход CloudFlare (flaresolverr-go + cfclient)

Для трекеров, защищённых CloudFlare (megapeer, bitru, anistar, anifilm, torrentby, mazepa), используются два встроенных компонента — внешний Docker-сервис не нужен:

1. **flaresolverr-go** — встроенный решатель CF-challenge на базе Chromium. Работает через виртуальный дисплей Xvfb. Куки кешируются на 30 минут на домен.
2. **cfclient** — подмена TLS-отпечатка (tls-client с профилем Chrome). Используется для HTTP-запросов после получения кук.

Поток: flaresolverr-go решает CF-challenge и получает куки → cfclient использует их с Chrome TLS fingerprint → fallback на стандартный HTTP при сбое cfclient.

```yaml
# Настройки встроенного flaresolverr-go (внешний сервис не требуется)
flaresolverr_go:
  headless: true              # true = headless Chrome (по умолчанию), false = видимый (нужен Xvfb)
  browser_path: ""            # Путь к Chromium (пусто = автоопределение)

# TLS fingerprint для CF-защищённых запросов
cfclient:
  profile: "chrome_146"       # TLS-профиль: chrome_133, chrome_144, chrome_146, firefox_117 и т.д.
```

Включается для каждого трекера через `fetchmode`:

```yaml
Megapeer:
  fetchmode: "flaresolverr"   # "standard" (по умолчанию) или "flaresolverr"
  host: "https://megapeer.vip"
```

Если ответ — CF challenge-страница (403 или отсутствие маркера контента), сессия автоматически сбрасывается и challenge решается заново.

### Evercache (кэш bucket-ов в памяти)

Хранит недавно открытые bucket-ы в RAM для уменьшения чтений с диска при повторных поисках.
Кэш ограничен `maxOpenWriteTask` записями; при заполнении вытесняются `dropCacheTake` старейших.
Устаревшие записи (старше `validHour`) чистятся каждые 10 минут фоновой горутиной.

```yaml
evercache:
  enable: true               # Включить кэширование bucket-ов в памяти
  validHour: 1               # TTL кэша в часах; записи старше вытесняются
  maxOpenWriteTask: 500      # Макс. bucket-ов в памяти (жёсткий лимит)
  dropCacheTake: 100         # Сколько вытеснять при достижении лимита
```

### Синхронизация (мульти-инстанс)

```yaml
syncapi: "http://other-instance:9117"   # URL удалённого jacred для синхронизации
timeSync: 60                            # Интервал синхронизации в секундах
synctrackers:                           # Синхронизировать только эти трекеры (пусто = все)
  - "Rutor"
  - "Kinozal"
disable_trackers:                       # Никогда не синхронизировать эти трекеры
  - "Mazepa"
syncsport: true                         # Синхронизировать спортивные торренты
syncspidr: true                         # Включить spider-синхронизацию (только метаданные)
timeSyncSpidr: 60                       # Интервал spider-синхронизации в секундах
```

### Настройки трекеров

Каждая секция трекера опциональна — если не указана, используются значения по умолчанию.

```yaml
Kinozal:
  host: "https://kinozal.tv"   # Переопределить хост по умолчанию
  cookie: "uid=abc123; pass=..." # Cookie сессии (обязательно для трекеров с авторизацией)
  login:
    u: "username"
    p: "password"
  reqMinute: 8                  # Макс. запросов в минуту (ограничение скорости)
  parseDelay: 7000              # Задержка между запросами категорий/страниц в мс
  log: false                    # Логировать запросы этого трекера
  useproxy: false               # Направлять запросы через globalproxy
```

**Хосты по умолчанию:**

| Трекер | Хост по умолчанию |
|--------|------------------|
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

### Прокси

```yaml
globalproxy:
  - pattern: "\.onion"           # Регулярка: применять прокси когда URL совпадает
    list:
      - "socks5://127.0.0.1:9050"
      - "http://proxy.example.com:8080"
    useAuth: false
    username: ""
    password: ""
    BypassOnLocal: true          # Пропускать прокси для 127.x / 192.168.x / 10.x
```

---

## Эндпоинты парсеров

Все эндпоинты парсеров возвращают:

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

Мульти-категорийные парсеры также включают `"by_category": [...]`.

### Стратегии парсинга

Есть пять различных стратегий парсинга среди 20 парсеров:

#### 1. Одна страница (`page=N`)

Парсит ровно одну страницу. По умолчанию — страница 0 (самая свежая) для большинства трекеров; Bitru по умолчанию страница 1.

**Трекеры:** Rutor, Selezen, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka

> Примечание: Rutor, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka также поддерживают task-based парсинг (см. §4 ниже). Эндпоинт `parse?page=N` доступен как альтернатива для одной страницы.

```bash
# Парсить последнюю страницу (по умолчанию)
curl "http://127.0.0.1:9117/cron/rutor/parse"

# Парсить конкретную страницу
curl "http://127.0.0.1:9117/cron/kinozal/parse?page=3"
```

#### 2. Несколько страниц (`maxpage=N` или `limit_page=N`)

Парсит N страниц начиная с первой. `0` означает без лимита (все страницы).

**Трекеры:** Megapeer (`maxpage`, по умолч. 1), Animelayer (`maxpage`, по умолч. 1), Baibako (`maxpage`, по умолч. 10), Anistar (`limit_page`, по умолч. 0 = все), Leproduction (`limit_page`, по умолч. 0 = все)

```bash
# Megapeer/Animelayer/Baibako: по умолчанию = 1 страница
curl "http://127.0.0.1:9117/cron/megapeer/parse"

# Парсить до 5 страниц
curl "http://127.0.0.1:9117/cron/megapeer/parse?maxpage=5"

# Anistar/Leproduction: по умолчанию = все страницы (limit_page=0)
curl "http://127.0.0.1:9117/cron/anistar/parse"

# Ограничить до 3 страниц
curl "http://127.0.0.1:9117/cron/anistar/parse?limit_page=3"
curl "http://127.0.0.1:9117/cron/leproduction/parse?limit_page=3"
```

#### 3. Диапазон страниц (`parseFrom=N&parseTo=M`)

Парсит страницы от N до M включительно.

**Трекеры:** Anidub, Aniliberty, Selezen

```bash
# Парсить страницы с 1 по 5
curl "http://127.0.0.1:9117/cron/anidub/parse?parseFrom=1&parseTo=5"

# Парсить только страницу 1
curl "http://127.0.0.1:9117/cron/selezen/parse?parseFrom=1&parseTo=1"

# Aniliberty также возвращает lastPage в ответе
curl "http://127.0.0.1:9117/cron/aniliberty/parse?parseFrom=1&parseTo=3"
```

#### 4. Task-based (инкрементальный, рекомендуется для больших трекеров)

Для больших трекеров с сотнями страниц по категориям. Работает в три шага:

1. **Обнаружение** всех страниц по категориям и годам → сохраняет задачи в `Data/{tracker}_tasks.json`
2. **Парсинг всех** обнаруженных задач (можно прервать и возобновить)
3. **Парсинг последних** — ярлык для парсинга только N последних страниц

**Трекеры:** Rutor, Selezen, Bitru, Kinozal, NNMClub, RuTracker, TorrentBy, Toloka

```bash
# Шаг 1: Обнаружить все страницы и построить список задач (запустить однократно или периодически)
curl "http://127.0.0.1:9117/cron/kinozal/updatetasksparse"

# Шаг 2: Парсить все обнаруженные задачи (может занять много времени)
curl "http://127.0.0.1:9117/cron/kinozal/parsealltask"

# Или: Парсить только 5 последних страниц (быстрое ежедневное обновление)
curl "http://127.0.0.1:9117/cron/kinozal/parselatest"
curl "http://127.0.0.1:9117/cron/kinozal/parselatest?pages=10"

# Альтернатива: парсить одну конкретную страницу
curl "http://127.0.0.1:9117/cron/kinozal/parse?page=0"
```

Состояние задач сохраняется — прерванный `parsealltask` продолжит с того места, где остановился.

#### 5. Полный vs. инкрементальный (`fullparse=true/false`)

**Трекер:** только Anifilm

```bash
# Инкрементальный: только новые/обновлённые с последнего запуска (по умолчанию)
curl "http://127.0.0.1:9117/cron/anifilm/parse"
curl "http://127.0.0.1:9117/cron/anifilm/parse?fullparse=false"

# Полный переразбор: все страницы
curl "http://127.0.0.1:9117/cron/anifilm/parse?fullparse=true"
```

---

### Справочник по трекерам

#### Rutor
```
GET /cron/rutor/parse
  page=N   (по умолч. 0) — парсить страницу N по всем 11 категориям

GET /cron/rutor/updatetasksparse      — обнаружить все страницы по категориям
GET /cron/rutor/parsealltask          — парсить все обнаруженные задачи
GET /cron/rutor/parselatest
  pages=N   (по умолч. 5) — парсить последние N страниц по категориям
```
Парсит все 11 категорий: фильмы, музыка, сериалы, документальные, мультфильмы, аниме, спорт, украинский контент.
Без параметров `parse` получает одну страницу (page 0) из всех категорий одновременно.

#### Megapeer
```
GET /cron/megapeer/parse
  maxpage=N   (по умолч. 1) — парсить до N страниц
```

#### Anidub
```
GET /cron/anidub/parse
  parseFrom=N   (по умолч. 0) — начало диапазона страниц
  parseTo=M     (по умолч. 0) — конец диапазона страниц
```
Без параметров: парсит только страницу 0.

#### Aniliberty
```
GET /cron/aniliberty/parse
  parseFrom=N   (по умолч. 0) — начало диапазона страниц
  parseTo=M     (по умолч. 0) — конец диапазона страниц

Ответ включает: { ..., "lastPage": 42 }
```
Без параметров: парсит только страницу 0.

#### Animelayer
```
GET /cron/animelayer/parse
  maxpage=N   (по умолч. 1)
```

#### Anistar
```
GET /cron/anistar/parse
  limit_page=N   (по умолч. 0 = парсить все страницы)
  limitPage=N    (алиас)
```

#### Anifilm
```
GET /cron/anifilm/parse
  fullparse=false   (по умолч.) — только новые/обновлённые с последнего запуска
  fullparse=true    — переразобрать все страницы
```

#### Baibako
```
GET /cron/baibako/parse
  maxpage=N   (по умолч. 10)
```

#### Bitru
```
GET /cron/bitru/parse
  page=N   (по умолч. 1) — парсить одну страницу

GET /cron/bitru/updatetasksparse      — обнаружить все страницы категорий
GET /cron/bitru/parsealltask          — парсить все обнаруженные задачи
GET /cron/bitru/parselatest
  pages=N   (по умолч. 5) — парсить только последние N страниц
```

#### BitruAPI
```
GET /cron/bitruapi/parse
  limit=N   (по умолч. 100) — количество последних элементов через API

GET /cron/bitruapi/parsefromdate
  lastnewtor=YYYY-MM-DD   — получить элементы новее этой даты
  limit=N                  (по умолч. 100)
```

#### Kinozal
```
GET /cron/kinozal/parse
  page=N   (по умолч. 0) — парсить одну страницу

GET /cron/kinozal/updatetasksparse
GET /cron/kinozal/parsealltask
GET /cron/kinozal/parselatest
  pages=N   (по умолч. 5)
```

#### Knaben
```
GET /cron/knaben/parse
  from=N            (по умолч. 0) — смещение в результатах
  size=N            (по умолч. 300) — результатов на страницу
  pages=N           (по умолч. 1) — количество страниц
  query=string      — поисковый запрос
  hours=N           (0 = без фильтра по времени) — только за последние N часов
  orderBy=string    (по умолч. "date") — порядок сортировки
  categories=a,b,c  — категории через запятую
```

#### Leproduction
```
GET /cron/leproduction/parse
  limit_page=N   (по умолч. 0 = парсить все страницы)
```

#### Lostfilm
```
GET /cron/lostfilm/parse            — парсить главный каталог (последние релизы)

GET /cron/lostfilm/parsepages
  pageFrom=N   (по умолч. 1)
  pageTo=N     (по умолч. 1)

GET /cron/lostfilm/parseseasonpacks
  series=SeriesName   — парсить все сезонные паки для конкретного сериала

GET /cron/lostfilm/verifypage
  series=SeriesName   — проверить разобранные данные для сериала

GET /cron/lostfilm/stats            — статистика Lostfilm
```

#### Mazepa
```
GET /cron/mazepa/parse   — без параметров, парсит текущую страницу
```

#### NNMClub
```
GET /cron/nnmclub/parse
  page=N   (по умолч. 0)

GET /cron/nnmclub/updatetasksparse
GET /cron/nnmclub/parsealltask
GET /cron/nnmclub/parselatest
  pages=N   (по умолч. 5)
```

#### RuTracker
```
GET /cron/rutracker/parse
  page=N   (по умолч. 0)

GET /cron/rutracker/updatetasksparse
GET /cron/rutracker/parsealltask
GET /cron/rutracker/parselatest
  pages=N   (по умолч. 5)
```

#### Selezen
```
GET /cron/selezen/parse
  parseFrom=N   (по умолч. 0) — начало диапазона страниц
  parseTo=M     (по умолч. 0) — конец диапазона страниц

GET /cron/selezen/updatetasksparse      — обнаружить все страницы
GET /cron/selezen/parsealltask          — парсить все обнаруженные задачи
GET /cron/selezen/parselatest
  pages=N   (по умолч. 5) — парсить только последние N страниц
```
Без параметров `parse` парсит только страницу 1.

#### Toloka
```
GET /cron/toloka/parse
  page=N   (по умолч. 0)

GET /cron/toloka/updatetasksparse
GET /cron/toloka/parsealltask
GET /cron/toloka/parselatest
  pages=N   (по умолч. 5)
```

#### TorrentBy
```
GET /cron/torrentby/parse
  page=N   (по умолч. 0)

GET /cron/torrentby/updatetasksparse
GET /cron/torrentby/parsealltask
GET /cron/torrentby/parselatest
  pages=N   (по умолч. 5)
```

---

## API поиска

### `GET /api/v1.0/torrents`

Полнотекстовый поиск торрентов.

**Параметры запроса:**

| Параметр | Алиасы | Тип | Описание |
|----------|--------|-----|----------|
| `search` | `q` | string | Полнотекстовый поиск по title и name |
| `altname` | `altName` | string | Поиск по оригинальному/альтернативному названию |
| `exact` | — | bool | Точное совпадение вместо нечёткого |
| `type` | — | string | Тип контента: `фильм`, `сериал`, `аниме`, `музыка` и т.д. |
| `tracker` | `trackerName` | string | Фильтр по трекеру (напр. `Kinozal`) |
| `voice` | `voices` | string | Фильтр по озвучке |
| `videotype` | `videoType` | string | Фильтр формата видео (напр. `hdr`, `sdr`) |
| `relased` | `released` | int | Год выпуска (напр. `2023`) |
| `quality` | — | int | Код качества: `480`, `720`, `1080`, `2160` |
| `season` | — | int | Номер сезона |
| `sort` | — | string | Сортировка: `date`, `size`, `sid` |

```bash
# Базовый поиск
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Interstellar"

# Фильтр по трекеру и качеству
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Dune&tracker=Kinozal&quality=1080"

# Аниме по сезону
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Naruto&type=аниме&season=5"

# Точное совпадение названия
curl "http://127.0.0.1:9117/api/v1.0/torrents?search=Inception&exact=true"
```

**Ответ:**

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

Постраничный список метаданных качества.

| Параметр | По умолч. | Описание |
|----------|-----------|----------|
| `name` | — | Фильтр по имени |
| `originalname` / `originalName` | — | Фильтр по оригинальному имени |
| `type` | — | Фильтр по типу |
| `page` | `1` | Номер страницы |
| `take` | `1000` | Элементов на странице |

### `GET /api/v2.0/indexers/[indexer]/results`

Jackett-совместимый API для использования с Sonarr, Radarr и др.

| Параметр | Алиасы | Описание |
|----------|--------|----------|
| `query` | `q` | Поисковый запрос |
| `title` | — | Название для поиска |
| `title_original` | — | Оригинальное название |
| `year` | — | Год выпуска |
| `is_serial` | — | `1` для сериалов, `0` для фильмов |
| `category` | — | Префикс категории: `mov_`, `tv_`, `anime_` и т.д. |
| `apikey` | `apiKey` | API-ключ (если настроен) |

```bash
curl "http://127.0.0.1:9117/api/v2.0/indexers/jacred/results?q=Dune&year=2021"
```

Ответ: `{ "Results": [...], "jacred": true }`

### `GET /api/v1.0/conf`

Проверка API-ключа.

```bash
curl "http://127.0.0.1:9117/api/v1.0/conf?apikey=your-key"
# Ответ: {"apikey": true}
```

---

## API статистики

Эндпоинты статистики открыты (без API-ключа) когда `openstats: true`.

### `GET /stats/refresh`

Принудительно обновить `Data/temp/stats.json`

### `GET /stats/torrents`

Возвращает предвычисленную статистику из `Data/temp/stats.json` (пересчитывается каждые `timeStatsUpdate` минут).

| Параметр | Алиасы | По умолч. | Описание |
|----------|--------|-----------|----------|
| `trackerName` | — | — | Если задано: вычислить на лету для этого трекера |
| `newtoday` | `newToday` | `0` | `1` = только торренты, добавленные сегодня |
| `updatedtoday` | `updatedToday` | `0` | `1` = только торренты, обновлённые сегодня |
| `limit` | `take` | `200` | Макс. элементов |

```bash
# Полная статистика (из кэша)
curl "http://127.0.0.1:9117/stats/torrents"

# Новые торренты сегодня с Kinozal
curl "http://127.0.0.1:9117/stats/torrents?trackerName=Kinozal&newtoday=1"
```

### `GET /stats/trackers`

Сводная статистика по трекерам.

| Параметр | По умолч. | Описание |
|----------|-----------|----------|
| `newtoday` / `newToday` | `0` | Фильтр по новым торрентам сегодня |
| `updatedtoday` / `updatedToday` | `0` | Фильтр по обновлённым торрентам сегодня |
| `limit` / `take` | `200` | Макс. элементов |

```bash
curl "http://127.0.0.1:9117/stats/trackers"
curl "http://127.0.0.1:9117/stats/trackers?newtoday=1&limit=50"
```

### `GET /stats/trackers/{trackerName}`

Статистика конкретного трекера.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Rutor"
curl "http://127.0.0.1:9117/stats/trackers/Rutor?newtoday=1"
```

### `GET /stats/trackers/{trackerName}/new`

Торренты, добавленные сегодня с указанного трекера.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Kinozal/new?limit=100"
```

### `GET /stats/trackers/{trackerName}/updated`

Торренты, обновлённые сегодня с указанного трекера.

```bash
curl "http://127.0.0.1:9117/stats/trackers/Kinozal/updated"
```

---

## API синхронизации

Синхронизация между инстансами. Включается через `opensync: true` в конфиге.

### `GET /sync/conf`

Эндпоинт обнаружения — проверка версии протокола.

```json
{ "fbd": true, "spidr": true, "version": 2 }
```

### Протокол V2

#### `GET /sync/fdb`

Список ключей bucket-ов базы данных (низкоуровневый, для репликации).

| Параметр | По умолч. | Описание |
|----------|-----------|----------|
| `key` | — | Фильтр подстроки по ключу bucket (напр. `matrix`) |
| `limit` / `take` | `20` | Макс. записей |

```bash
curl "http://127.0.0.1:9117/sync/fdb?key=matrix&limit=5"
```

#### `GET /sync/fdb/torrents`

Инкрементальная синхронизация — возвращает торренты, изменённые после метки времени.

| Параметр | Алиасы | Обязат. | Описание |
|----------|--------|---------|----------|
| `time` | `fileTime` | **Да** | Вернуть только bucket-ы с fileTime > этого значения (формат Windows FILETIME) |
| `start` | `startTime` | Нет | Вернуть только торренты с updateTime > этого значения |
| `spidr` | — | Нет | `true` = вернуть только url/sid/pir метаданные (облегчённый ответ) |
| `take` | `limit` | Нет | Размер пакета (по умолч. 2000) |

```bash
# Начальная синхронизация (time=0 = получить всё)
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=0&take=2000"

# Инкрементальная синхронизация используя fileTime из предыдущего ответа
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=133476543210000000"

# Spider-режим (только метаданные, быстрее)
curl "http://127.0.0.1:9117/sync/fdb/torrents?time=0&spidr=true"
```

**Ответ:**

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

Когда `nextread: true`, вызовите снова с последним полученным `fileTime` для получения оставшихся данных.

### Протокол V1 (устаревший)

Включается через `opensync_v1: true`.

#### `GET /sync/torrents`

| Параметр | Алиасы | Обязат. | Описание |
|----------|--------|---------|----------|
| `time` | `fileTime` | **Да** | Фильтр по метке времени |
| `trackerName` | `tracker` | Нет | Фильтр по трекеру |
| `take` | `limit` | Нет | Размер пакета (по умолч. 2000) |

```bash
curl "http://127.0.0.1:9117/sync/torrents?time=0&trackerName=Rutor"
```

Ответ: плоский массив `{ "key": "name:originalname", "value": {...} }`

---

## Управление базой данных

### `GET /jsondb/save`

Принудительно сбросить базу данных из памяти на диск (`Data/masterDb.bz`).

```bash
curl "http://127.0.0.1:9117/jsondb/save"
# "work" — уже сохраняется
# "ok"   — сохранено успешно
```

Ежедневный бэкап `Data/masterDb_DD-MM-YYYY.bz` создаётся при каждом сохранении. Бэкапы старше 3 дней автоматически удаляются.

---

## Dev/Maintenance эндпоинты

Доступны только с **локального IP** (127.0.0.1, ::1, fe80::/10 link-local, fc00::/7 ULA, IPv4-mapped IPv6).

### Целостность данных

#### `GET /dev/findcorrupt`

Сканирует все bucket-ы на повреждённые записи.

| Параметр | Алиасы | По умолч. | Описание |
|----------|--------|-----------|----------|
| `sampleSize` | `sample`, `limit` | `20` | Макс. примеров на тип проблемы |

```bash
curl "http://127.0.0.1:9117/dev/findcorrupt"
curl "http://127.0.0.1:9117/dev/findcorrupt?sampleSize=50"
```

**Ответ:**
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

Удаляет записи где значение торрента = `null`.

```bash
curl "http://127.0.0.1:9117/dev/removeNullValues"
# Ответ: { "ok": true, "removed": 5, "files": 3 }
```

#### `GET /dev/findDuplicateKeys`

Находит ключи bucket-ов где name == originalname (потенциальные дубли).

| Параметр | Алиасы | По умолч. | Описание |
|----------|--------|-----------|----------|
| `tracker` | `trackerName` | — | Фильтр: только ключи с торрентами этого трекера |
| `excludeNumeric` | — | `true` | Исключить чисто числовые ключи |

```bash
curl "http://127.0.0.1:9117/dev/findDuplicateKeys"
curl "http://127.0.0.1:9117/dev/findDuplicateKeys?tracker=Kinozal&excludeNumeric=false"
```

#### `GET /dev/findEmptySearchFields`

Находит торренты с пустыми `_sn` (поисковое имя) или `_so` (поисковое оригинальное имя).

| Параметр | Алиасы | По умолч. | Описание |
|----------|--------|-----------|----------|
| `sampleSize` | `sample`, `limit` | `20` | Макс. примеров на категорию |

```bash
curl "http://127.0.0.1:9117/dev/findEmptySearchFields"
```

### Обновление данных

#### `GET /dev/updateDetails`

Пересчитывает `quality`, `videotype`, `voices`, `languages`, `seasons` для всех хранимых торрентов.

```bash
curl "http://127.0.0.1:9117/dev/updateDetails"
```

#### `GET /dev/updateSearchName`

Обновить поисковые поля для конкретного bucket.

| Параметр | Обязат. | Описание |
|----------|---------|----------|
| `bucket` | Да | Ключ bucket в формате `name:originalname` |
| `fieldName` | Нет | Поле для обновления |
| `value` | Нет | Новое значение |

```bash
curl "http://127.0.0.1:9117/dev/updateSearchName?bucket=матрица:the+matrix&fieldName=_sn&value=матрица"
```

#### `GET /dev/updateSize`

Обновить размер торрента в bucket.

| Параметр | Обязат. | Описание |
|----------|---------|----------|
| `bucket` | Да | Ключ bucket |
| `value` | Да | Размер с суффиксом: `500MB`, `14.7GB`, `1.2TB`, или в байтах |

```bash
curl "http://127.0.0.1:9117/dev/updateSize?bucket=матрица:the+matrix&value=14.7GB"
```

#### `GET /dev/resetCheckTime`

Сбрасывает `checkTime` на вчера для всех торрентов (форсирует повторную проверку при следующем парсинге).

```bash
curl "http://127.0.0.1:9117/dev/resetCheckTime"
```

### Исправления для конкретных трекеров

#### `GET /dev/fixKnabenNames`

Нормализует имена/года/названия торрентов Knaben и мигрирует в правильные ключи bucket.

```bash
curl "http://127.0.0.1:9117/dev/fixKnabenNames"
```

#### `GET /dev/fixBitruNames`

Очищает названия торрентов Bitru (убирает теги качества, кодек, релиз-группы).

```bash
curl "http://127.0.0.1:9117/dev/fixBitruNames"
```

#### `GET /dev/fixEmptySearchFields`

Автозаполнение пустых `_sn`/`_so` поисковых полей и миграция торрентов в правильные bucket-ы.

```bash
curl "http://127.0.0.1:9117/dev/fixEmptySearchFields"
```

#### `GET /dev/migrateAnilibertyUrls`

Добавляет `?hash=<btih>` к URL Aniliberty используя хеши из magnet-ссылок.

```bash
curl "http://127.0.0.1:9117/dev/migrateAnilibertyUrls"
```

#### `GET /dev/removeDuplicateAniliberty`

Дедупликация торрентов Aniliberty по magnet-хешу, сохраняя самый свежий.

```bash
curl "http://127.0.0.1:9117/dev/removeDuplicateAniliberty"
```

#### `GET /dev/fixAnimelayerDuplicates`

Нормализация http→https для URL Animelayer и удаление дублей по hex ID.

```bash
curl "http://127.0.0.1:9117/dev/fixAnimelayerDuplicates"
```

#### `GET /dev/fixKinozalUrls`

Нормализует http→https для Kinozal и удаляет дубликаты по `id=NNN`, сохраняя запись с самым свежим `updateTime`.

```bash
curl "http://127.0.0.1:9117/dev/fixKinozalUrls"
```

#### `GET /dev/fixSelezenUrls`

Нормализует host Selezen (старые хосты типа `open.selezen.org` → текущий `Selezen.Host` из конфига) и удаляет дубликаты по числовому ID, сохраняя запись с самым свежим `updateTime`.

```bash
curl "http://127.0.0.1:9117/dev/fixSelezenUrls"
```

### Управление bucket-ами

#### `GET /dev/removeBucket`

Удалить целый bucket (все торренты под ключом).

| Параметр | Обязат. | Описание |
|----------|---------|----------|
| `key` | Да | Ключ bucket в формате `name:originalname` |
| `migrateName` | Нет | Если задано: переместить торренты в этот новый name вместо удаления |
| `migrateOriginalname` | Нет | Новое originalname для цели миграции |

```bash
# Удалить bucket
curl "http://127.0.0.1:9117/dev/removeBucket?key=матрица:the+matrix"

# Переименовать/мигрировать bucket в новый ключ
curl "http://127.0.0.1:9117/dev/removeBucket?key=матрица:the+matrix&migrateName=the+matrix&migrateOriginalname=the+matrix"
```

---

## Горячая перезагрузка конфига

Файл `init.yaml` проверяется на изменения каждые 10 секунд. При изменении времени модификации файла конфигурация перезагружается автоматически — перезапуск не требуется.

Горячо-перезагружаемые настройки: API-ключи, флаги логирования, настройки синхронизации, интервал обновления статистики, хосты/cookie/учётные данные трекеров, лимиты скорости.

## Кэширование результатов поиска

Эндпоинты поиска (`/api/v1.0/torrents` и `/api/v2.0/indexers/*/results`) кэшируют результаты в памяти с TTL 5 минут. При попадании в кэш возвращается заголовок `X-Cache: HIT`. Ключ кэша включает полную строку запроса.

## FDB аудит-лог

При `logFdb: true` каждое изменение bucket записывается в `Data/log/fdb.YYYY-MM-dd.log` в формате JSON Lines. Каждая строка содержит входящие и существующие данные торрента для изменённой записи.

Ротация управляется параметрами `logFdbRetentionDays`, `logFdbMaxSizeMb` и `logFdbMaxFiles`. Очистка запускается автоматически после каждого сохранения masterDb.

## Логирование парсеров

Двухуровневое управление логированием:
1. **Глобальный гейт:** `logParsers: true` должен быть установлен для включения любого логирования парсеров
2. **Для трекера:** `log: true` в настройках каждого трекера включает логирование для этого конкретного трекера

Оба флага должны быть `true` для записи логов. Файлы логов хранятся в `Data/log/{tracker}.log`.

---

## Примеры cron

Типичный внешний crontab (`/etc/cron.d/jacred` или `Data/crontab`):

```cron
# Лёгкое ежедневное обновление — только последние страницы
0  6 * * *  root  curl -s "http://127.0.0.1:9117/cron/rutor/parselatest?pages=3" >/dev/null
5  6 * * *  root  curl -s "http://127.0.0.1:9117/cron/kinozal/parselatest?pages=3" >/dev/null
10 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/rutracker/parselatest?pages=3" >/dev/null
15 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/nnmclub/parselatest?pages=3" >/dev/null
20 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/torrentby/parselatest?pages=3" >/dev/null

# Аниме-трекеры — несколько страниц
30 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/animelayer/parse?maxpage=3" >/dev/null
35 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anidub/parse?parseFrom=1&parseTo=3" >/dev/null
40 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anistar/parse?limit_page=3" >/dev/null
45 6 * * *  root  curl -s "http://127.0.0.1:9117/cron/anifilm/parse" >/dev/null

# Еженедельный полный переразбор (task-based трекеры)
0  2 * * 0  root  curl -s "http://127.0.0.1:9117/cron/kinozal/updatetasksparse" >/dev/null
30 2 * * 0  root  curl -s "http://127.0.0.1:9117/cron/kinozal/parsealltask" >/dev/null

# Принудительное сохранение БД после тяжёлого парсинга
0  8 * * *  root  curl -s "http://127.0.0.1:9117/jsondb/save" >/dev/null
```

---

## Структура базы данных

```
Data/
  masterDb.bz               # Сжатый gzip JSON-индекс: key → {fileTime, updateTime, path}
  masterDb_DD-MM-YYYY.bz    # Ежедневные бэкапы (создаются автоматически, хранятся 3 дня)
  fdb/
    ab/cdef012...            # Bucket-файлы (сжатый gzip JSON): url → объект торрента
  temp/
    stats.json               # Предвычисленный кэш статистики
  log/
    YYYY-MM-DD.log           # Лог приложения (при log: true)
    kinozal.log              # Логи парсеров: добавлено/обновлено/пропущено/ошибка
    fdb.YYYY-MM-dd.log       # FDB аудит-лог: JSON Lines при каждом изменении bucket
  {tracker}_tasks.json       # Состояние задач для инкрементальных парсеров
```

Ключ каждого bucket-файла вычисляется из MD5 ключа bucket (`name:originalname`), разбитого на 2-символьный директорийный префикс.

### Поля объекта торрента

| Поле | Тип | Описание |
|------|-----|----------|
| `url` | string | URL страницы трекера (первичный ключ внутри bucket) |
| `title` | string | Полное название торрента как на трекере |
| `name` | string | Нормализованное русское/английское название |
| `originalname` | string | Оригинальное (обычно английское) название |
| `trackerName` | string | Имя трекера-источника |
| `relased` | int | Год выпуска |
| `size` | int64 | Размер в байтах |
| `sizeName` | string | Читаемый размер (напр. `14.0 GB`) |
| `sid` | int | Сиды |
| `pir` | int | Пиры/личи |
| `magnet` | string | Magnet-ссылка |
| `btih` | string | Info-хеш |
| `quality` | int | `480`, `720`, `1080`, `2160` |
| `videotype` | string | `sdr`, `hdr` |
| `voices` | string | Студии озвучки |
| `languages` | string | `rus`, `ukr` |
| `seasons` | string | Список сезонов, напр. `1-5` или `1,3,7` |
| `types` | []string | Теги типа контента |
| `createTime` | string | Метка времени первого обнаружения |
| `updateTime` | string | Метка времени последнего обновления |
| `checkTime` | string | Метка времени последней проверки/парсинга |
| `_sn` | string | Поисково-нормализованное имя |
| `_so` | string | Поисково-нормализованное оригинальное имя |

---

## Служебные эндпоинты

```
GET /health          → {"status": "OK"}
GET /version         → {"version": "...", "gitSha": "...", "gitBranch": "...", "buildDate": "..."}
GET /lastupdatedb    → метка времени последней записи в БД
GET /                → index.html (если web: true)
GET /stats           → stats.html (если web: true)
```
