package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCostArithmeticMatchesGatewayCatalog фіксує головний інваріант ДЗ:
// арифметика харнесу мусить збігатися з арифметикою гейтвея до останнього знаку.
//
// Очікувані значення взяті НЕ з цього коду, а з живого access-log локального
// agentgateway на тих самих ставках (див. README, §Два виміри однієї вартості).
// Тому тест ловить саму помилку, через яку колонка «$» стає марною: поділ на
// 1000 замість 1 000 000 дає таблицю, яка виглядає правдоподібно й бреше в
// тисячу разів.
func TestCostArithmeticMatchesGatewayCatalog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		in, out  int
		expected float64
	}{
		{"baseline", gatewayConfigs[0], 9, 12, 0.00000855},
		{"cheap", gatewayConfigs[1], 9, 12, 0.00000285},
		{"premium", gatewayConfigs[2], 9, 12, 0.000207},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.CostUSD(tc.in, tc.out)
			if diff := got - tc.expected; diff > 1e-12 || diff < -1e-12 {
				t.Fatalf("CostUSD(%d,%d) = %.12f, гейтвей порахував %.12f", tc.in, tc.out, got, tc.expected)
			}
		})
	}
}

// TestCheaperPriceSheetCanCostMore — те, чого не видно з прайс-листа.
//
// Модель із нижчою ставкою за ВХІД виходить дорожчою за задачу, бо задача
// генеративна: вихідних токенів багато, і саме їхня ставка визначає рахунок.
// Це причина, чому «виберемо дешевшу» без заміру — це не рішення, а здогад.
func TestCheaperPriceSheetCanCostMore(t *testing.T) {
	cheapInput := Config{Label: "cheap-in", Model: "a", AsOf: "27.09.2026",
		InputUSDPerMTok: 0.05, OutputUSDPerMTok: 20.00}
	pricierInput := Config{Label: "pricier-in", Model: "b", AsOf: "27.09.2026",
		InputUSDPerMTok: 1.00, OutputUSDPerMTok: 2.00}

	const in, out = 500, 800
	if cheapInput.CostUSD(in, out) <= pricierInput.CostUSD(in, out) {
		t.Fatalf("очікували, що дешевший вхід дасть дорожчу задачу: %.8f vs %.8f",
			cheapInput.CostUSD(in, out), pricierInput.CostUSD(in, out))
	}
}

func TestConfigValidateRejectsPriceWithoutDate(t *testing.T) {
	c := Config{Label: "x", Model: "m", InputUSDPerMTok: 1}
	if err := c.Validate(); err == nil {
		t.Fatal("конфігурація без as_of мусить падати: ціна без дати — це не ціна")
	}
}

// TestIsTransient — рішення «повторювати чи ні».
func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", errors.New("POST /v1/chat: 429 Too Many Requests"), true},
		{"503", errors.New("503 Service Unavailable"), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:4000: connection refused"), true},
		{"400 не повторюємо", errors.New("400 Bad Request: model not found"), false},
		{"401 не повторюємо", errors.New("401 Unauthorized: invalid api key"), false},
		{"скасування згори не повторюємо", context.Canceled, false},
		{"дедлайн не повторюємо", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Fatalf("IsTransient(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryDoesNotRepeatPermanentErrors — ретрай на 401 це вчетверо довше
// повідомлення про ту саму проблему.
func TestRetryDoesNotRepeatPermanentErrors(t *testing.T) {
	calls := 0
	policy := RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, Sleep: noSleep}
	attempts, err := Retry(context.Background(), policy, func() error {
		calls++
		return errors.New("401 Unauthorized")
	})
	if err == nil {
		t.Fatal("очікували помилку")
	}
	if calls != 1 || attempts != 1 {
		t.Fatalf("постійну помилку повторили: calls=%d attempts=%d, очікували 1/1", calls, attempts)
	}
}

// TestRetrySucceedsAfterTransientFailures — той самий сценарій, що дає mock-fail
// на гейтвеї: дві 500-ки, потім успіх.
func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	var delays []time.Duration
	policy := RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			return nil
		},
	}

	attempts, err := Retry(context.Background(), policy, func() error {
		calls++
		if calls < 3 {
			return errors.New("500 internal server error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("очікували успіх на третій спробі: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if len(delays) != 2 {
		t.Fatalf("очікували 2 паузи між 3 спробами, отримали %d", len(delays))
	}
	// Backoff мусить РОСТИ: дві однакові паузи означають, що експонента не
	// працює, і апстрім отримає той самий шторм запитів.
	if delays[1] <= delays[0] {
		t.Fatalf("затримка не зростає: %v → %v", delays[0], delays[1])
	}
}

// TestRetryStopsAtLimit — ліміт спроб має бути стелею, а не побажанням.
func TestRetryStopsAtLimit(t *testing.T) {
	calls := 0
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, Sleep: noSleep}
	attempts, err := Retry(context.Background(), policy, func() error {
		calls++
		return errors.New("429 rate limit")
	})
	if err == nil {
		t.Fatal("очікували помилку після вичерпання спроб")
	}
	if calls != 3 || attempts != 3 {
		t.Fatalf("calls=%d attempts=%d, очікували 3/3", calls, attempts)
	}
}

// TestDelayGrowsAndIsCapped — jitter не має ламати ні зростання, ні стелю.
func TestDelayGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: 100 * time.Millisecond, MaxDelay: 800 * time.Millisecond}
	for attempt := 1; attempt <= 10; attempt++ {
		d := p.Delay(attempt)
		if d <= 0 {
			t.Fatalf("Delay(%d) = %v, мусить бути > 0", attempt, d)
		}
		if d > p.MaxDelay {
			t.Fatalf("Delay(%d) = %v перевищує стелю %v", attempt, d, p.MaxDelay)
		}
	}
	// Full jitter дає діапазон [d/2, d), тож перша спроба ніколи не має
	// перевищити базу, а сьома — завжди впертися в стелю.
	if got := p.Delay(1); got >= p.BaseDelay {
		t.Fatalf("Delay(1) = %v, очікували < BaseDelay=%v (jitter half-range)", got, p.BaseDelay)
	}
	if got := p.Delay(7); got < p.MaxDelay/2 {
		t.Fatalf("Delay(7) = %v, очікували впертися в стелю %v", got, p.MaxDelay)
	}
}

// TestRunAllRespectsParallelLimit — стеля одночасних запитів.
//
// Без неї «однакові промпти на всі моделі одночасно» — це найшвидший спосіб
// самому собі зробити 429, і бенчмарк починає міряти власний rate limit.
func TestRunAllRespectsParallelLimit(t *testing.T) {
	const limit = 2
	var live, peak atomic.Int64

	run := func(context.Context, Config, Prompt) (Observation, error) {
		now := live.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		live.Add(-1)
		return Observation{Calls: []Call{{Latency: time.Millisecond, InTokens: 3, OutTokens: 4}}}, nil
	}

	cfgs := gatewayConfigs
	samples, err := RunAll(context.Background(), cfgs, benchPrompts, 2, limit, noRetry(), run)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if want := len(cfgs) * len(benchPrompts) * 2; len(samples) != want {
		t.Fatalf("вимірів %d, очікували %d", len(samples), want)
	}
	if peak.Load() > limit {
		t.Fatalf("стеля паралелізму порушена: пік %d > %d", peak.Load(), limit)
	}
}

// TestRunAllKeepsGoingAfterOneFailure — таблиця на дві моделі з третім рядком
// «помилка» інформативніша за відсутність таблиці.
func TestRunAllKeepsGoingAfterOneFailure(t *testing.T) {
	run := func(_ context.Context, cfg Config, _ Prompt) (Observation, error) {
		if cfg.Label == gatewayConfigs[1].Label {
			return Observation{}, errors.New("401 Unauthorized")
		}
		return Observation{Calls: []Call{{Latency: time.Millisecond, InTokens: 9, OutTokens: 12}}}, nil
	}

	samples, err := RunAll(context.Background(), gatewayConfigs, benchPrompts[:1], 1, 3, noRetry(), run)
	if err != nil {
		t.Fatalf("одна помилка не мусить валити весь бенчмарк: %v", err)
	}
	var failed, ok int
	for _, s := range samples {
		if s.Err != "" {
			failed++
		} else {
			ok++
		}
	}
	// Порахувати очікуване з таблиці конфігурацій, а не вписати 1/2 руками:
	// інакше кожен новий рядок у gatewayConfigs ламає тест, який перевіряє
	// зовсім інше.
	wantOK := len(gatewayConfigs) - 1
	if failed != 1 || ok != wantOK {
		t.Fatalf("failed=%d ok=%d, очікували 1/%d", failed, ok, wantOK)
	}
}

func TestRunAllRejectsZeroRuns(t *testing.T) {
	run := func(context.Context, Config, Prompt) (Observation, error) { return Observation{}, nil }
	if _, err := RunAll(context.Background(), gatewayConfigs, benchPrompts, 0, 1, noRetry(), run); err == nil {
		t.Fatal("n=0 мусить падати, а не повертати порожню таблицю")
	}
}

// TestSummarizeIsMeasurementNotRanking — харнес віддає ВИМІР, не рейтинг.
//
// Сортування за вартістю чи латентністю ховає компроміс: найдешевша
// конфігурація може не проходити за data residency, і таблиця, відсортована за
// ціною, підсовує рішення замість того, щоб показати дані.
func TestSummarizeIsMeasurementNotRanking(t *testing.T) {
	samples := []Sample{
		{Config: gatewayConfigs[0].Label, Latency: 30 * time.Millisecond, CostUSD: 0.9},
		{Config: gatewayConfigs[1].Label, Latency: 10 * time.Millisecond, CostUSD: 0.1},
		{Config: gatewayConfigs[2].Label, Latency: 20 * time.Millisecond, CostUSD: 0.5},
	}
	sums := Summarize(gatewayConfigs, samples)
	for i, cfg := range gatewayConfigs {
		if sums[i].Config != cfg.Label {
			t.Fatalf("рядок %d = %q, очікували %q: порядок таблиці — це порядок конфігурацій, а не метрики",
				i, sums[i].Config, cfg.Label)
		}
	}
}

// TestSummarizeExcludesRefusalsFromLatency — відмова межі не є швидкою моделью.
func TestSummarizeExcludesRefusalsFromLatency(t *testing.T) {
	cfg := gatewayConfigs[0]
	sums := Summarize([]Config{cfg}, []Sample{
		{Config: cfg.Label, Prompt: "out-of-domain", Refused: true, Latency: 0},
		{Config: cfg.Label, Prompt: "plan", Latency: 40 * time.Millisecond},
		{Config: cfg.Label, Prompt: "plan", Latency: 60 * time.Millisecond},
	})
	if len(sums) != 1 {
		t.Fatalf("очікували один агрегат, отримали %d", len(sums))
	}
	if sums[0].Refusals != 1 {
		t.Fatalf("Refusals = %d, want 1", sums[0].Refusals)
	}
	if sums[0].MinLatency == 0 {
		t.Fatal("нульова латентність відмови потрапила в розкид: межа виглядає як найшвидша модель")
	}
	if sums[0].MedianLatency < 40*time.Millisecond {
		t.Fatalf("медіана %v затягнута нулем відмови", sums[0].MedianLatency)
	}
}

// TestSummarizeOnlyAveragesScoredSamples — сценарій, від якого формат не
// вимагається, не має тягнути середнє вниз.
//
// Це був справжній баг першого прогону: context-stress (правильна відповідь —
// «цих даних немає», а не план) ішов у середнє нулем, і модель, яка відповіла
// ЧЕСНО, отримувала за це 2.67 замість 4.00.
func TestSummarizeOnlyAveragesScoredSamples(t *testing.T) {
	cfg := gatewayConfigs[0]
	sums := Summarize([]Config{cfg}, []Sample{
		{Config: cfg.Label, Prompt: "saturday-plan", Scored: true, Format: 4, Latency: time.Millisecond},
		{Config: cfg.Label, Prompt: "rain-fallback", Scored: true, Format: 4, Latency: time.Millisecond},
		{Config: cfg.Label, Prompt: "context-stress", Scored: false, Format: 0, Latency: time.Millisecond},
		{Config: cfg.Label, Prompt: "out-of-domain", Refused: true},
	})
	if got := sums[0].FormatAvg; got != 4 {
		t.Fatalf("FormatAvg = %.2f, want 4.00: неоцінювані сценарії потрапили в середнє", got)
	}
}

func TestFormatScore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   int
	}{
		{"повний контракт", offlineAnswer, 4},
		{"без бюджету", "Ранок: кава\nДень: музей\nВечір: опера", 3},
		{"вільний текст", "Hello from mock-llm! You asked for model \"mock-gpt\".", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatScore(tc.answer); got != tc.want {
				t.Fatalf("FormatScore = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- gateway.go ---

// accessLogSample — справжні рядки з локального agentgateway v1.5.0,
// скопійовані з прогону 27.09.2026 (llm.prompt урізано).
const accessLogSample = `2026-09-27T12:41:01.454219Z	info	request gateway=default/default http.status=200 trace.id=dc13 protocol=llm gen_ai.provider.name=mock gen_ai.request.model=mock-gpt gen_ai.response.model=mock-gpt gen_ai.usage.input_tokens=9 gen_ai.usage.output_tokens=12 agw.ai.usage.cost.total=0.00000855 duration=6ms llm.prompt=[{"role": "user", "content": "x=1 y=2"}]
2026-09-27T12:41:01.473298Z	info	request gateway=default/default http.status=200 trace.id=1973 protocol=llm gen_ai.provider.name=mock gen_ai.request.model=mock-gpt-mini gen_ai.response.model=mock-gpt-mini gen_ai.usage.input_tokens=9 gen_ai.usage.output_tokens=12 agw.ai.usage.cost.total=0.00000285 duration=1ms
2026-09-27T12:41:02.000000Z	info	request gateway=default/default http.status=500 trace.id=aaaa protocol=llm gen_ai.provider.name=mock gen_ai.request.model=mock-fail gen_ai.usage.input_tokens=4 gen_ai.usage.output_tokens=0 duration=2ms
2026-09-27T12:41:03.000000Z	info	listener established address=127.0.0.1:15000 component="admin"`

func TestParseAccessLog(t *testing.T) {
	stats, err := ParseAccessLog(strings.NewReader(accessLogSample))
	if err != nil {
		t.Fatalf("ParseAccessLog: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("моделей у логу %d, очікували 3: %v", len(stats), keys(stats))
	}

	base := stats["mock-gpt"]
	if base == nil || base.Requests != 1 || base.InTokens != 9 || base.OutTokens != 12 {
		t.Fatalf("mock-gpt розібрано неправильно: %+v", base)
	}
	if diff := base.CostUSD - 0.00000855; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost mock-gpt = %.12f, очікували 0.00000855", base.CostUSD)
	}
	if base.Statuses[200] != 1 {
		t.Fatalf("статуси mock-gpt = %v, очікували {200:1}", base.Statuses)
	}

	// Рядок без ціни в каталозі все одно мусить бути в звіті: запит відбувся.
	fail := stats["mock-fail"]
	if fail == nil || fail.Requests != 1 {
		t.Fatalf("mock-fail загубився: %+v", fail)
	}
	if fail.Statuses[500] != 1 {
		t.Fatalf("500-ка mock-fail не потрапила в звіт: %v", fail.Statuses)
	}
	if fail.CostUSD != 0 {
		t.Fatalf("для mock-fail гейтвей ціни не знав, а ми дописали %.12f", fail.CostUSD)
	}

	// Рядок, що не є запитом до моделі, не мусить створювати «модель».
	if _, ok := stats["established"]; ok {
		t.Fatal("службовий рядок логу розібрано як запит до моделі")
	}
}

// TestJoinGatewayCostStripsRoutePrefix — у конфігурації модель несе маршрут
// (`mock/mock-gpt`), а гейтвей у лог пише ім'я ПІСЛЯ трансформації.
func TestJoinGatewayCostStripsRoutePrefix(t *testing.T) {
	sums := Summarize(gatewayConfigs, nil)
	stats := map[string]*GatewayStat{
		"mock-gpt":      {Model: "mock-gpt", CostUSD: 0.00000855},
		"mock-gpt-mini": {Model: "mock-gpt-mini", CostUSD: 0.00000285},
	}
	sums = JoinGatewayCost(sums, stats)

	if sums[0].GatewayCostUSD == 0 {
		t.Fatal("вартість гейтвея не приклеїлася: префікс маршруту не зрізано")
	}
	if sums[2].GatewayCostUSD != 0 {
		t.Fatal("для конфігурації без рядка в логу вартість мусить лишитися нульовою, а не вгаданою")
	}
}

// --- chart.go ---

// TestRenderSVGIsWellFormedAndShowsBothMeasurements — графік мусить бути
// валідним SVG (інакше GitHub покаже битий значок) і показувати ОБА виміри
// вартості: у цьому вся суть панелі.
func TestRenderSVGIsWellFormedAndShowsBothMeasurements(t *testing.T) {
	sums := []Summary{
		{Config: "A & B", Model: "mock/mock-gpt", AsOf: "27.09.2026", N: 3,
			MedianLatency: 6 * time.Millisecond, HarnessCostUSD: 0.00000855,
			GatewayCostUSD: 0.00000855, FormatAvg: 4},
		{Config: "premium", Model: "mock/mock-gpt-pro", AsOf: "27.09.2026", N: 3,
			MedianLatency: 5 * time.Millisecond, HarnessCostUSD: 0.000207, FormatAvg: 0},
	}
	svg := RenderSVG(ChartInput{Title: "T", Mode: "M", AsOf: "27.09.2026", Summaries: sums})

	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatal("рендер не є цілим SVG-документом")
	}
	if strings.Count(svg, "<svg") != 1 {
		t.Fatal("вкладений <svg>")
	}
	// Амперсанд у підписі мусить бути екранований, інакше SVG невалідний.
	if strings.Contains(svg, "A & B") {
		t.Fatal("підпис не екранований: `&` робить SVG невалідним")
	}
	if !strings.Contains(svg, "A &amp; B") {
		t.Fatal("екранований підпис не знайдено")
	}
	for _, want := range []string{"0.00000855", "0.00020700", colHarness, colGateway} {
		if !strings.Contains(svg, want) {
			t.Fatalf("у графіку немає %q", want)
		}
	}
	// Конфігурація, для якої гейтвей нічого не виміряв, мусить казати це
	// вголос, а не показувати нуль, який читається як «безкоштовно».
	if !strings.Contains(svg, "гейтвей не виміряв") {
		t.Fatal("відсутній вимір гейтвея показано як нуль")
	}
}

func TestRenderSVGSurvivesEmptyInput(t *testing.T) {
	if svg := RenderSVG(ChartInput{}); !strings.Contains(svg, "<svg") {
		t.Fatal("порожній вхід мусить давати валідний SVG із поясненням, а не паніку")
	}
}

// --- helpers ---

func noSleep(context.Context, time.Duration) error { return nil }

func noRetry() RetryPolicy {
	return RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, Sleep: noSleep}
}

func keys(m map[string]*GatewayStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
