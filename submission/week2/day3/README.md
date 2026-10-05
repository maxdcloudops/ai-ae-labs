# ДЗ 3 — Агент стає графом: вузли, ребра, тести

**Прогін: 05.10.2026.** Go 1.27.1 · `google.golang.org/adk/v2 v2.5.0`.

> Версії зафіксовано після апгрейду курсу з v2.4.0 на v2.5.0 (коміт `1a2bffc`
> в апстрімі). Лаба й усі тести пройшли апгрейд без змін у коді.

## Як запустити

```bash
# граф без моделі й без ключа
go run ./week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3 -mode=graph

# усі тести лаби та спільного пакета — теж без ключа й без мережі
go test ./week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3 ./week2/internal/refund
```

Станом на 05.10.2026: **72 тести, усі зелені**.

## Нормалізований фрагмент event log

Один прогін `-mode=graph`, stdout без змін:

```json
{"author":"first_graph_agent","routes":["refund"],"output":"Мерчант A-114 просить повернення по транзакції txn-2026-07-118845"}
{"author":"first_graph_agent","output":{"transaction_id":"txn-2026-07-118845","merchant_id":"A-114"}}
{"author":"first_graph_agent","output":{"case_id":"rc-txn-2026-07-118845-A-114","merchant_id":"A-114","status":"pending","transaction_id":"txn-2026-07-118845"},"state_delta":{"refund:last_case_id":"rc-txn-2026-07-118845-A-114","refund:last_merchant_id":"A-114","refund:last_status":"pending"}}
{"author":"first_graph_agent","output":"Кейс rc-txn-2026-07-118845-A-114: транзакція txn-2026-07-118845, мерчант A-114, статус pending"}
```

| Рядок | Вузол | Що сталося |
|---|---|---|
| 1 | `classify` | маршрут обрано: `routes:["refund"]` |
| 2 | `prepare` | з тексту видобуто `transaction_id` і `merchant_id` — текст став структурою |
| 3 | `open_refund_case` | **виклик інструмента**, і разом із ним `state_delta` |
| 4 | `format` | структура повернулася в текст для людини |

Третій рядок — єдиний, де щось **змінилося**, а не було сказано. `state_delta`
і є те, що показують комплаєнсу.

### Чому цей фрагмент можна класти в README

Умова вимагає **нормалізований** лог, тобто відтворюваний. Перевірено прогоном
на 20 ітерацій:

```bash
go build -o /tmp/day3 ./week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3
for i in $(seq 1 20); do /tmp/day3 -mode=graph; done > /tmp/all.json
wc -l < /tmp/all.json          # 80
sort -u /tmp/all.json | wc -l  # 4
```

**80 рядків, 4 унікальні** — кожна рівно по 20 разів, байт у байт. Лог
детермінований: його можна диффити, зберігати як еталон і прикладати до тікета.

Нестабільну частину винесено на **інший потік**, і це зроблено навмисно:

| Потік | Вміст | Стабільний? |
|---|---|---|
| `stdout` | JSON event log — для машини | **так**, тому його й порівнюємо |
| `stderr` | `classify: rule "…" → refund` з таймстемпом — для людини | ні, і не мусить |

```bash
/tmp/day3 -mode=graph 2>/dev/null   # лише stdout: 4 рядки JSON
/tmp/day3 -mode=graph 1>/dev/null   # лише stderr: рядок classify
```

Якби таймстемп лишили в stdout, golden-тест «мигав» би між прогонами — рівно та
пастка, про яку попереджає troubleshooting умови.

## Що дає граф проти imperative-скрипта

Звичайний агент із ДЗ 1 сам вирішує, викликати інструмент чи просто відповісти
текстом. Через це він може написати «кейс відкрито», не відкривши нічого — і для
того, хто читає відповідь, ці два випадки виглядають однаково. У графі порядок
кроків задано в коді, тож виклик `open_refund_case` обійти не можна: він не
рішення моделі, а обов'язковий вузол. Кожен крок лишає запис у логу, а крок,
який щось змінив, лишає ще й `state_delta`. Саме `state_delta` відрізняє зроблене
від сказаного: за логом видно, що номер кейса з'явився на третьому кроці, бо на
другому його ще не було. Двадцять прогонів дали байт у байт однаковий лог —
такий лог можна порівнювати, зберігати як еталон і прикладати до тікета, а з
відповіддю моделі так не вийде: вона щоразу інша.

## Структура графа

```
Start → classify ─┬─ "refund"        → prepare        → open_refund_case  → format
                  ├─ "status"        → prepare_status → check_refund_status → format_status
                  └─ "out_of_domain" → refuse
```

Топологія зібрана в [`agent_graph.go`](../../../week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3/agent_graph.go)
через `workflow.Chain` і `workflow.Concat`; інструмент підключено як
`workflow.NewToolNodeTyped[refund.Input, refund.Output]`.

Відмова (`refuse`) — **оголошений вузол**, а не відсутність відповіді: запит поза
доменом лишає подію в логу так само, як і решта.

## Тести

| Файл | Що перевіряє |
|---|---|
| [`week2/internal/refund/refund_test.go`](../../../week2/internal/refund/refund_test.go) | табличні тести вузлів на `agent.NewStrictContextMock`: `Prepare`, `Format`, `Classify`, `StateDelta` при успіху й при відмові |
| [`labs3/agent_graph_test.go`](../../../week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3/agent_graph_test.go) | граф цілком: 5 сценаріїв маршрутизації, відхилений запит не створює кейс |
| [`labs3/agent_test.go`](../../../week2/Day3_First_ADK2_Agent_Workflow_Graph/labs3/agent_test.go) | шлях із моделлю: `TestFluentAnswerWithoutSideEffect` фіксує, що красива відповідь **без** сліду можлива |

Два найцікавіші:

- **`TestToolStateAndIdempotency`** перевіряє `StateDelta` у двох напрямках:
  валідний вхід без запису → `missing audit`; відхилений вхід із записом →
  `rejected input mutated state`. Правило не «порожньо добре», а «запис мусить
  збігатися з тим, що справді сталося».
- **`TestFluentAnswerWithoutSideEffect`** — це тест-обвинувачення, а не опис
  бажаної поведінки: він доводить, що модель уміє сказати «Кейс відкрито»,
  не викликавши інструмент і не змінивши нічого.
