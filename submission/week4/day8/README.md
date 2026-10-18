# ДЗ 8 — Fault-Tolerant Tool Harness

**Прогін: 18.10.2026.** Go 1.27.1 · `google.golang.org/adk/v2 v2.5.0` ·
`go.temporal.io/sdk v1.49.0` + `contrib/googleadk v0.3.0` · Temporal CLI 1.9.1 (dev server на SQLite).

Реалізація:
[`week4/Day8_Fault_Tolerant_Tool_Harness/labs8/main.go`](../../../week4/Day8_Fault_Tolerant_Tool_Harness/labs8/main.go) ·
тести: [`harness_test.go`](../../../week4/Day8_Fault_Tolerant_Tool_Harness/labs8/harness_test.go).

```bash
cd week4/Day8_Fault_Tolerant_Tool_Harness/labs8
go test ./...     # 15 тестів, без мережі
```

## Що зроблено, а що ні — одразу

| Критерій | Балів | Стан |
|---|---:|---|
| Класифікація через `errors.Is/As` | 15 | ✅ |
| Observation + бюджет самокорекції з явною помилкою | 15 | ✅ |
| Durable-цикл на Temporal (`AgentWorkflow`, Activity, ідемпотентність) | 20 | ✅ |
| Доказ crash-safe (`kill -9`, лічильник LLM не зріс) | 10 | ✅ |
| Персистентна сесія в SQLite | 10 | ❌ workflow бере `InMemoryService` |
| Персистентна пам'ять + recall після рестарту | 10 | ❌ Dgraph не піднімався |

**60 із 80 базових балів.**

## Класифікація помилок

Три класи, і різниця між ними — це різниця між «спробуй ще», «скажи моделі» і
«зупинись».

```go
switch {
case errors.Is(err, context.Canceled):        → ClassFatal
case errors.As(err, &ledger.SemanticError{}): → ClassSurfaceToModel
case errors.Is(err, ledger.ErrTransient):     → ClassRetryTransient
default:                                      → ClassFatal
}
```

**Ніколи по тексту помилки.** `errors.Is`/`errors.As` розгортають обгортки:
семантична помилка, загорнута в `fmt.Errorf("open case: %w", …)`, лишається
семантичною. Перевірка по рядку зламалася б від першого ж додаткового
префікса — і зламалася б тихо.

### Три рішення, які виглядають дрібницями

**`context.Canceled` — перший і фатальний.** Скасування приходить *згори*:
користувач закрив вкладку, деплой зупиняє под. Повторювати його означає
витрачати гроші на відповідь, яку ніхто не прочитає.

**`DeadlineExceeded` — транзієнтний, на відміну від `Canceled`.** Виглядають
схоже, означають протилежне: дедлайн це «цього разу не встигли», скасування —
«більше не треба».

**Семантична перевіряється ДО транзієнтної.** Помилка може задовольняти обидві
(обгортки складаються), і тоді вирішує порядок. Якщо виграє транзієнтна,
harness почне повторювати неправильний аргумент і повторить його стільки
разів, скільки дозволить рушій, жодного разу не наблизившись до успіху.
Закріплено `TestSemanticErrorBeatsTransient`.

**Невідома помилка — фатальна.** Повторити наосліп означає ризикнути другим
возвратом: запис міг пройти, а відповідь не доїхати.

## Самокорекція

Семантична помилка **не повертається як error** — вона стає спостереженням:

```json
{"status":"invalid_argument","field":"merchant_id",
 "observation":"поле merchant_id: порожній","attempt":1}
```

Модель читає `field` і `observation` і виправляє наступний хід. Це та сама
теза, що в ДЗ 2: помилка має називати конкретне значення, інакше наступна
спроба буде такою самою.

Бюджет — **3 самокорекції на хід**. Четверта дає явну помилку з
`ErrSelfCorrectionExhausted`, а не тиху зупинку: модель, яка тричі поспіль
подала неправильний аргумент, не розуміє контракт, і з четвертої спроби не
виправиться. Мовчазне припинення тут виглядало б як «нічого не знайдено».

Транзієнтна помилка бюджет **не витрачає** (`TestObserveSurfacesTransientAsUnavailable`):
недоступність джерела — не помилка моделі, і карати її за це нема за що. Після
вичерпаних повторів модель бачить `status: "unavailable"` — щоб не вигадала
відповідь і не повторила виклик власноруч.

### Лог самокорекції

```
хід 1: open_refund_case(merchant_id="")       → invalid_argument, field=merchant_id, attempt=1
хід 2: open_refund_case(merchant_id="A-114",
                        txn="")               → invalid_argument, field=txn,         attempt=2
хід 3: open_refund_case(merchant_id="A-114",
                        txn="txn-2026-07-118845") → ok, case_id=rc-…
```

Третій хід проходить. Якби модель помилилася ще раз, четвертий дав би
`self-correction budget exhausted: 3 семантичних помилок поспіль`.

## Тести

**15 тестів**, усі офлайн.

| Тест | Сценарій збою |
|---|---|
| `TestClassifyError` | 9 кейсів: скасування, дедлайн, семантична типом і сентинелом, транзієнтна, **обгорнуті** обидві, невідома, nil |
| `TestSemanticErrorBeatsTransient` | помилка задовольняє обидві перевірки — виграє семантична |
| `TestObserveTurnsSemanticIntoObservation` | семантична стає Observation, а не error |
| `TestSelfCorrectionBudgetIsExhaustedLoudly` | 4-та помилка → `errors.Is(err, ErrSelfCorrectionExhausted)` |
| `TestObserveSurfacesTransientAsUnavailable` | транзієнтна → `unavailable`, бюджет не витрачено |
| `TestObserveStopsOnCancel` | скасування не стає спостереженням |

Умова вимагає **≥3 офлайн-сценарії збоїв** як обов'язкову умову — тут їх
більше, і кожен перевіряє окреме рішення класифікатора, а не «воно працює».

## Durable-цикл

```bash
temporal server start-dev --db-filename .data/temporal.db   # сервер
LABS8_MODEL=demo go run . worker                            # воркер
go run . start -session s1 "Open a refund for txn-2026-07-118845 at merchant A-114"
go run . counter
```

Код: [`durable.go`](../../../week4/Day8_Fault_Tolerant_Tool_Harness/labs8/durable.go) (workflow і Activity),
[`cmd_durable.go`](../../../week4/Day8_Fault_Tolerant_Tool_Harness/labs8/cmd_durable.go) (воркер, start, counter).

Агент **не переписувався**. Змінилися рівно дві речі, як і обіцяє контриб:
модель стала `googleadk.NewModel(modelName)` (виклик їде в Activity, ключі
лишаються на воркері), а контекст — `googleadk.NewContext(workflowCtx)`
(детерміновані час і UUID).

### Повтори віддані рушію

```go
RetryPolicy: &temporal.RetryPolicy{
    InitialInterval: time.Second, BackoffCoefficient: 2, MaximumAttempts: 4,
    NonRetryableErrorTypes: []string{"SemanticError"},
}
```

Це той самий `retry`, що був стенд-іном у `main.go`, але тепер цикл живе **на
сервері** й переживає падіння воркера. Класифікація з пункту 1 стала
контрактом рушія: Activity повертає семантичний збій як
`temporal.NewApplicationError(msg, "SemanticError", field)`, і рушій його
**не повторює**.

### Дві пастки, які коштували часу

**`ActivityAsTool` не підійшов.** Він вимагає рівно `(context.Context, TArgs)`,
а метод із ресивером дає три аргументи — граф не збирається. Activity стали
функціями пакета зі станом воркера в `activities`.

Глибша проблема: `ActivityAsTool` віддає помилку Activity прямо моделі **як
помилку інструмента**, і семантичний збій перестає бути Observation. Потрібна
точка перетворення, тож інструмент зібрано звичайним `functiontool.New`, а
Activity викликається всередині через `workflow.ExecuteActivity` — туди
дістається `googleadk.WorkflowContext(actx)`.

**Сентинели не переживають серіалізацію.** `ledger.ErrSemantic` після Activity
стає `ApplicationError` із типом-рядком, і `errors.Is` по сентинелу більше не
спрацьовує. Тому `unwrapActivityError` відновлює `*ledger.SemanticError` із
типу й деталей, які Activity сама й поклала.

## Доказ crash-safe

Лічильник викликів моделі живе **поза процесом**, у `.data/agent.db`: якби він
був у пам'яті, `kill -9` обнулив би його разом із процесом і порівнювати було
б нічого.

```console
# 1. Контрольний хід без падіння
$ ./labs8 start -session base "Open a refund for txn-2026-07-118845 at merchant A-114"
[ev:success] Refund case rc-txn-2026-07-118845-A-114 is open …
$ ./labs8 counter
llm_calls=3 cases=1

# 2. Той самий хід, але інструмент повільний
$ LABS8_MODEL=demo LEDGER_LATENCY=15s ./labs8 worker &
$ ./labs8 start -session crash "Open a refund for txn-2026-07-118845 at merchant A-114" &

# 3. Поки хід висить усередині tool call
$ ./labs8 counter
llm_calls=2 cases=0          ← дві моделі відпрацювали, кейс ще не створено

$ pkill -9 -f "labs8 worker"
воркер убитий: 14:11:55

# 4. Піднімаємо воркер заново — хід доходить сам
$ LABS8_MODEL=demo ./labs8 worker &
[ev:success] Refund case rc-txn-2026-07-118845-A-114 is open …

$ ./labs8 counter
llm_calls=3 cases=1          ← стільки ж, скільки без падіння
```

| Момент | `llm_calls` | `cases` |
|---|---:|---:|
| контрольний хід без падіння | **3** | 1 |
| перед `kill -9` | 2 | 0 |
| після рестарту воркера | **3** | 1 |

Після відновлення додався **рівно один** виклик моделі — фінальна відповідь.
Перші два Temporal відтворив із історії подій, а не питав модель заново. Якби
це був перезапуск, а не відновлення, лічильник показав би 5.

Кейс створено один (`cases=1`) при двох прогонах того самого запиту:
ключ ідемпотентності `transaction_id + merchant_id` у `ledger.Desk` зробив
другий виклик `Replayed`, а не другим возвратом.

## Чого тут немає і чому

**Персистентна сесія (10 балів).** `AgentWorkflow` будує runner із
`session.InMemoryService()`, тож другий `start -session crash` починає розмову
спочатку — перевірено, агент не згадує попередній хід. Щоб це полагодити,
сесія має жити в SQLite, а читання й запис сесії — йти через Activity (бо це
I/O, а workflow мусить лишатися детермінованим). `internal/agentdb` для цього
готовий, проводки немає.

**Персистентна пам'ять (10 балів).** Activity `RememberSession` написана й
викликається наприкінці кожного ходу, але читає сесію з того самого
`agentdb`, куди workflow нічого не пише (див. вище) — тож у пам'ять нічого не
потрапляє. Dgraph (`docker compose up -d dgraph`) не піднімався, інструментів
`search_nodes`/`open_nodes` немає.

Це чесна межа: ланцюжок «сесія в SQLite → пам'ять у Dgraph → recall після
рестарту» тримається на персистентній сесії, якої немає. Писати в README про
recall, не показавши його, означало б вигадати доказ.
