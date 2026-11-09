# AI Agents Engineering (Prometheus AIAE101) — конспект курса

**Собрано 08.11.2026.** Источники: платформа Prometheus (структура курса и блоки
«Рекомендовані джерела» по всем 12 вебинарам), репозиторий курса
`dimetron/ai-ae-labs` (условия ДЗ, лекция дня 5, гайды, 18 паттернов, демо) и
собственные 12 сданных работ с измеренными результатами.

**Автор курса:** Дмитро Рашко — Software Architect, maintainer **KAgent**
(CNCF sandbox), contributor **AgentGateway**. Это объясняет, почему в курсе
есть демо с kagent и бонус-трек с agentgateway: оба проекта его.

**Чего здесь нет:** записи вебинаров и слайды (лежат на платформе, локально
была одна презентация из двенадцати). Всё остальное — конспект по материалам
и по тому, что реально получилось в лабах.

---

## 0. Статус и сроки (на 08.11.2026)

Оценка — среднее по 12 вебинарам, балл каждого делится на его максимум.
Текущая: **43%**, для сертификата нужно **60%**.

| ДЗ | Балл | Статус отправки на платформе |
|---|---|---|
| 1 | 100/100 | засчитано |
| 2 | 100/100 | засчитано |
| 3 | 55/145 | засчитано |
| 4 | 60/145 | засчитано |
| 5 | 80/100 | засчитано |
| 6 | 80/100 | засчитано |
| 7 | 80/100 | засчитано |
| 8 | 0/100 | **НЕЗАВЕРШЕНО, закрыто** (срок 2.11) |
| 9 | 0/100 | **НЕЗАВЕРШЕНО, закрыто** (срок 5.11) |
| 10 | — | ЗАВЕРШЕНО, ждёт проверки (срок 9.11) |
| 11 | — | ЗАВЕРШЕНО, ждёт проверки (срок 12.11) |
| 12 | — | ЗАВЕРШЕНО, ждёт проверки (срок 16.11) |

Если 10, 11 и 12 оценят по ≥67 — выходит ровно 60%. По 80 — 63%.
ДЗ 8 и 9 сделаны и лежат в репозитории, но форма на платформе не заполнена.

---

## 1. Стек и терминология

| Что | Версия на курсе | Заметка |
|---|---|---|
| Google ADK Go | v2.0.0 GA 30.06.2026; в репо `v2.5.0`, условия ДЗ 9–12 называют `v2.6.0` | расхождение реальное, собирать на том, что в `go.mod` |
| Go | 1.27.1 (ADK требует ≥1.26.6) | два разных числа, не выравнивать |
| MCP | спецификация 2025-11-25 | + расширение Tasks (2026-07-28) |
| A2A | v1.0.1 (26.05.2026) | Go-библиотека `adk-go` имеет свою версию |

Три стиля построения агентной системы, и выбор между ними — отдельный навык:

1. **Статический граф** — топология известна заранее. Узлы, рёбра, маршруты.
2. **Динамический граф** (`NewDynamicNode`) — число шагов выясняется в рантайме.
3. **Collaboration agents** — координатор + подагенты как инструменты.

Ось выбора: **кто решает, каким будет следующий шаг**. Код — статический граф.
Код в рантайме — динамический. Модель — coordinator.

---

## 2. Неделя 1. Фундамент: API, типизация, ландшафт

### Вебинар 1 — LLMs as a Runtime
Токеновая экономика (FinOps), системные промпты, контекстные окна, модельный ряд.
Практика: Cross-Model Benchmark Harness с калькулятором стоимости.

**Что важно на практике:**
- Классификация ошибок: transient (429, таймаут) против permanent (403, неверный
  аргумент). Ретраить нужно только первое.
- **Retry amplification**: два уровня ретраев по 4 попытки дают 16 запросов к
  падающему API. Держать один уровень, ретраи SDK выключать явно.
- Граница домена принадлежит коду, а не промпту. Отказ — это *declared step*,
  который попадает в лог, а не отсутствие ответа.

### Вебинар 2 — Stop Parsing Hope
Структурированный вывод через Go-структуры и JSON Schema, function calling.
Практика: Strict Schema Enforcer.

**Что важно на практике:**
- У Go нет опциональных полей: выведенная из структуры схема помечает всё как
  `required`, и вызовы без необязательного поля молча отклоняются до обработчика.
  Лечится тегом `json:"...,omitempty"`.
- Правило **«ремонт против отказа»**: привести `Prod` → `prod` безопасно;
  угадать `prod-eu` из `prod` — нет. Второе это догадка, а не нормализация.
- Ошибка инструмента обязана называть **конкретное поле и значение**, иначе
  следующая попытка модели будет такой же.

---

## 3. Неделя 2. ADK 2.0: граф и деплой

### Вебинар 3 — Agent Anatomy: граф как данные
Первый агент ADK как граф с типизированными узлами-инструментами.

**Ключевое:** `StateDelta` и event log. Запись в состояние должна идти через
`ctx.State().Set` — только тогда она становится `StateDelta` на событии
инструмента, из которого потом выводится аудит. `ctx.Session().State().Set`
не оставляет следа.

### Вебинар 4 — Ship the Agent
Один статический бинарник, HTTP-сервис со стримингом, готовый к Kubernetes.
*(Единственная презентация, которая была локально: 21 слайд.)*

**Ключевое со слайдов:**
- «Агент, которого нельзя вызвать по сети, не существует для других программ».
- Health-check должен быть дешёвым и **не** вызывать LLM: он проверяет
  доступность по стабильному адресу, а не качество ответа.
- `/readyz` отдаёт 200 в штатном режиме и 503 во время drain.
- Корректное завершение по `SIGTERM`: drain, ограниченное ожидание, честный код
  выхода. PID 1 и exec-форма ENTRYPOINT — иначе сигнал не доходит.
- `FROM scratch` + статическая сборка: образ порядка 15 МБ. **Измерять, а не
  угадывать** размер.

---

## 4. Неделя 3. Agentic GraphRAG

### Вебинар 5 — От PDF к графу знаний
Lossless parsing многотабличных PDF, ontology-driven chunking, entity resolution.

**Что важно на практике:**
- **Parent-Child chunking**: ищем по мелкому чанку, в модель отдаём родительский
  раздел. Поиск любит мелкое, ответ любит крупное.
- **Таблицы целиком или никак.** Таблица лимитов, разрезанная по строкам, даёт
  ответ, который звучит уверенно и неверен. В отчёт ingestion выводить
  `Tables: K/K preserved`.
- Ловушка: заголовок блока из парсера уже содержит номер раздела, и приклеивание
  номера даёт «1 1 Scope».

### Вебинар 6 — Agentic GraphRAG
Hybrid retrieval, multi-hop planning, re-ranking, semantic cache.

**Что важно на практике:**
- **Разнообразие важнее чистого скора.** Топ по релевантности целиком
  состоит из одной ветви, и второй факт теряется ровно тогда, когда нужен
  multi-hop. Нужна квота в топе на каждую ветвь.
- Сравнение по подстроке ловит ложные совпадения: `"t-2"` нашлось внутри
  `"ct-2025-014"` и увело ответ. Сравнивать по токенам.
- **Семантический кеш опаснее, чем кажется.** У лексического эмбеддера
  «инцидент T-1» и «инцидент T-2» похожи сильнее, чем перефраз того же вопроса.
  То есть кеш промахивается там, где должен попадать, и попадает там, где нельзя.
  Лечится лексической страховкой: совпадение вектора **и** идентификаторов.
- Порог похожести из чужого конспекта не переносится — калибровать на своих данных.

---

## 5. Неделя 4. Циклы и отказы

### Вебинар 7 — Inside the ReAct Loop
Thought → Action → Observation руками и в графе.

**ReAct-цикл — это двадцать строк `for`.** Три решения, которые выглядят
мелочами и ими не являются:
- Лимит итераций проверяется **перед** вызовом модели. После — последняя
  итерация успевает заплатить за шаг, который мы уже решили не делать.
- Порядок наблюдений равен порядку вызовов, и каждое несёт `CallID` своего
  вызова: модель может попросить три инструмента одним шагом.
- Без `history.Append` следующий шаг не видит результата и повторяет тот же
  вызов — самый частый способ получить цикл, который никогда не сходится.
- Trace возвращается **вместе с ошибкой**: в три ночи вопрос звучит «где
  застряло», и отвечает на него след, а не текст ошибки.
- Ноль в лимите итераций не означает «без ограничений»: это не настройка для
  цикла, который вызывает платную модель.

### Вебинар 8 — When Tools Fail + lethal trifecta
Транзиентные сбои против логических ошибок, самокоррекция, durable execution.

**Классификация — только через `errors.Is`/`errors.As`, никогда по тексту:**
```
context.Canceled        → Fatal          (отмена пришла сверху)
*SemanticError          → SurfaceToModel (ДО транзиентной!)
ErrTransient            → RetryTransient
default                 → Fatal          (неизвестная = фатальная)
```
Семантическая проверяется **до** транзиентной: одна ошибка может удовлетворять
обеим (обёртки складываются), и если выиграет транзиентная — harness начнёт
повторять неверный аргумент столько раз, сколько разрешит движок.
`DeadlineExceeded` транзиентный, `Canceled` фатальный: выглядят похоже,
означают противоположное.

**Durable execution (Temporal).** Повторы отдаются движку:
`RetryPolicy{NonRetryableErrorTypes: ["SemanticError"]}` — классификация
становится контрактом сервера, а не `if` в нашем цикле.

Измерено на практике: `kill -9` посреди вызова инструмента, воркер поднимается,
ход доходит сам. Счётчик вызовов модели: **3 → 2 (перед kill) → 3 (после)** —
то есть после восстановления добавился ровно один вызов, остальное Temporal
воспроизвёл из истории событий. Был бы перезапуск — было бы 5.

**Две ловушки:**
- `ActivityAsTool` отдаёт ошибку Activity прямо модели как ошибку инструмента,
  и семантический сбой перестаёт быть Observation. Нужна точка преобразования.
- Сентинелы не переживают сериализацию: после Activity остаётся только
  тип-строка, `errors.Is` по сентинелу не срабатывает.

**Lethal trifecta** (Simon Willison) — сочетание трёх свойств, при котором
агент становится эксплуатируемым: доступ к приватным данным + обработка
недоверенного контента + возможность внешней коммуникации. Убрать любое из трёх.

---

## 6. Неделя 5. Память и команды

### Вебинар 9 — Память, event-sourced состояние
**Запись в долговременную память — привилегированная операция.** OWASP ведёт это
как отдельный класс угроз (**ASI06, Memory & Context Poisoning**); работа
**MINJA** (arXiv:2503.03704) показала отравление банка памяти обычными запросами.

Механика:
- `TaintAfterTool("...")` помечает вызов «забруднено» после любого инструмента,
  вернувшего внешний текст. Пока метка стоит, запись отклоняется.
- **Кто пишет — решает программа.** У контракта записи нет поля «источник»:
  это ровно то поле, которое заполнила бы инъекция.
- Отказ — это Observation `{"status":"rejected","reason":…}`, а не ошибка Go.
  Ошибка оборвала бы ход, и модель не узнала бы причину.
- Журнал выводится из событий, второго источника правды нет.
- TTL на факте: факт про инфраструктуру обязан истекать.

**Деталь, которая стоит больше механики:** в лабе модель *сама* написала в
тексте ответа «это чужой текст, подтвердите» — и всё равно записала факт.
Инструкция сработала, гарантия — нет. Текст ответа и то, что попало в память,
две разные вещи.

**Цена памяти (измерено):** один факт в промпте — 54 токена, преамбула
memory-инструментов ADK — **115 токенов на каждый вызов, даже когда память
пуста**. Вдвое дороже самого факта.

### Вебинар 10 — Agents as Teams, human approval
**Главный вывод: решение по протоколу — это строка журнала, а не ответ в сессии.**
Ответ, который разбудил приостановленный прогон, — только сигнал, что кто-то
где-то ответил. Решение пишет отдельная команда с именем человека, один раз,
условным `UPDATE ... WHERE status='pending'`. Поэтому подделанный ответ в сессии
не может ничего одобрить.

- В вопросе человеку — **факты**: действие, получатели, хеш артефакта, диф,
  статус источников, пробелы. Не проза координатора.
- Три разные записи на одно следствие: `denied` / `timed_out` (автор
  `system:timeout`) / `invalid`. Журнал, где всё это «не одобрено», не отвечает
  на вопрос «нас отказали или мы проспали дедлайн».
- Одобрение **тратится отправкой**: повторный прогон не отправит дважды.
- `RerunOnResume: &true` обязателен. С `nil`/`&false` ответ человека уходит
  **следующему** узлу, и затвор не доходит до чтения журнала.
- HITL **нельзя** внутри параллельных ветвей: ADK вернёт
  `ErrParallelHITLUnsupported`.
- Чего затвор не умеет: дедлайн держит не он, а чья-то развёртка. Нет cron —
  нет отказа, вопрос висит. Лечится durable-слоем.

**Два противоположных текста, которые стоит читать вместе:**
Anthropic «How we built our multi-agent research system» (за) и Cognition
«Don't Build Multi-Agents» (против). Плюс arXiv:2503.13657 «Why Do Multi-Agent
LLM Systems Fail?».

---

## 7. Неделя 6. Динамика и самоулучшение

### Вебинар 11 — Dynamic Subagents, Multi-Critic
- **Объективная проверка первой, LLM-критики после.** Манифест/код, который не
  парсится, невозможно судить по стилю, а платить трём моделям за разбор
  безопасности файла с синтаксической ошибкой — самый дешёвый способ сжечь
  бюджет.
- Кворум **одностайный**, а не голосование: фокусы критиков не пересекаются,
  поэтому чистый вердикт о привилегиях ничего не говорит об отсутствующих
  лимитах памяти, и два «да» не перебивают одно «нет».
- Невалидный вердикт **отсекается**, а не «как-то интерпретируется»: неизвестная
  роль, подмена роли, `passed=false` без комментариев, `passed=true` с
  комментариями. Fail-closed: отклонённый вердикт никогда не даёт принятия,
  иначе самый дешёвый способ пройти панель — сломать критика.
- **Главная цифра отчёта — false accepts**: сколько раз цикл принял кандидата,
  оказавшегося ложным на holdout.

Измерено: 3 из 3 ложных принятий — и это результат, а не поломка. Панель поймала
всё, за что отвечает, а скрытые правила были вне зоны ответственности всех трёх
критиков. **Вывод не про промпт: нужен критик, который владеет этим классом правил.**

Побочное наблюдение: у объекта без пода (k8s Service) критики привилегий,
лимитов и секретов проходят тривиально — панель из трёх голосов тихо становится
панелью из нуля.

### Вебинар 12 — Autoresearch: улучшение под бюджет
Пять тормозов паттерна (`karpathy/autoresearch`):

1. **Ограниченная поверхность изменений.** Тип, а не `strings.HasPrefix` на
   месте записи: префиксная проверка пропускает traversal, абсолютный путь,
   симлинк на месте файла и симлинк в каталоге пути. Проверять и само
   объявление поверхности.
2. **Фиксированный бюджет.** Ноль отклоняется: читается как «дефолт», означает
   «без ограничений». Лимит проверяется **до** вызова модели.
3. **Одна объективная метрика.** Чистая функция без модели. Отдельный набор
   решает, отдельный только отчитывается. Кандидат, цитирующий ожидаемые
   ответы, отклоняется **до** измерения.
4. **Человеко-читаемый артефакт стратегии** — control point, который человек
   правит в PR, а не пост-фактум анализ.
5. **Полный журнал** — и keep, и discard. Журнал только из успехов — это журнал
   цикла, из которого никто ничего не узнал.

**Два вывода из прогона, которые дороже механики:**
- Жадный цикл keep/discard **не перелезает плато, которое требует двух изменений
  сразу**. Одиночный рычаг метрику не двигает, кандидат откатывается, следующий
  узкий шаг стартует с того же места.
- Цикл **трижды отбросил** изменение, улучшавшее результат на отложенном наборе:
  на основном метрика стояла. Подглядывать в отложенный набор правилом нельзя —
  он перестанет быть отложенным. Информацией, которой движку пользоваться
  запрещено, имеет право воспользоваться человек.

Плюс мелочь, которая тихо ломает: `0.54 − 0.5` в float64 даёт
`0.040000000000000036`, так что «выросла ровно на ε» без допуска решается
двоичным округлением — по-разному на разных машинах.

**Чего не хватает до продакшна (все три вне курса):**
- **Изоляция.** Проверка пути живёт внутри процесса, между проверкой и записью
  есть окно TOCTOU. Границу должно держать ядро: gVisor, Firecracker microVM,
  `kubernetes-sigs/agent-sandbox`, e2b.
- **Идентичность.** Журнал отвечает «что произошло», но не «кто имел право».
  SPIFFE/SPIRE + политика (OPA/Cedar) на каждом делегировании. Не передавать
  дочернему агенту полный токен родителя — confused deputy.
- **Durable execution.** Цикл живёт в памяти процесса; `kill -9` стирает
  состояние. Единственный из трёх, который в курсе доказан числами (ДЗ 8).

---

## 8. Каталог паттернов (demo/3_adk2_patterns, 18 штук)

| Код | Паттерн | Когда брать |
|---|---|---|
| a1 | single agent | один источник, один контекст; дешевле всего |
| a2 | ReAct | нужен цикл рассуждение→действие, шагов заранее не знаем |
| b1 | sequential pipeline | фиксированная цепочка этапов |
| b2 | fan-out / gather | несколько независимых источников, нужен барьер |
| b3 | loop | повтор до условия; всегда с бюджетом |
| b4 | conditional route | ветвление по типу запроса |
| b5 | custom logic | узел-функция без модели |
| c1 | coordinator | модель решает, кому делегировать |
| c2 | hierarchical decomposition | задача дробится на подзадачи рекурсивно |
| c3 | swarm | равные агенты без координатора |
| c4 | routed agent | маршрутизация к специалисту по домену |
| d1 | review / critique | один критик над результатом |
| d2 | iterative refinement | критик + переделка в цикле |
| e1 | human in the loop | пауза на схвалення перед риском |
| e2 | tool confirmation gate | подтверждение конкретного вызова инструмента |
| **e3** | **ambient agent** | **запускает событие, а не человек** |
| x | function graph | граф как данные, без агентов |
| x | guards | предохранители графа |

**e3 отдельно** — это обёртка развёртывания, а не паттерн оркестрации: внутрь
кладётся любой граф A–D. Форма: `очередь → пул воркеров → свежая сессия на
событие → граф → исход со сводкой, называющей причину каждого сбоя`.
Свежая сессия обязательна: два события в одной сессии смешиваются в одну
историю. Память между событиями — это memory-сервис, а не состояние сессии.

**Предохранители графа, которые ADK ловит на сборке** (`errors.Is`):
`ErrUnconditionalCycle` (цикл без маршрута), `ErrUnsupportedFanIn` (две
безусловные ветви в не-JoinNode), `ErrMultipleDefaultRoutes`,
`ErrNodesNotReachable`, `ErrDuplicateNodeName`, `ErrNoStartNode`.
В рантайме: `ErrParallelHITLUnsupported`, `ErrInvalidRunNodeContext`,
`ErrMultipleOutputs`.

**Ловушки, которые не ловит никто:**
- `RerunOnResume` трёхзначный: `&true` перезапускает узел с начала; `&false`
  **и `nil`** отдают ответ человека следующему узлу.
- Побочные эффекты до паузы выполняются **дважды**.
- `JoinNode` несёт только выходы ветвей — ничего из того, что было до fan-out.
  Контекст везти внутри выхода каждой ветви.
- Значения в join — `any`; приведение типа падает, нужен JSON round-trip.
- Событие не может нести и `Output`, и возврат узла одновременно.
- `loopagent.MaxIterations: 0` — бесконечный цикл, а не дефолт.

---

## 9. Практики multi-agent — то, что применять в своём продукте

Это выжимка для агента, который пишет код. Каждый пункт — следствие
измеренного, а не мнение.

### Границы
1. **Права — свойство учётных данных, а не промпта.** ClusterRole `view`, IAM
   `ReadOnlyAccess`, read-scope токены. Промпт «не удаляй» не является
   реализацией требования.
2. **Модель не вызывает инструменты из рассуждения.** Сбор фактов — отдельный
   узел, выполняемый до рассуждения, иначе в логе не видно, что прочитано.
3. **Аргументы инструментов — типизированные поля**, не строки из текста модели.
   Ремонт против отказа: нормализовать регистр можно, угадывать имя — нет.
4. **Policy-точка на вызове модели не является границей авторизации**, если код
   читает источник напрямую. Проверять в обоих местах, и **до** чтения,
   а не фильтровать результат на выходе.

### Ошибки и повторы
5. Классификация только через `errors.Is`/`errors.As`; порядок проверок —
   семантическая до транзиентной.
6. Один уровень повторов. Ретраи SDK выключать явно.
7. Повторы отдавать движку (`RetryPolicy`), а не писать свой `for` поверх.
8. Побочные эффекты идемпотентны по ключу запроса.

### Человек в контуре
9. Решение — строка журнала с именем человека, а не ответ в сессии.
10. В вопросе — факты, не проза. Три разные записи отказа.
11. Молчание = отказ **с названной причиной**; дедлайн держит движок таймеров,
    а не чей-то cron.
12. Одобрение тратится действием.

### Циклы и бюджеты
13. Лимит проверяется до вызова модели. Ноль — не «без ограничений».
14. Объективная проверка раньше LLM-критиков.
15. Кворум критиков одностайный; невалидный вердикт отсекается, fail-closed.
16. Метрика, которую предъявляют, — **false accepts**, а не «сколько итераций».

### Память
17. Память добавляется **после** политики записи, а не вместе с ней.
18. Кто пишет — решает программа; у контракта нет поля «источник».
19. Внешний текст помечается и не может переписать факт.
20. TTL на факте обязателен.

### Наблюдаемость
21. Журнал выводится из событий, второго источника правды нет.
22. Запись в состояние — через контекст инструмента, иначе следа нет.
23. Счётчики восстанавливать из журнала: если поток событий не является
    достаточной записью, «event log показывает траекторию» — это фраза,
    а не свойство.

### Что всегда говорить честно
24. Если провайдера нет — первая строка вывода обязана это называть, и обязана
    называть, чего прогон **не** доказывает.
25. Оценка токенов и измерение провайдера — разные колонки. Не складывать.

---

## 10. Код курса и запуск

```bash
git clone https://github.com/maxdcloudops/ai-ae-labs && cd ai-ae-labs
cp apps/.env-example apps/.env     # ключ опционален: без него работают заглушки

task check        # fmt + vet + build + test
task build:all    # включая вложенные модули демо
go test -race ./...
```

Вложенные Go-модули (из корня `go build ./...` их не видит):
`demo/1_ai-gateway/mock-llm/`, `demo/adk-quickstart/`.

Демо, которые стоит посмотреть:

| Каталог | Что |
|---|---|
| `demo/3_adk2_patterns/` | 18 паттернов, свой Taskfile |
| `demo/4_adk_examples/` | примеры по неделям 1–6 |
| `demo/5_kagent/` | kagent на Kind: скрипт из 10 шагов, проверен за 15 мин 23 с |
| `demo/1_ai-gateway/` | agentgateway + Jaeger/Prometheus/Grafana |
| `demo/9_cloudflare-durable-go/` | durable execution на Cloudflare |
| `demo/6_adk_telegram_bot/` | ⚠️ во flaky-тесте есть гонка, `-race` иногда краснеет |

Скилы для ИИ-агентов в `.agents/skills/`: `go-senior-developer`,
`asd-ste100`, `adk-go-workflow` (три стиля workflow, 18 паттернов,
шпаргалка API v2.5.0, предохранители, офлайн-тестирование).

Наши сданные работы: `submission/week<N>/day<M>/README.md` — там измеренные
числа, а не пересказ условий.

---

## 11. Источники преподавателя («Рекомендовані джерела» по вебинарам)


### W1 — LLMs as a Runtime
adk.dev/2.0/
github.com/google/adk-go
ollama.com
agentgateway.dev
developers.openai.com/api/docs/guides/migrate-to-responses  (Migrate to the Responses API)
arena.ai/leaderboard/agent  (LMArena)
artificialanalysis.ai  (порівняння моделей)
www.anthropic.com/research/building-effective-agents
huyenchip.com/2025/01/07/agents.html  (Chip Huyen — Agents)
simonwillison.net
youtu.be/Mzr7byMFy_4  (Graph Engineering with ADK)
КНИГИ: Chip Huyen «AI Engineering» (O'Reilly, 2025); Antonio Gulli «Agentic Design Patterns» (2025)

### W2 — Typed Outputs, Function Calls, Tool Contracts
landscape.aaif.io
platform.openai.com/docs/guides/structured-outputs
docs.anthropic.com/en/docs/build-with-claude/tool-use
modelcontextprotocol.io
github.com/dimetron/mono-go-mcp
ai.google.dev/gemini-api/docs/function-calling
ai.google.dev/gemini-api/docs/structured-output
arxiv.org/pdf/2608.06790  (AgentChaos: Chaos Engineering for Agent Systems)
www.anthropic.com/engineering/writing-tools-for-agents
openai.com/index/introducing-structured-outputs-in-the-api
cookbook.openai.com
eugeneyan.com/writing/llm-patterns/
ВІДЕО: The Future of MCP; AGNTCon + MCPCon Europe

### W3 — Agent Anatomy in Go ADK
google.github.io/adk-docs/
github.com/google/adk-go
pkg.go.dev/google.golang.org/adk/v2
github.com/google/adk-go/tree/main/examples/workflow
github.com/google/adk-go/blob/main/README-v2.md
modelcontextprotocol.io
www.anthropic.com/research/building-effective-agents
www.anthropic.com/engineering/writing-tools-for-agents
www.kaggle.com/whitepaper-agent-tools-and-interoperability
genai.owasp.org/llm-top-10/
developers.googleblog.com/build-zero-trust-ai-agents-with-googles-agent-development-kit

### W4 — Ship the Agent (один бінарник, одна межа)
google.github.io/adk-docs/deploy/
docs.docker.com/build/building/multi-stage/
12factor.net
cloud.google.com/run/docs
agentgateway.dev
kagent.dev
www.solo.io/blog/is-a-pod-the-right-deployment-unit-for-an-ai-agent
github.com/humanlayer/12-factor-agents
opentelemetry.io/docs/specs/semconv/gen-ai/
a2a-protocol.org/latest/specification/
ВІДЕО: ADK Course #4 (Run ADK Agents 4 ways); ADK Course #6 (Deploy to Cloud Run);
       Prototype to Production with ADK; Go 1.25 CPU throttling (lightning talk)

### W5 — Від PDF до графа знань
github.com/docling-project/docling · docling-project.github.io/docling/
github.com/unidoc/unipdf · github.com/pdfcpu/pdfcpu · unstructured.io
github.com/opendatalab/OmniDocBench · github.com/datalab-to/marker
arxiv.org/abs/2404.16130  (Microsoft GraphRAG — From Local to Global)
arxiv.org/abs/2409.04701  (Jina AI — Late Chunking)
www.anthropic.com/news/contextual-retrieval
neo4j.com/blog/genai/what-is-graphrag/
neo4j.com/labs/agent-memory/explanation/resolution-deduplication/
neo4j.com/knowledge-graphs-practitioners-guide/  (безкоштовна книга)
jessicatalisman.substack.com/p/knowledge-management-is-the-vibe

### W6 — Agentic GraphRAG
github.com/google/adk-go/tree/main/examples/workflow/complex
docs.cohere.com/docs/rerank-overview · huggingface.co/BAAI/bge-reranker-v2-m3
docs.ragas.io · github.com/pgvector/pgvector · github.com/microsoft/graphrag
github.com/zilliztech/GPTCache
hamel.dev  («Stop Saying RAG Is Dead», «"It's Hard to Eval" Is a Product Smell»)
weaviate.io/blog/retrieval-quality-rag-overview · weaviate.io/blog/hybrid-search-explained
www.pinecone.io/learn/ · eugeneyan.com · jxnl.co/writing/ · nlp.stanford.edu/IR-book/
КНИГИ: Huyen «AI Engineering»; Lakshmanan «Generative AI Design Patterns»;
       Manning/Raghavan/Schütze «Introduction to Information Retrieval» (безкоштовно);
       Grainger/Turnbull/Irwin «AI-Powered Search»

### W7 — ReAct як система управління
arxiv.org/abs/2210.03629  (ReAct) · arxiv.org/abs/2201.11903  (Chain-of-Thought)
github.com/google/adk-go/tree/main/examples/workflow/dynamic
www.anthropic.com/research/building-effective-agents
www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
lilianweng.github.io/posts/2023-06-23-agent/
openai.com/index/harness-engineering/
github.com/cobusgreyling/loop-engineering
datasciencedojo.com/blog/agentic-loops-explained-from-react-to-loop-engineering-2026-guide
tianpan.co/blog/2026-04-10-parallel-tool-calls-hidden-coupling
www.motomtech.com/blog-post/ai-agent-retries-idempotency-tool-failures

### W8 — Відмови інструментів, guardrails, lethal trifecta
simonwillison.net/2025/Jun/16/the-lethal-trifecta/  ← ключове
simonwillison.net/series/prompt-injection/
genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/
www.anthropic.com/engineering/writing-tools-for-agents
martinfowler.com/bliki/CircuitBreaker.html
modelcontextprotocol.io/extensions/tasks/overview  (MCP Tasks, 2026-07-28)
earendil.com/posts/pi-1-0/ · github.com/earendil-works/pi (pi-durable)
tailscale.com/blog/aperture-lethal-trifecta
blog.crewai.com/youre-building-agent-security-in-the-wrong-order/
greptime.com/blogs/2026-05-09-opentelemetry-genai-semantic-conventions
temporal.io/blog/introducing-deputy
КНИГИ: Albada «Building Applications with AI Agents»; Nygard «Release It!» (circuit breakers)

### W9 — Памʼять, event-sourced стан, context engineering
google.github.io/adk-docs/sessions/
github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md  (OKF v0.2)
gist.github.com/karpathy  (LLM Wiki)
arxiv.org/abs/2310.08560  (MemGPT) · docs.letta.com · agentskills.io/specification
www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
blog.langchain.com/context-engineering-for-agents/
www.dbreunig.com/2025/06/22/how-contexts-fail-and-how-to-fix-them.html
martinfowler.com/eaaDev/EventSourcing.html · letta.com/blog/context-repositories
КНИГА: Kleppmann «Designing Data-Intensive Applications» — dataintensive.net

### W10 — Команди агентів, human approval, write-safe
google.github.io/adk-docs/agents/multi-agents/ · a2a-protocol.org · docs.temporal.io
langchain-ai.github.io/langgraph/concepts/human_in_the_loop/
arxiv.org/abs/2503.13657  (Why Do Multi-Agent LLM Systems Fail?)  ← ключове
www.anthropic.com/engineering/built-multi-agent-research-system
cognition.ai/blog/dont-build-multi-agents  ← протилежна позиція, читати разом
temporal.io/blog/manetu-the-thread-is-the-workflow

### W11 — Динамічні субагенти і multi-critic
a2a-protocol.org · github.com/google/adk-go/tree/main/examples/a2a
adk.dev/workflows/collaboration/ · google.github.io/adk-docs/agents/workflow-agents/
github.com/google/adk-go/tree/main/examples/workflow/dynamic
www.anthropic.com/engineering/building-agents-with-the-claude-agent-sdk
github.com/SWE-agent/SWE-agent
www.langchain.com/blog/3-years-of-graph-engineering-with-langgraph
hamel.dev/blog/posts/evals/  (Your AI Product Needs Evals)
arxiv.org/abs/2306.05685  (Judging LLM-as-a-Judge)
arxiv.org/abs/2303.11366  (Reflexion)
linuxfoundation.org — A2A protocol, перший рік

### W12 — Autoresearch і самополіпшення під бюджет
github.com/karpathy/autoresearch  ← референс патерну
github.com/karpathy/nanochat
kagent.dev · github.com/kubernetes-sigs/agent-sandbox
e2b.dev · gvisor.dev · firecracker-microvm.github.io   ← три рівні ізоляції
deepmind.google — AlphaEvolve
arxiv.org/abs/2505.22954  (Darwin Gödel Machine) · sakana.ai/dgm/
lilianweng.github.io/posts/2026-07-04-harness/  (Harness Engineering)
cobusgreyling.substack.com/p/loop-engineering
fortune.com — «The Karpathy Loop»
shopify.engineering/autoresearch · www.kaggle.com/whitepaper-agent-quality
