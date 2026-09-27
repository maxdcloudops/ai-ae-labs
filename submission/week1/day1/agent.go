// ДЗ 1 — «Планувальник вихідних у Львові» + Cross-Model Benchmark Harness.
//
// Цей файл — уся ADK-проводка: інструкція агента, доменна межа і метрика.
// Логіка бенчмарку (вартість, конкурентність, ретраї) живе в bench.go і не
// знає про ADK взагалі — так її можна тестувати без моделі, ключа й мережі.
//
// Звірено проти google.golang.org/adk/v2 v2.4.0, Go 1.27.1 — станом на 27.09.2026.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/geminitool"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/adkenv"
)

// ErrProviderNotConfigured повідомляє, що для конфігурації немає credentials.
var ErrProviderNotConfigured = errors.New("провайдера не налаштовано")

// instruction — власна доменна інструкція агента.
//
// Три речі тут навмисно сформульовані як правила, а не як побажання:
// формат відповіді (щоб її можна було порівнювати між моделями), обов'язковий
// пошук для всього, що змінюється в часі (погода, події), і відмова поза
// доменом. Останнє дублюється кодом у DomainGuard — див. коментар там, чому
// одного тексту недостатньо.
const instruction = `Ти — планувальник вихідних для міста ЛЬВІВ (Україна). Відповідай українською.

ФОРМАТ ВІДПОВІДІ — завжди рівно три блоки і рядок бюджету:
Ранок: <що робити, де, скільки часу>
День: <...>
Вечір: <...>
Бюджет: <сумарна оцінка в гривнях, діапазон>

ПРАВИЛА:
1. Погода, афіша, години роботи, ціни — це дані, що змінюються щодня. Для них
   ОБОВ'ЯЗКОВО використовуй інструмент пошуку. Не називай прогноз погоди і не
   називай конкретну подію з пам'яті.
2. Якщо пошук недоступний або не дав відповіді — скажи це прямо і дай план,
   стійкий до погоди (варіанти в приміщенні). Не вигадуй ні прогноз, ні події.
3. Якщо в запиті тобі передали внутрішній довідник (список локацій, цін, годин)
   — відповідай ЛИШЕ за ним. Якщо потрібної інформації в довіднику немає, скажи
   рівно це: чого саме не вистачає і де це взяти. Не добудовуй відповідь
   здогадами з пам'яті.
4. Ти працюєш ТІЛЬКИ з дозвіллям, подіями, їжею, погодою й маршрутами у Львові.
   Усе інше — не твій домен: коротко відмов і назви, що ти вмієш.`

// outOfDomainReply — текст відмови доменної межі.
const outOfDomainReply = `Це поза моїм доменом: я планувальник вихідних у Львові — дозвілля, події, їжа, погода, маршрути.
Із цим питанням я не допоможу. Можу натомість скласти план на суботу чи неділю у Львові — з ранком, днем, вечором і бюджетом.`

// outOfDomainMarkers — слова, за якими межа розпізнає запит поза доменом.
//
// Список короткий і навмисно грубий. Це не класифікатор і не претендує ним
// бути: це поріг least agency — дешева перевірка, яка ловить типові
// «а порахуй мені податок» ДО того, як запит стане оплаченим викликом моделі.
// Межа свідомо асиметрична: хибне спрацювання дає користувачу зрозумілу
// відмову (яку він одразу переформулює), а пропуск дає агента, який упевнено
// відповідає поза своєю компетенцією. Друге дорожче.
var outOfDomainMarkers = []string{
	"податок", "податк", "декларац", "кредит", "іпотек", "інвест", "акці",
	"діагноз", "симптом", "лікув", "лікар", "препарат", "дозуван",
	"юридич", "позов", "адвокат", "договір",
	"напиши код", "напиши програм", "sql", "regex", "debug",
	"пароль", "api key", "приватний ключ",
}

// DomainGuard — доменна межа агента, реалізована як BeforeModelCallback.
//
// Навіщо код, якщо про відмову вже написано в інструкції: інструкція — це
// ПРОХАННЯ до моделі, а не властивість системи. Вона тримається на тому, що
// модель її послухає, і слабшає з кожним наступним рядком контексту.
// Callback — це властивість системи: ADK документує, що перший
// BeforeModelCallback, який повернув не-nil LLMResponse, СКАСОВУЄ виклик
// моделі. Тобто відмова тут:
//
//   - детермінована (її можна покрити тестом, а не «прогнати й подивитись»);
//   - безкоштовна (виклику моделі не відбулося — нуль токенів, нуль латентності);
//   - не залежить від того, яку модель ви підставили в бенчмарк.
//
// Саме тому в таблиці бенчмарку рядок «поза доменом» має 0 токенів на всіх
// конфігураціях: це не помилка вимірювання, а те, що межа спрацювала до моделі.
type DomainGuard struct {
	mu      sync.Mutex
	refused int
}

// Before реалізує llmagent.BeforeModelCallback.
func (g *DomainGuard) Before(_ agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if !g.outOfDomain(lastUserText(req)) {
		return nil, nil
	}
	g.mu.Lock()
	g.refused++
	g.mu.Unlock()
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  "model",
			Parts: []*genai.Part{{Text: outOfDomainReply}},
		},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}, nil
}

// Refused повертає кількість запитів, зупинених межею.
func (g *DomainGuard) Refused() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused
}

// outOfDomain — сама перевірка, винесена окремо рівно заради тесту.
func (g *DomainGuard) outOfDomain(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range outOfDomainMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// lastUserText дістає текст останнього користувацького повідомлення.
//
// Перевіряти саме останнє, а не весь запит: системна інструкція теж їде в
// LLMRequest, і в ній є слово «пароль» — межа спрацьовувала б на кожному
// запиті, включно з валідними.
func lastUserText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role == "model" {
			continue
		}
		var b strings.Builder
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteString(" ")
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// Meter знімає латентність і токени кожного виклику моделі.
//
// Відмінність від «обгортки навколо model.LLM»: пара Before/After бачить
// РІВНО те, що агент відправив і отримав, разом із системною інструкцією та
// результатами інструментів. Обгортка бачить те саме, але не має доступу до
// agent.Context, тож не відрізняє виклик усередині tool-циклу від першого.
//
// Один Meter — на ОДИН прогін (одна пара конфігурація×промпт). Конкурентність
// у harness влаштована так, що кожна горутина будує власний агент і власний
// Meter: спільний Meter на паралельних гілках виміряв би суму, вважаючи, що
// виміряв крок. Мьютекс тут усе одно є — усередині одного прогону ADK робить
// кілька викликів моделі (tool-цикл), і вони можуть іти з різних горутин.
type Meter struct {
	mu      sync.Mutex
	start   time.Time
	calls   []Call
	pending bool
}

// Call — один виклик моделі.
type Call struct {
	Latency   time.Duration `json:"latency_ns"`
	InTokens  int           `json:"in_tokens"`
	OutTokens int           `json:"out_tokens"`
	Refused   bool          `json:"refused"`
}

// Before реалізує llmagent.BeforeModelCallback — запускає секундомір.
//
// Повертає (nil, nil) завжди: Meter нічого не вирішує, він тільки міряє.
// Порядок callbacks у конфігурації агента має значення — DomainGuard стоїть
// ПЕРЕД Meter'ом, тож зупинений запит до секундоміра не доходить.
func (m *Meter) Before(_ agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.start = time.Now()
	m.pending = true
	return nil, nil
}

// After реалізує llmagent.AfterModelCallback — зупиняє секундомір і читає usage.
func (m *Meter) After(_ agent.Context, resp *model.LLMResponse, respErr error) (*model.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pending {
		// Відповідь без свого Before — це відповідь, яку підставив
		// DomainGuard. Її не міряємо: латентність тут — це швидкість
		// strings.Contains, і в таблиці вона б виглядала як «модель
		// відповіла за 3 мікросекунди».
		return nil, respErr
	}
	m.pending = false

	call := Call{Latency: time.Since(m.start)}
	if u := usageOf(resp); u != nil {
		call.InTokens = int(u.PromptTokenCount)
		call.OutTokens = int(u.CandidatesTokenCount) + int(u.ThoughtsTokenCount)
	}
	m.calls = append(m.calls, call)
	return nil, respErr
}

// Calls повертає копію зібраних вимірів.
func (m *Meter) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// usageOf дістає usage-метадані, переживаючи їх відсутність.
//
// Провайдер, який їх не повертає, — це не помилка: OpenAI-сумісні бекенди
// віддають usage не в кожному режимі. Тоді токени в таблиці будуть нулями, і
// це чесніше за оцінку токенайзером, видану за виміряне значення.
func usageOf(resp *model.LLMResponse) *genai.GenerateContentResponseUsageMetadata {
	if resp == nil || resp.UsageMetadata == nil {
		return nil
	}
	return resp.UsageMetadata
}

// NewAgent будує агента-планувальника для однієї конфігурації моделі.
//
// Повертає ще й Meter та DomainGuard — без них агент не спостережуваний, а
// бенчмарк на ньому не побудуєш.
func NewAgent(m model.LLM, withSearch bool) (agent.Agent, *Meter, *DomainGuard, error) {
	if m == nil {
		return nil, nil, nil, errors.New("NewAgent: model is nil")
	}
	meter := &Meter{}
	guard := &DomainGuard{}

	var tools []tool.Tool
	if withSearch {
		// geminitool.GoogleSearch — вбудований grounding, і він працює ЛИШЕ
		// на бекенді Gemini. Через agentgateway/OpenAI-сумісний маршрут його
		// підключати не можна: бекенд відхилить незнайомий tool, і падіння
		// виглядатиме як помилка агента, якою воно не є.
		tools = append(tools, geminitool.GoogleSearch{})
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "lviv_weekend_planner",
		Model:       m,
		Description: "Планувальник вихідних у Львові: ранок/день/вечір + бюджет.",
		Instruction: instruction,
		Tools:       tools,
		// Порядок обов'язковий: межа перед метрикою. Інакше Meter запустить
		// секундомір на запиті, який межа зараз скасує, і в таблиці з'явиться
		// виклик, якого не було.
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{guard.Before, meter.Before},
		AfterModelCallbacks:  []llmagent.AfterModelCallback{meter.After},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build agent: %w", err)
	}
	return a, meter, guard, nil
}

// BuildModel будує бекенд для конфігурації.
//
// Три маршрути, і різниця між ними — рівно BaseURL:
//
//   - gateway: OpenAI-сумісний endpoint agentgateway на :4000. Агент не знає,
//     що говорить із проксі; натомість ми отримуємо трасу, метрику й
//     РЕАЛІЗОВАНУ вартість запиту в access-log. Це друге джерело правди про
//     гроші — див. gateway.go.
//   - gemini / openai: прямий вендорський API.
//
// Ключ гейтвея не опційний: конфіг стека тримає apiKey у режимі strict, і
// порожній ключ дає 401, який читається як «шлюз зламався».
func BuildModel(ctx context.Context, cfg Config) (model.LLM, error) {
	switch cfg.Backend {
	case BackendGateway:
		base, ok := adkenv.Key("AGENTGATEWAY_BASE_URL")
		if !ok {
			base = "http://localhost:4000/v1"
		}
		key, ok := adkenv.Key("AGENTGATEWAY_API_KEY")
		if !ok {
			return nil, fmt.Errorf("%w: %s потребує AGENTGATEWAY_API_KEY", ErrProviderNotConfigured, cfg.Label)
		}
		m, err := openaimodel.NewModel(ctx, cfg.Model, &openaimodel.ClientConfig{
			APIKey:  key,
			BaseURL: base,
		})
		if err != nil {
			return nil, fmt.Errorf("agentgateway %s: %w", cfg.Model, err)
		}
		return m, nil

	case BackendGemini:
		key, ok := firstKey("GOOGLE_API_KEY", "GEMINI_API_KEY")
		if !ok {
			return nil, fmt.Errorf("%w: %s потребує GOOGLE_API_KEY", ErrProviderNotConfigured, cfg.Label)
		}
		m, err := gemini.NewModel(ctx, cfg.Model, &genai.ClientConfig{APIKey: key})
		if err != nil {
			return nil, fmt.Errorf("gemini %s: %w", cfg.Model, err)
		}
		return m, nil

	case BackendOpenAI:
		key, ok := adkenv.Key("OPENAI_API_KEY")
		if !ok {
			return nil, fmt.Errorf("%w: %s потребує OPENAI_API_KEY", ErrProviderNotConfigured, cfg.Label)
		}
		m, err := openaimodel.NewModel(ctx, cfg.Model, &openaimodel.ClientConfig{APIKey: key})
		if err != nil {
			return nil, fmt.Errorf("openai %s: %w", cfg.Model, err)
		}
		return m, nil

	default:
		return nil, fmt.Errorf("невідомий бекенд %q у конфігурації %q", cfg.Backend, cfg.Label)
	}
}

// firstKey віддає перше значення з переліку синонімів змінної.
//
// GOOGLE_API_KEY і GEMINI_API_KEY — це те саме, але різні інструменти пишуть
// у різні імена. Читаємо через adkenv.Key, а не os.Getenv: порожня-але-
// виставлена змінна не має вважатися налаштованим провайдером, інакше замість
// «не налаштовано» ви отримаєте незрозумілий 401.
func firstKey(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := adkenv.Key(n); ok {
			return v, true
		}
	}
	return "", false
}

// LoadEnv підтягує apps/.env, мовчки переживаючи його відсутність.
func LoadEnv() error {
	if err := adkenv.Load("."); err != nil && !errors.Is(err, adkenv.ErrNotFound) {
		return err
	}
	return nil
}
