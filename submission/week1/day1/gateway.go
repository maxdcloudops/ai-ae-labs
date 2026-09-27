// Друге джерело правди про гроші: access-log agentgateway.
//
// ++ Advanced, пункт «Два виміри однієї вартості». Harness рахує вартість із
// того, що бачить АГЕНТ: usage-метадані у відповіді × ставка з конфігурації.
// Гейтвей рахує її з того, що справді пішло в мережу, і пише результат у
// поле agw.ai.usage.cost.total кожного рядка access-log.
//
// Це два різні виміри однієї величини, і саме тому їх варто зводити:
// вартість вимірюють на МЕЖІ системи, а не всередині агента. Якщо ці числа
// розходяться — праві зазвичай не ви: агент не бачить ні ретраїв транспорту,
// ні кешованих префіксів, ні того, що маршрут підмінив модель.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GatewayStat — агрегат гейтвея по одній моделі.
type GatewayStat struct {
	Model     string  `json:"model"`
	Requests  int     `json:"requests"`
	InTokens  int     `json:"in_tokens"`
	OutTokens int     `json:"out_tokens"`
	CostUSD   float64 `json:"cost_usd"`
	// Statuses — розподіл HTTP-статусів. Потрібен, щоб 500-ки від бекенда
	// (сценарій fault injection) було видно в звіті, а не тільки в ретраях.
	Statuses map[int]int `json:"statuses"`
}

// ParseAccessLog розбирає access-log гейтвея й агрегує по імені моделі.
//
// Формат рядка — key=value через табуляцію та пробіли, тож парсер навмисно
// простий: шукаємо відомі ключі, усе інше ігноруємо. Це стійкіше за regexp на
// увесь рядок — llm.prompt усередині містить і пробіли, і знаки рівності.
//
// Рядок без agw.ai.usage.cost.total НЕ пропускається: запит, для якого гейтвей
// не знайшов ціни в каталозі, все одно відбувся, і його токени мають бути в
// звіті. Інакше «безкоштовна» модель виглядає як модель, якої не викликали.
func ParseAccessLog(r io.Reader) (map[string]*GatewayStat, error) {
	out := map[string]*GatewayStat{}
	sc := bufio.NewScanner(r)
	// Рядки з llm.prompt/llm.completion бувають довгими — дефолтний 64 KiB
	// буфер на них падає з bufio.Scanner: token too long, і звіт тихо
	// виходить коротшим за реальність.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "gen_ai.request.model=") {
			continue
		}
		model := field(line, "gen_ai.response.model=")
		if model == "" {
			model = field(line, "gen_ai.request.model=")
		}
		if model == "" {
			continue
		}
		stat, ok := out[model]
		if !ok {
			stat = &GatewayStat{Model: model, Statuses: map[int]int{}}
			out[model] = stat
		}
		stat.Requests++
		stat.InTokens += atoi(field(line, "gen_ai.usage.input_tokens="))
		stat.OutTokens += atoi(field(line, "gen_ai.usage.output_tokens="))
		stat.CostUSD += atof(field(line, "agw.ai.usage.cost.total="))
		if code := atoi(field(line, "http.status=")); code > 0 {
			stat.Statuses[code]++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("читання access-log: %w", err)
	}
	return out, nil
}

// field дістає значення ключа key= до першого пробілу або табуляції.
func field(line, key string) string {
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func atof(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// FetchGatewayStats читає access-log живого гейтвея через docker compose.
//
// since обмежує вікно: без нього в агрегат потрапили б усі запити з моменту
// старту контейнера, включно з curl-перевірками, і «вартість за бенчмарк»
// виявилася б більшою за бенчмарк. Вікно беремо з моменту старту прогону.
//
// Помилка тут НЕ фатальна для бенчмарку: гейтвей — необов'язкова частина ДЗ,
// і таблиця без колонки «$ гейтвей» усе одно таблиця. Тому виклик у main
// логує помилку й іде далі.
func FetchGatewayStats(ctx context.Context, composeDir string, since time.Time) (map[string]*GatewayStat, error) {
	// --since приймає RFC3339; віднімаємо секунду, щоб не втратити перший
	// запит через округлення таймстемпів у docker.
	arg := since.Add(-time.Second).UTC().Format(time.RFC3339)
	cmd := exec.CommandContext(ctx, "docker", "compose", "logs",
		"--no-log-prefix", "--since", arg, "agentgateway")
	cmd.Dir = composeDir

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose logs у %s: %w", composeDir, err)
	}
	return ParseAccessLog(strings.NewReader(string(out)))
}

// JoinGatewayCost доповнює агрегати харнесу вартістю з гейтвея.
//
// Зведення йде за іменем моделі, а не за trace.id, і це свідоме спрощення:
// агент не знає свого trace.id, тож поєднати рядок таблиці з конкретною трасою
// без зміни транспорту не вийде. Порівняння сум за вікном прогону відповідає
// на те саме питання — чи зійшлася арифметика, — і не вдає точності, якої тут
// немає.
//
// Ім'я моделі в конфігурації несе маршрут (`mock/mock-gpt`), а гейтвей у лог
// пише ім'я ПІСЛЯ трансформації (`mock-gpt`), тож префікс маршруту зрізаємо.
func JoinGatewayCost(sums []Summary, stats map[string]*GatewayStat) []Summary {
	for i := range sums {
		name := sums[i].Model
		if _, after, ok := strings.Cut(name, "/"); ok {
			name = after
		}
		if stat, ok := stats[name]; ok {
			sums[i].GatewayCostUSD = stat.CostUSD
		}
	}
	return sums
}
