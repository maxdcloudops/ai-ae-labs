# ДЗ 11 — Multi-Critic Manifest Agent (розв'язок, альтернативна тема)

Повний опис — у **[`submission/week6/day11/README.md`](../../../submission/week6/day11/README.md)**.

Альтернативна тема умови (Security): агент пише **Kubernetes-маніфести**,
критики — привілеїв / ресурсних лімітів / секретів, обʼєктивна метрика —
власний валідатор маніфестів (бо `kubectl --dry-run=client` не валідує; див. README).

```bash
go test -race ./...     # 32 тести (93 з підтестами), без мережі й ключа

# один прогін; insecure — єдиний режим, де видно фідбек від критиків
go run . -offline -maker insecure -task pay_api_deployment -trace

# eval по трьох завданнях; головна цифра — хибні прийняття
go run . -offline -eval -maker buggy       # 3 із 3
go run . -offline -eval -maker insecure    # 1 із 3
go run . -offline -eval -maker reference   # 0 із 3
go run . -offline -eval -maker stub        # бюджет вичерпано

# Web UI лаунчера (не перевіряв)
go run . -task ledger_cronjob web webui api
```

| Файл | Що в ньому |
|---|---|
| `loop.go` | динамічний цикл maker → валідатор → панель → фідбек; бюджет і сентинели |
| `critics.go` | три LLM-критики, `ParseVerdict`/`ValidateVerdict`, кворум, офлайн-правила |
| `internal/manifest/` | обʼєктивний критик (схема + видимі правила) і три завдання з holdout |
| `eval.go` | звіт: прийнято / спроб / час / токени / holdout / **false accepts** |
| `main.go` | CLI, облік токенів, `-trace` |
| `internal/{gate,billing,usage}/` | **стартер**, не моє (gate і billing — Go-трек, не використовуються) |
| `solution/` | **еталон курсу**, не моє і не читав |
