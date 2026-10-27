# Seans Worker

Worker для self-hosted медиасервиса Seans. Скачивает торренты через qBittorrent и заливает файлы в S3 через rclone.

## Как работает

1. Регистрируется на backend API при старте
2. Каждые N секунд запрашивает новую задачу (claim)
3. Выполняет задачу: preparing → downloading → uploading → done
4. Отправляет heartbeat с прогрессом всех активных задач
5. Graceful shutdown по SIGTERM/SIGINT

### `file_paths` в claim

Поле `file_paths` из `POST /api/worker/claim` — фильтр файлов торрента из `POST /api/tasks`:

- **`[]` / отсутствует** — полный торрент (медиа-файлы; sample/trailer по умолчанию не качаются).
- **непустой список** — скачать/залить **только** эти пути. Совпадение как на бэке: точный путь после нормализации `\`→`/`, либо suffix-совпадение по `/` (короткое имя `ep01.mkv` матчится с `Show/Season 1/ep01.mkv` и наоборот). В manifest и complete уходят только выбранные файлы. Если ни один путь не совпал — задача фейлится (`no files matched task.file_paths`), полный торрент не качается.

## Быстрый старт

```bash
# 1. Скопировать и заполнить конфиг
cp .env.example .env
# Заполнить QBIT_API_KEY, BACKEND_URL, WORKER_REGISTER_TOKEN

# 2. Запустить (docker compose)
make compose-up

# 3. Проверить логи
docker compose logs -f agent
```

### Первый запуск qBittorrent

При первом запуске нужно сгенерировать API key в Web UI qBittorrent:
Settings → Web UI → Authentication → Generate new password/API key.
Вписать ключ в `.env` как `QBIT_API_KEY`, затем перезапустить:
```bash
make compose-down && make compose-up
```

### Доступ к Web UI qBittorrent

| Способ | URL | Когда работает |
|---|---|---|
| Локально | `http://localhost:8080` | Всегда, сразу после `compose-up` |
| По домену | `https://<WORKER_NAME>.w.seans.tedeshi.ru` | DNS `*.w.seans.tedeshi.ru` → IP машины + порт 443 |

Домен формируется из `WORKER_NAME` в `.env`. Например, при `WORKER_NAME=home-pc`
Web UI будет на `https://home-pc.w.seans.tedeshi.ru`.

Для доступа снаружи нужно, чтобы `*.w.seans.tedeshi.ru` резолвился в IP этой
машины, и порты 80/443 были доступны (port forwarding / публичный IP / tunnel).

## Режим разработки (без S3 и реального backend)

Mock backend + локальная загрузка вместо S3:

```bash
# В .env:
# MOCK_S3=true
# BACKEND_URL=http://host.docker.internal:9999

# Запустить mock backend на хосте
make mock

# В другом терминале — docker compose
make compose-up
```

### Тестовый торрент

Для разработки используйте официальный образ Ubuntu:
`https://releases.ubuntu.com/24.04/ubuntu-24.04.3-desktop-amd64.iso.torrent`

Настройте mock backend через переменные окружения:
```bash
export MOCK_TORRENT_KIND=file_b64
export MOCK_TORRENT_DATA=<URL .torrent файла>
export MOCK_DEST_PREFIX=media/users/u_mock/md_ubuntu/s1/
export MOCK_MAX_BYTES=5368709120
```

## Команды

| Команда | Описание |
|---------|----------|
| `make build` | Собрать бинарники в `bin/` |
| `make test` | Запустить тесты |
| `make vet` | Проверить `go vet` |
| `make compose-up` | Запустить qbit + agent через docker compose |
| `make compose-down` | Остановить все сервисы |
| `make mock` | Запустить mock backend на хосте |

## Переменные окружения

| Переменная | По умолчанию | Описание |
|---|---|---|
| `BACKEND_URL` | — | URL backend API (обязательно) |
| `WORKER_REGISTER_TOKEN` | — | Общий секрет для register (обязательно) |
| `WORKER_NAME` | hostname ОС | Имя воркера; также задаёт домен Web UI `<WORKER_NAME>.w.seans.tedeshi.ru` |
| `QBIT_URL` | `http://qbittorrent:8080` | Web API qBittorrent |
| `QBIT_USER` | `admin` | Логин qBittorrent |
| `QBIT_PASS` | — | Пароль qBittorrent (обязательно) |
| `QBIT_DOWNLOADS` | `/downloads` | Путь загрузок внутри контейнера qbit |
| `STAGING_DIR` | `/staging` | Путь staging внутри контейнера агента |
| `STAGING_HOST_DIR` | — | Host-каталог, монтируемый в оба контейнера |
| `S3_ACCESS_KEY` | — | Ключ S3 (buckets.ru) |
| `S3_SECRET_KEY` | — | Секрет S3 (buckets.ru) |
| `S3_ENDPOINT` | `https://s3.buckets.ru` | S3 API endpoint |
| `S3_BUCKET` | `seans` | Имя бакета |
| `S3_REGION` | `ru-1` | Регион S3 |
| `MOCK_S3` | `false` | Локальная загрузка вместо S3 |
| `CLAIM_INTERVAL_SEC` | `10` | Интервал запроса задач (сек) |
| `MAX_CONCURRENT_TASKS` | `1` | Макс. параллельных задач |
| `HEARTBEAT_INTERVAL_SEC` | `60` | Интервал heartbeat (сек) |
| `DOWNLOAD_STALL_TIMEOUT_MIN` | `30` | Таймаут простоя загрузки (мин) |
| `TORRENT_METADATA_TIMEOUT` | `300` | Таймаут получения метаданных (сек) |
| `TASK_MAX_HOURS` | `12` | Макс. время выполнения задачи (часы) |
| `DRAIN_ON_SHUTDOWN` | `false` | Дождаться завершения задач при shutdown |
| `DRAIN_TIMEOUT_SEC` | `3600` | Таймаут drain при shutdown (сек) |
| `STATE_DIR` | `/state` | Каталог для rclone.conf и логов |
| `STREAM_SEGMENT_SIZE_MB` | `8` | Размер сегмента для progressive-заливки в S3 (МиБ) |

## Архитектура

```
cmd/agent/main.go          — entrypoint воркера
cmd/mockbackend/main.go    — mock backend для разработки
internal/config/            — конфигурация из env
internal/backend/           — HTTP клиент к backend API
internal/qbit/              — клиент qBittorrent Web API v2
internal/engine/            — цикл worker'а и state machine задачи
internal/media/             — разбор имён файлов (сериалы, озвучки, субтитры)
internal/stream/            — piece map → seekable prefix, сегменты для progressive upload
internal/s3uploader/        — генерация rclone.conf, вызов rclone move/copyto
internal/sysutil/           — утилиты (disk free)
```

## Стек

- Go 1.23+, стандартная библиотека
- rclone (subprocess) для заливки в S3
- qBittorrent Web API v2
- Docker + docker compose