# Disk Live Usage — личный Community App Store для umbrelOS

Аналог встроенного Live Usage (CPU/RAM по приложениям), только для диска.
Показывает в реальном времени (раз в 1 с, через SSE), **кто именно** читает и пишет на диск:

| Блок | Источник | Что видно |
|---|---|---|
| Диски | `/proc/diskstats` | чтение/запись MB/s, IOPS, загрузка % (io_ticks) |
| Приложения | cgroup v2 `io.stat` + Docker API | MB/s по каждому приложению Umbrel (контейнеры сгруппированы по `<app-id>_<service>_1`) |
| Хост-сервисы | `system.slice/*.service/io.stat` | `umbrel` (umbreld), `docker`, `containerd`… — только когда они что-то делают |
| Процессы | `/proc/<pid>/io` | топ-15 процессов хоста, с меткой приложения или systemd-юнита |
| Сводка | `/proc/stat`, `/proc/vmstat`, `/proc/meminfo` | iowait CPU, swap in/out, занятый swap, доступная память |

Ради последнего пункта всё и затевалось: на Raspberry Pi главным «пожирателем» диска оказался
`umbreld` (индексатор файлов umbrelOS, `node /opt/umbreld`), а это не контейнер — в `docker stats` его не видно.

![screenshot](docs/screenshot.jpg)

## Структура

```
umbrel-app-store.yml              id: homelab — префикс для всех приложений этого стора
homelab-disk-live-usage/
  umbrel-app.yml                  манифест (port 7841 на Umbrel)
  docker-compose.yml              app_proxy + сервис web
  Dockerfile                      Go → статический бинарник в scratch (~8–11 МБ образ)
  *.go, web/index.html            сборщик и UI (только стандартная библиотека Go, без JS-зависимостей)
dev/docker-compose.yml            локальный запуск без umbrelOS
.github/workflows/docker.yml      сборка arm64+amd64 образа в GHCR по тегу v*
```

Сборщик лёгкий: один проход по `/proc` и cgroupfs за тик, без запуска внешних команд, список контейнеров
из Docker API обновляется раз в 5 с. **Опрос идёт только пока открыта страница** (или кто-то дёргает
`/api/snapshot`) — закрыли вкладку, и приложение ничего не делает. На Mac (Docker Desktop) под нагрузкой: ~0.1 % CPU, 6.4 МиБ RAM;
на Pi замеров ещё не было, ожидаю порядка 1–2 % одного ядра.

## ⚠️ Безопасность: почему это для личного стора, а не для официального

Чтобы видеть процессы хоста и сопоставлять cgroup с приложениями, контейнер запускается с:

- `pid: host` — видит все процессы Umbrel, их командные строки (а там бывают пароли/токены в аргументах);
- `cap_add: SYS_PTRACE, DAC_READ_SEARCH` и `apparmor:unconfined` — нужно, чтобы ядро позволило читать `/proc/<pid>/io`
  чужих root-процессов (umbreld). В связке с `pid: host` это даёт техническую возможность подключиться (ptrace) к любому процессу хоста;
- `/var/run/docker.sock:ro` — **`:ro` на сокете ничего не защищает**: через сокет можно не только читать, но и запускать
  контейнеры с любыми правами. Приложение делает ровно один запрос `GET /containers/json`, но сам доступ к сокету = root на хосте.

Итого: **приложение эквивалентно root-доступу к вашему Umbrel**. Компенсирующие меры: `cap_drop: ALL` (оставлены
только две нужные capability), `read_only: true`, `no-new-privileges`, `mem_limit: 64m`, образ `scratch` без shell, нет исходящих
соединений, хост-пути смонтированы только на чтение и ничего не пишется. UI закрыт авторизацией Umbrel через `app_proxy`.
Код короткий (~800 строк Go) — его можно прочитать целиком перед установкой. В официальный Umbrel App Store такое
приложение, скорее всего, не примут — это вариант для личного community-стора.

Хотите уменьшить риск — поставьте рядом [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) с `CONTAINERS=1`
и задайте `DOCKER_HOST=tcp://homelab-disk-live-usage_docker_proxy_1:2375`: сервис умеет ходить в Docker и по TCP.
Без сокета вообще пропадут только имена приложений (диски и процессы останутся).

## Публикация образа

Образ: `ghcr.io/mrshakhov/umbrel-disk-live-usage` (linux/arm64 + linux/amd64). Его собирает GitHub Actions
([.github/workflows/docker.yml](.github/workflows/docker.yml)) при пуше тега `v*`:

```bash
git tag v0.1.1 && git push origin v0.1.1   # → ghcr.io/mrshakhov/umbrel-disk-live-usage:0.1.1
```

Новый выпуск: увеличьте `version` в `umbrel-app.yml` и тег образа в `docker-compose.yml`, закоммитьте, запушьте
и поставьте тег с тем же номером. Пакет в GHCR должен быть **публичным**
(GitHub → Packages → umbrel-disk-live-usage → Package settings → Change visibility → Public), иначе Umbrel не скачает образ.

Можно переименовать стор: поменяйте `id` в `umbrel-app-store.yml`, имя папки приложения, `id` в манифесте и `APP_HOST`
в compose — все четыре должны совпадать по префиксу.

## Установка в umbrelOS

1. Откройте **App Store** → меню **⋯** справа вверху → **Community App Stores**.
2. Вставьте URL репозитория (`https://github.com/MrShakhov/umbrel-disk-live-usage-store`) → **Add**.
3. Откройте появившийся **Homelab App Store** → **Disk Live Usage** → **Install**.
4. Приложение откроется на `http://umbrel.local:7841`.

После нового выпуска (см. выше) Umbrel сам предложит обновление.

## Локальный запуск (Mac/Linux с Docker)

```bash
docker compose -f dev/docker-compose.yml up --build
```

UI: <http://localhost:8080>, JSON: `curl localhost:8080/api/snapshot`.

На Mac всё работает, но показывает **виртуальную машину Docker Desktop**, а не сам Mac: диски `vda/vdb`,
процессы `dockerd`/`containerd` VM, контейнеры Docker Desktop. systemd-сервисов там нет, поэтому строк «host» не будет.

## Как читать цифры и известные ограничения

- MB = 10⁶ байт. Загрузка диска 100 % на HDD = очередь не успевает рассасываться.
- `/proc/<pid>/io` считает реальные обращения к диску: чтение из page cache не учитывается, а запись учитывается
  в момент «загрязнения» страницы, поэтому у процесса пик записи может опережать диск на несколько секунд (writeback).
- I/O завершившихся дочерних процессов ядро прибавляет к родителю — короткие `dd`/`ffmpeg` будут видны под своим родителем.
- cgroup `io.stat` учитывает и отложенную запись, поэтому сумма по приложениям ближе к цифрам дисков, чем сумма по процессам.
- **Не проверено на реальном Umbrel.** Ожидается cgroup v2 с systemd-драйвером Docker (`system.slice/docker-<id>.scope`),
  поддерживается и `docker/<id>`. Если внизу страницы появятся жёлтые заметки — там написано, чего не хватает
  (нет io.stat, Docker API недоступен, N процессов нечитаемы из-за AppArmor/прав и т. п.).

## Настройки (env в docker-compose.yml)

| Переменная | По умолчанию | |
|---|---|---|
| `INTERVAL` | `1s` | период опроса (минимум 250ms) |
| `TOP_PROCS` | `15` | сколько процессов показывать |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | или `tcp://host:port` |
| `CGROUP_ROOT` | `/host/cgroup` | куда смонтирован `/sys/fs/cgroup` хоста |
| `PROC_ROOT`, `SYS_ROOT` | `/proc`, `/sys` | |
| `LISTEN` | `:8080` | |
