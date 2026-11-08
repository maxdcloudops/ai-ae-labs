# ДЗ 12 — Personal Research Agent (капстон, розв'язок)

Повний опис — у **[`submission/week6/day12/README.md`](../../../submission/week6/day12/README.md)**.

```bash
go test -race ./...        # 41 тест, без мережі й ключа

go run . -eval             # метрика поточної стратегії (dev + heldout)
go run . -improve 5        # пʼять експериментів самополіпшення
go run . -improve 5 -report-only   # L1: що БУЛО Б, нічого не змінюючи
go run . -generalize       # питання, яких пропонент не бачив
go run . -q "SEV1 paging time and update cadence"   # один запит через конвеєр
go run . console           # інтерактивно, з паузою на людину
```

| Файл | Що в ньому |
|---|---|
| `pipeline.go` | зшивка: plan → fan-out → JoinNode → draft → критики → HITL → звіт |
| `research.go` | метрика (чиста `ledgerkb.Score`), джерела, пропоненти |
| `internal/sandbox/` | гальмо (а): `IsMutable` рівно для `strategy.md` і `experiments/` |
| `internal/improve/` | гальма (б), (в), (д): бюджет, keep/discard з ε, журнал |
| `strategy.md` | **мутабельний артефакт**: системна інструкція воркера |
| `experiments.log` | JSON-line на кожен експеримент, і keep, і discard |
| `experiments/` | відкинуті кандидати |
| `internal/{ledgerkb,offline}/` | **стартер**, не моє: база знань, eval-набір, правилова заглушка |
| `solution/` | **еталон курсу**; умова дозволяє адаптувати ядро — див. README, «що взяв» |
