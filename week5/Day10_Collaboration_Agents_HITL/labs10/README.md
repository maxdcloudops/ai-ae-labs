# ДЗ 10 — команда агентів і людина в циклі (розв'язок)

Повний опис, прогони й пояснення — у
**[`submission/week5/day10/README.md`](../../../submission/week5/day10/README.md)**.

```bash
go test -race ./...     # 44 тести, без мережі й ключа

# ранній win: один спеціаліст
go run . -offline

# повний граф до паузи (процес 1)
go run . start -offline -trace -session s1 "Міграція з legacy-процесора"

# рішення (процес 2 — окремий запуск програми)
go run . pending
go run . decide -offline -session s1 -id approval-… -by oksana approve
go run . audit

# стримування збою: джерело не відповідає
go run . start -offline -session s2 -fail integration_desk "Міграція"

# мовчання = відмова
go run . start -offline -session s3 -window 1s "Міграція" && go run . expire -offline -session s3

# policy-точка: роль визначає, які джерела доступні
go run . start -offline -role analyst "Міграція"
```

| Файл | Що в ньому |
|---|---|
| `graph.go` | граф (fan-out → JoinNode → merge → затвор → відправка) |
| `desks.go` | три спеціалісти, їхні джерела й керований збій |
| `report.go` | артефакт, хеш, прогалини, блок доказів для людини |
| `policy.go` | таблиця прав і policy-точка (`BeforeToolCallback`) |
| `main.go` | CLI: start / pending / decide / expire / audit |
| `internal/approvals/` | **стартер**, не моє: журнал схвалень |
