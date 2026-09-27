// Cross-Model Benchmark Harness — ADK-free ядро.
//
// Тут немає ні агента, ні моделі, ні мережі: прогін одного сценарію приходить
// сюди як функція RunFunc. Саме тому всю логіку, яку легко зробити непомітно
// неправильною — арифметику вартості, ретраї з backoff, агрегацію,
// конкурентність — можна покрити тестами без ключа й без інтернету.
//
// Головний інваріант файлу: harness НЕ сортує таблицю за метрикою. Сортування
// за вартістю чи латентністю — це вже рейтинг, а рейтинг ховає компроміс.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// Backend — сімейство бекенда, яким їде конфігурація.
type Backend string

const (
	// BackendGateway — OpenAI-сумісний маршрут локального agentgateway.
	BackendGateway Backend = "gateway"
	// BackendGemini — прямий вендорський API Google.
	BackendGemini Backend = "gemini"
	// BackendOpenAI — прямий вендорський API OpenAI.
	BackendOpenAI Backend = "openai"
)

// Config — одна конфігурація моделі в бенчмарку.
//
// AsOf не опційний і не косметичний: alias, ціна й доступність моделей
// змінюються швидше за дедлайн домашнього завдання, тож рядок таблиці без дати
// свого прогону — це число без одиниць вимірювання.
type Config struct {
	Label            string  `json:"label"`
	Model            string  `json:"model"`
	Backend          Backend `json:"backend"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
	AsOf             string  `json:"as_of"`
}

// Validate ловить конфігурацію, яка дала б правдоподібну, але безглузду таблицю.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Label) == "":
		return errors.New("конфігурація без label")
	case strings.TrimSpace(c.Model) == "":
		return fmt.Errorf("конфігурація %q без імені моделі", c.Label)
	case strings.TrimSpace(c.AsOf) == "":
		return fmt.Errorf("конфігурація %q без as_of: ціна без дати — це не ціна", c.Label)
	case c.InputUSDPerMTok < 0 || c.OutputUSDPerMTok < 0:
		return fmt.Errorf("конфігурація %q: відʼємна ставка", c.Label)
	}
	return nil
}

// CostUSD рахує вартість прогону за оголошеним прайсом.
//
// Вхід і вихід рахуються за РІЗНИМИ ставками — це не деталь, а основна причина,
// чому «дешевша модель» у прайсі може дати дорожчий рахунок за задачу:
// у вихідних токенах різниця ставок зазвичай на порядок більша.
func (c Config) CostUSD(inTokens, outTokens int) float64 {
	const perMillion = 1_000_000.0
	return float64(inTokens)*c.InputUSDPerMTok/perMillion +
		float64(outTokens)*c.OutputUSDPerMTok/perMillion
}

// Prompt — один сценарій бенчмарку.
//
// ExpectRefusal позначає сценарій, у якому ПРАВИЛЬНА поведінка — відмова.
// Без цього поля харнес порахував би відмову невдачею й видав таблицю, у якій
// коректно працююча межа виглядає як зламана модель.
type Prompt struct {
	Name          string `json:"name"`
	Text          string `json:"text"`
	ExpectRefusal bool   `json:"expect_refusal"`
	ExpectFormat  bool   `json:"expect_format"`
}

// Observation — те, що харнес дізнався про один прогін.
type Observation struct {
	Calls   []Call
	Answer  string
	Refused bool
}

// RunFunc виконує один прогін «конфігурація × промпт».
//
// Це шов, який тримає ядро незалежним від ADK: у бою сюди приходить справжній
// агент (runner.go), у тестах — функція на три рядки.
type RunFunc func(ctx context.Context, cfg Config, p Prompt) (Observation, error)

// Sample — один вимір.
type Sample struct {
	Config    string        `json:"config"`
	Prompt    string        `json:"prompt"`
	Attempts  int           `json:"attempts"`
	Latency   time.Duration `json:"latency_ns"`
	InTokens  int           `json:"in_tokens"`
	OutTokens int           `json:"out_tokens"`
	CostUSD   float64       `json:"cost_usd"`
	Refused   bool          `json:"refused"`
	// Scored каже, чи цей вимір узагалі підлягає оцінці формату. Без цього
	// прапорця сценарій, від якого формат НЕ вимагається (context-stress —
	// правильна відповідь там не план, а «цих даних немає»), потрапляв у
	// середнє нулем і занижував бал моделі за те, що вона зробила правильно.
	Scored bool   `json:"format_scored"`
	Format int    `json:"format_score"`
	Err    string `json:"error,omitempty"`
}

// RetryPolicy — ретрай з експоненційною затримкою.
//
// Jitter обов'язковий, а не «на всяк випадок»: без нього всі горутини, які
// зловили 429 на одній секунді, повторять запит теж на одній секунді й
// отримають 429 знову. Експонента без jitter лише зсуває чергу, не розріджує її.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Sleep підмінюється в тестах: тест на backoff, який справді спить
	// 1+2+4 секунди, дуже швидко перестають запускати.
	Sleep func(context.Context, time.Duration) error
}

// DefaultRetryPolicy — розумний дефолт для локального стека.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: 250 * time.Millisecond, MaxDelay: 4 * time.Second}
}

// Delay повертає затримку перед спробою attempt (1-based) з jitter.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := p.BaseDelay
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	max := p.MaxDelay
	if max <= 0 {
		max = 5 * time.Second
	}
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(max) {
		d = float64(max)
	}
	// Full jitter: [d/2, d). Дає розкид, зберігаючи експоненційне зростання.
	return time.Duration(d/2 + rand.Float64()*d/2)
}

// IsTransient відрізняє «сервер попросив зачекати» від «запит неправильний».
//
// Це головне рішення ретраїв, і воно навмисно консервативне: повторювати 400
// чи 401 означає чотири рази отримати ту саму помилку й витратити вчетверо
// більше часу на те, щоб її показати. Повторюємо лише те, що з'явилося не
// через наш запит: 429, 5xx, обриви з'єднання й таймаути.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Скасований контекст — це рішення згори, а не збій апстріму.
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"429", "too many requests", "rate limit",
		"500", "502", "503", "504", "internal server error",
		"connection refused", "connection reset", "eof",
		"timeout", "temporarily", "overloaded", "unavailable",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Retry виконує fn із експоненційним backoff, повертаючи кількість спроб.
//
// Повертає attempts навіть при помилці: у таблиці бенчмарку колонка «спроб»
// показує, скільком рядкам довелося чекати апстрім — без неї латентність
// рядка з трьома ретраями виглядає як «модель повільна».
func Retry(ctx context.Context, p RetryPolicy, fn func() error) (int, error) {
	attempts := max(p.MaxAttempts, 1)
	sleep := p.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}

	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(); err == nil {
			return attempt, nil
		}
		if !IsTransient(err) || attempt == attempts {
			return attempt, err
		}
		if sleepErr := sleep(ctx, p.Delay(attempt)); sleepErr != nil {
			return attempt, errors.Join(err, sleepErr)
		}
	}
	return attempts, err
}

// FormatScore — структурна оцінка відповіді за контрактом інструкції, 0..4.
//
// Це НЕ «якість плану», і плутати ці дві речі не можна. Тут перевіряється рівно
// те, що можна перевірити машинно й однаково для всіх моделей: чи є три блоки
// (Ранок/День/Вечір) і рядок бюджету. Модель може ідеально виконати формат і
// порадити зимою пікнік — цей бал такого не зловить, і в README про це сказано
// прямо. Зате він відповідає на питання, на яке маркетингові бенчмарки не
// відповідають: чи взагалі ця модель тримає МІЙ контракт відповіді.
func FormatScore(answer string) int {
	lower := strings.ToLower(answer)
	score := 0
	for _, marker := range []string{"ранок", "день", "вечір", "бюджет"} {
		if strings.Contains(lower, marker) {
			score++
		}
	}
	return score
}

// RunAll проганяє всі пари «конфігурація × промпт» по n разів.
//
// Конкурентність тут — не оптимізація, а умова коректності порівняння:
// послідовний прогін трьох конфігурацій розтягується в часі, і третя модель
// міряється в іншій мережевій обстановці, ніж перша. errgroup.SetLimit тримає
// стелю одночасних запитів, бо «однакові промпти на всі моделі одночасно» без
// стелі — це найшвидший спосіб самому собі зробити 429.
//
// Помилка ОДНОГО прогону не валить бенчмарк: вона їде в Sample.Err і в таблицю.
// errgroup тут використовується для очікування й стелі, а не для fail-fast —
// таблиця на дві моделі з третім рядком «помилка» інформативніша за відсутність
// таблиці.
func RunAll(
	ctx context.Context,
	cfgs []Config,
	prompts []Prompt,
	n, parallel int,
	policy RetryPolicy,
	run RunFunc,
) ([]Sample, error) {
	if len(cfgs) == 0 {
		return nil, errors.New("немає жодної конфігурації для бенчмарку")
	}
	if len(prompts) == 0 {
		return nil, errors.New("немає жодного промпту для бенчмарку")
	}
	if n <= 0 {
		return nil, errors.New("n має бути ≥ 1: один прогін — це вже не бенчмарк, нуль прогонів — це взагалі ніщо")
	}
	for _, c := range cfgs {
		if err := c.Validate(); err != nil {
			return nil, err
		}
	}
	if parallel <= 0 {
		parallel = 1
	}

	type slot struct {
		cfg    Config
		prompt Prompt
		idx    int
	}
	slots := make([]slot, 0, len(cfgs)*len(prompts)*n)
	for _, c := range cfgs {
		for _, p := range prompts {
			for range n {
				slots = append(slots, slot{cfg: c, prompt: p, idx: len(slots)})
			}
		}
	}

	samples := make([]Sample, len(slots))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	for _, s := range slots {
		g.Go(func() error {
			samples[s.idx] = runOne(gctx, s.cfg, s.prompt, policy, run)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return samples, err
	}
	return samples, nil
}

// runOne виконує один прогін із ретраями й перетворює його на Sample.
func runOne(ctx context.Context, cfg Config, p Prompt, policy RetryPolicy, run RunFunc) Sample {
	out := Sample{Config: cfg.Label, Prompt: p.Name}

	var obs Observation
	start := time.Now()
	attempts, err := Retry(ctx, policy, func() error {
		var runErr error
		obs, runErr = run(ctx, cfg, p)
		return runErr
	})
	out.Attempts = attempts
	out.Latency = time.Since(start)

	if err != nil {
		out.Err = err.Error()
		return out
	}

	// Латентність беремо з Meter, а не з wall-clock прогону: wall-clock
	// включає ретраї й сон між ними, і рядок із трьома ретраями виглядав би як
	// «модель у 8 разів повільніша».
	var modelLatency time.Duration
	for _, c := range obs.Calls {
		modelLatency += c.Latency
		out.InTokens += c.InTokens
		out.OutTokens += c.OutTokens
	}
	if len(obs.Calls) > 0 {
		out.Latency = modelLatency
	}
	out.Refused = obs.Refused
	out.CostUSD = cfg.CostUSD(out.InTokens, out.OutTokens)
	if p.ExpectFormat {
		out.Scored = true
		out.Format = FormatScore(obs.Answer)
	}
	return out
}

// Summary — агрегат по одній конфігурації.
type Summary struct {
	Config         string        `json:"config"`
	Model          string        `json:"model"`
	AsOf           string        `json:"as_of"`
	N              int           `json:"n"`
	MedianLatency  time.Duration `json:"median_latency_ns"`
	MinLatency     time.Duration `json:"min_latency_ns"`
	MaxLatency     time.Duration `json:"max_latency_ns"`
	InTokens       int           `json:"in_tokens"`
	OutTokens      int           `json:"out_tokens"`
	HarnessCostUSD float64       `json:"harness_cost_usd"`
	GatewayCostUSD float64       `json:"gateway_cost_usd"`
	Retries        int           `json:"retries"`
	Refusals       int           `json:"refusals"`
	FormatAvg      float64       `json:"format_avg"`
	Errors         int           `json:"errors"`
}

// Summarize агрегує виміри по конфігураціях.
//
// Медіана, а не середнє: середнє з трьох вимірів, один з яких застряг на
// п'яти секундах, — це художній твір. Разом із медіаною завжди друкується
// розкид min–max, бо медіана сама не показує, чого вона не показує.
//
// Порядок рядків — порядок конфігурацій, тобто рішення інженера, а НЕ
// сортування за метрикою. Це закріплено тестом.
func Summarize(cfgs []Config, samples []Sample) []Summary {
	byLabel := make(map[string]*Summary, len(cfgs))
	order := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		byLabel[c.Label] = &Summary{Config: c.Label, Model: c.Model, AsOf: c.AsOf}
		order = append(order, c.Label)
	}

	lat := make(map[string][]time.Duration, len(cfgs))
	formats := make(map[string][]int, len(cfgs))
	for _, s := range samples {
		sum, ok := byLabel[s.Config]
		if !ok {
			continue
		}
		sum.N++
		sum.InTokens += s.InTokens
		sum.OutTokens += s.OutTokens
		sum.HarnessCostUSD += s.CostUSD
		sum.Retries += s.Attempts - 1
		if s.Refused {
			sum.Refusals++
		}
		if s.Err != "" {
			sum.Errors++
			continue
		}
		// Відмову межі в латентність не беремо: виклику моделі не було, і
		// нуль у медіані зробив би межу схожою на швидку модель.
		if !s.Refused {
			lat[s.Config] = append(lat[s.Config], s.Latency)
		}
		if s.Scored {
			formats[s.Config] = append(formats[s.Config], s.Format)
		}
	}

	out := make([]Summary, 0, len(order))
	for _, label := range order {
		sum := byLabel[label]
		ds := lat[label]
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		if len(ds) > 0 {
			sum.MedianLatency = ds[len(ds)/2]
			sum.MinLatency, sum.MaxLatency = ds[0], ds[len(ds)-1]
		}
		if fs := formats[label]; len(fs) > 0 {
			total := 0
			for _, f := range fs {
				total += f
			}
			sum.FormatAvg = float64(total) / float64(len(fs))
		}
		out = append(out, *sum)
	}
	return out
}

// Markdown друкує таблицю для README.
func Markdown(sums []Summary) string {
	var b strings.Builder
	b.WriteString("| Конфігурація | Модель | n | латентність медіана (min–max) | токени in/out | $ харнес | $ гейтвей | ретраїв | формат 0–4 | станом на |\n")
	b.WriteString("|---|---|---:|---|---:|---:|---:|---:|---:|---|\n")
	for _, s := range sums {
		gateway := "—"
		if s.GatewayCostUSD > 0 {
			gateway = fmt.Sprintf("%.8f", s.GatewayCostUSD)
		}
		b.WriteString(fmt.Sprintf("| %s | `%s` | %d | %s (%s–%s) | %d/%d | %.8f | %s | %d | %.2f | %s |\n",
			s.Config, s.Model, s.N,
			ms(s.MedianLatency), ms(s.MinLatency), ms(s.MaxLatency),
			s.InTokens, s.OutTokens,
			s.HarnessCostUSD, gateway, s.Retries, s.FormatAvg, s.AsOf))
	}
	return b.String()
}

// ms форматує тривалість у мілісекундах.
func ms(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f ms", float64(d)/float64(time.Millisecond))
}
