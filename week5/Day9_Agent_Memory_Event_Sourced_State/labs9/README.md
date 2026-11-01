# ДЗ 9 — агент із керованою пам'яттю (розв'язок)

Повний опис, прогони й пояснення — у
**[`submission/week5/day9/README.md`](../../../submission/week5/day9/README.md)**.

```bash
go test -race ./...     # 27 тестів, 1 skip (жива модель — потрібен провайдер)

# три процеси, одна БД: факт переживає рестарт
go run . -offline say day1 "A-114 сидить на legacy-інтеграції, міграцію відклали до Q4."
go run . -offline facts day1
go run . -offline say day2 "Що порадити мерчанту A-114?"

# контраст: інший користувач на тій самій БД факту не бачить
go run . -offline -user marko say day2 "Що порадити мерчанту A-114?"

# інцидент: нотатка порталу НЕ може переписати факт
go run . -offline poison

# журнал відповідності з подій сесії
go run . -offline audit day1            # → audit_iryna_day1.json
```

Що дописано (решта — стартер):

| Файл | Що саме |
|---|---|
| `agent.go` | секція «Пам'ять» в `Instruction`; політика в `rememberFact`; `TaintAfterTool`; облік промпту по компонентах |
| `main.go` | прапорці `-user`, `-portal-note`; команда `audit`; таблиця токенів |
| `main_test.go` | `TestPoisonedNoteCannotRewriteFact`, `TestConcurrentWritesDifferentKeys`; фікстури `failingState` |
| `internal/memlab/audit.go` | виправлено підрахунок подій у рядку `compaction` |

Останні два рядки — правки стартера, а не TODO; причини описані в
[submission-README](../../../submission/week5/day9/README.md#що-я-змінив-у-стартері-щоб-рецензент-не-шукав).
