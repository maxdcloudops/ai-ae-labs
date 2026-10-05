# ДЗ 4 — Агент виходить у мережу: HTTP-сервіс і бінарник `FROM scratch`

**Прогін: 05.10.2026.** Go 1.27.1 · `google.golang.org/adk/v2 v2.5.0` · Docker на macOS/arm64.

Граф ДЗ 3 (`prepare → open_refund_case → format`), загорнутий у REST через
`adkrest.NewServer`. Ключ провайдера не потрібен ні для сервісу, ні для контейнера.

## Збірка й запуск

```bash
# локально
go build -o /tmp/day4 ./week2/Day4_Agent_as_Service_Deploy/labs4
/tmp/day4                       # слухає 127.0.0.1:8080

# контейнер (контекст збірки — корінь репозиторію)
docker build -f week2/Day4_Agent_as_Service_Deploy/labs4/Dockerfile -t adk-service:0.1.0 .
docker run --rm -p 8081:8080 --name adk-service adk-service:0.1.0
```

Тег **`0.1.0`, не `latest`** — вимога Security зі сценарію: `latest` не
відтворюваний, учорашній і сьогоднішній образ під ним різні, і відкотитися нема до чого.

## Чотири curl-перевірки

Прогін проти контейнера, `BASE=http://127.0.0.1:8081`.

### 1. Liveness

```console
$ curl -fsS "$BASE/health"
OK
```

### 2. Readiness

```console
$ curl -fsS "$BASE/readyz"
OK
```

### 3. Створення сесії

```console
$ curl -fsS -X POST "$BASE/api/apps/first_graph_agent/users/learner/sessions/demo" \
    -H 'Content-Type: application/json' -d '{}'
{"id":"demo","appName":"first_graph_agent","userId":"learner","lastUpdateTime":1791230780,"events":[],"state":{}}
```

`events: []` і `state: {}` — на момент створення сесія порожня.

`appName` — **`first_graph_agent`**, тобто ім'я агента в коді, а не назва контейнера.

### 4. SSE-запит

```console
$ curl -fsS -N "$BASE/api/run_sse" -H 'Content-Type: application/json' \
    -d '{"appName":"first_graph_agent","userId":"learner","sessionId":"demo",
         "newMessage":{"role":"user","parts":[{"text":"Мерчант A-114 просить повернення по транзакції txn-2026-07-118845"}]}}'

data: {... "output":{"transaction_id":"txn-2026-07-118845","merchant_id":"A-114"},
       "actions":{"stateDelta":{}},
       "nodeInfo":{"path":"first_graph_agent@1/prepare@1"}}

data: {... "output":{"case_id":"rc-txn-2026-07-118845-A-114","status":"pending"},
       "actions":{"stateDelta":{"refund:last_case_id":"rc-txn-2026-07-118845-A-114",
                                "refund:last_merchant_id":"A-114",
                                "refund:last_status":"pending"}},
       "nodeInfo":{"path":"first_graph_agent@1/open_refund_case@1"}}

data: {... "output":"Кейс rc-txn-2026-07-118845-A-114: транзакція txn-2026-07-118845,
       мерчант A-114, статус pending",
       "actions":{"stateDelta":{}},
       "nodeInfo":{"path":"first_graph_agent@1/format@1"}}
```

`-N` вимикає буферизацію curl: події приходять по черзі, а не одним куском у кінці.

Три події — три вузли графа, і `nodeInfo.path` називає кожен явно. **`stateDelta`
непорожній рівно в одного** — `open_refund_case`. `prepare` і `format` лише
перетворюють дані й нічого не записують; якби запис з'явився в них, це був би баг.

### Ідемпотентність (повтор того самого запиту)

Локальний прогін, той самий запит двічі:

```
перший:  "refund:last_status":"pending"       "status":"pending"
другий:  "refund:last_status":"already_open"  "status":"already_open"
```

Кейс на ту саму транзакцію не створюється вдруге. Сервіс пам'ятає стан між
HTTP-запитами — інакше на один возврат завелося б два.

**Обмеження, яке варто назвати вголос:** стан живе **в пам'яті процесу**. Свіжий
контейнер знову віддає `pending`, бо реєстр порожній. Це навчальний мінімум без
durable storage, і умова це обумовлює окремо.

## Lifecycle

### Адреса прослуховування залежить від оточення

```
локально (PORT не задано):   ADK service listening at 127.0.0.1:8080
у контейнері (ENV PORT=8080): ADK service listening at [::]:8080
```

`listenAddress(PORT)`: у контейнері інакше не достукатися, на ноутбуці немає
причин світити порт у мережу.

### Graceful shutdown: SIGTERM і код виходу

```console
$ /tmp/day4
2026/10/05 15:57:18 ADK service listening at 127.0.0.1:8080 (no model credentials required)
2026/10/05 15:57:25 draining: rejecting new work and waiting for active requests
2026/10/05 15:57:25 ADK service drained and stopped

$ echo $?
0
```

Середній рядок і є дренаж: **перестали приймати нове, чекаємо активні запити**.
Вихід `0` — сервіс завершився штатно, його не вбивали.

У контейнері те саме через `docker stop adk-service`: ті самі два рядки, і команда
повертається за долі секунди.

### Чому `/readyz` не встигає віддати 503

Спроба побачити 503 на новому `curl` під час зупинки не вдасться:
`http.Server.Shutdown` закриває listener одразу, тож нове з'єднання отримає
`connection refused`. 503 побачить лише той, у кого з'єднання **вже відкрите**.
Це не баг, а порядок роботи `Shutdown`.

### Активний дренаж — тестом, а не Ctrl+C

Граф відпрацьовує за мілісекунди, тож у момент сигналу активних запитів зазвичай
немає, і живий Ctrl+C нічого не доводить. Для цього є тест з **утримуваним** запитом:

```console
$ go test -v -run '^TestShutdown$' ./week2/Day4_Agent_as_Service_Deploy/labs4
=== RUN   TestShutdown/drains_active_request
2026/10/05 16:01:29 draining: rejecting new work and waiting for active requests
=== RUN   TestShutdown/timeout_closes_active_request
2026/10/05 16:01:29 draining: rejecting new work and waiting for active requests
--- PASS: TestShutdown (0.00s)
    --- PASS: TestShutdown/drains_active_request (0.00s)
    --- PASS: TestShutdown/timeout_closes_active_request (0.00s)
```

| Підтест | drain-timeout | Результат |
|---|---|---|
| `drains active request` | 5 с | `err == nil`, клієнт отримав повну відповідь |
| `timeout closes active request` | 0 | `context.DeadlineExceeded`, з'єднання розірвано |

`0.00s` тут не означає «нічого не сталося»: 5 секунд — це **стеля очікування**, а не
`sleep` (як `terminationGracePeriodSeconds` у k8s). Рядок `draining:` надрукувався
двічі — по разу на підтест. Найцінніше твердження тесту — що `Shutdown` **не
повернувся раніше**, ніж дожив активний запит; інакше тест падає з
`returned before active request completed`.

## Контейнер

### Виміряний розмір

```console
$ docker images adk-service:0.1.0 --format '{{.Repository}}:{{.Tag}}  {{.Size}}'
adk-service:0.1.0  22.2MB

$ docker image inspect adk-service:0.1.0 --format '{{.Os}}/{{.Architecture}}'
linux/arm64
```

**22.2 MB, linux/arm64**, виміряно 05.10.2026. Базовий образ збірки
`golang:1.27.1` — близько гігабайта; у фінальному шарі лишилися тільки бінарник і
CA-сертифікати.

### Що дає кожен рядок Dockerfile

| Рядок | Навіщо |
|---|---|
| `CGO_ENABLED=0` | статичний бінарник: у `scratch` немає libc, динамічний просто не стартує |
| `-trimpath -ldflags="-s -w"` | прибирає шляхи збірки й таблиці налагодження — менший розмір і менше зайвого про хост |
| `FROM scratch` | порожній образ: ні shell, ні пакетного менеджера, ні утиліт |
| `COPY ... ca-certificates.crt` | у `scratch` немає кореневих сертифікатів **взагалі** |
| `USER 65532:65532` | не root |
| `ENTRYPOINT ["/adk-service"]` | **exec-форма** — бінарник сам PID 1 і отримує сигнали |
| `STOPSIGNAL SIGTERM` | явно, а не за замовчуванням |

Про CA окремо: поточний граф назовні не ходить, тож без цього рядка все одно
працювало б. Але щойно з'явиться HTTPS-інструмент — буде
`x509: certificate signed by unknown authority`, і шукати причину доведеться в
порожньому образі без жодної утиліти.

### Пастка з PID 1 — спіймана наживо

Перша спроба показати graceful shutdown провалилася, і це варто записати.

Сервіс запускали через `go run`, а SIGTERM надіслали знайденому PID:

```console
$ kill -TERM $(pgrep -f 'labs4' | head -1)
[1]  4538 terminated  go run ./week2/Day4_Agent_as_Service_Deploy/labs4
```

Рядка `drained and stopped` не було — лише `terminated`. `go run` компілює
бінарник у тимчасову теку й запускає його **дочірнім** процесом: сигнал отримав
батько (4538), а сервіс жив у дитині. Наступний запуск це й підтвердив:

```console
$ lsof -nP -iTCP:8080 -sTCP:LISTEN
labs4   4548  ...  TCP 127.0.0.1:8080 (LISTEN)     ← осиротіла дитина тримає порт
```

Це та сама природа, що й shell-форма `ENTRYPOINT` у контейнері: PID 1 стає `sh`,
сигнал до застосунку не доходить, і `docker stop` чекає 10 секунд та б'є `SIGKILL`.
У Dockerfile лаби форма exec саме тому.

> Проблема відома й авторам курсу: в `AGENTS.md` репозиторію є задача
> `task kill:8080` з поясненням, що «перерваний `go run` часто лишає дочірній
> процес живим».

Правильний спосіб перевірити — зібрати бінарник і сигналити йому, без обгортки:

```bash
go build -o /tmp/day4 ./week2/Day4_Agent_as_Service_Deploy/labs4
/tmp/day4 &
kill -TERM $(pgrep -f '^/tmp/day4$')
```

## Межі цієї роботи

Навчальний мінімум, і варто назвати, чого тут немає:

- **немає app-level авторизації** — `/api/` відкритий усім, хто дістав порт;
- **немає durable storage** — реєстр кейсів у пам'яті, рестарт його втрачає;
- **немає rate limiting** — SSE-з'єднання тримаються скільки завгодно;
- `/debug/pprof` не піднімався; якби піднімався — лише на `127.0.0.1`.
