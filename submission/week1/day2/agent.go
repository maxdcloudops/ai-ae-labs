// ADK-проводка ДЗ 2: типізований інструмент і агент навколо нього.
//
// Тут же живе порівняння двох способів задати схему — автогенерація з
// Go-структур проти явної functiontool.Config.InputSchema (++ Advanced).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/dimetron/ai-eng-course/labs/internal/adkenv"
)

// toolName — ім'я інструмента. Одне місце: воно їде і в схему, і в інструкцію,
// і в тести, і розходження між ними дало б агента, який просить викликати
// інструмент, якого немає.
const toolName = "get_exchange_rate"

// toolDescription — опис, за яким модель вирішує, ВИКЛИКАТИ чи ні.
//
// Це найнедооціненіший рядок у всьому файлі. Модель не читає ваш код — вона
// читає цей опис. Тому він каже не лише «що робить інструмент», а й КОЛИ його
// викликати і коли перепитати. Опис «returns exchange rate» дає агента, який
// половину запитів відповідає з пам'яті.
const toolDescription = `Returns the official exchange rate between two ISO 4217 currencies,
cross-rated through UAH using National Bank of Ukraine data, optionally with recent daily history.

Call this tool for ANY question about a currency rate, conversion or amount in another currency —
including "how much is X in Y". Never state a rate from memory: rates change daily.
If the user names a currency you cannot map to a three-letter ISO 4217 code, ask them to
clarify instead of guessing — several currencies are called "dollar".`

// instruction — системна інструкція агента.
//
// Порівняйте її з валідацією в contract.go: інструкція КЕРУЄ поведінкою, але
// нічого не гарантує. Саме тому найважливіші правила тут продубльовані кодом,
// а не залишені на добру волю моделі.
const instruction = `Ти відповідаєш на питання про курси валют для українських користувачів.

Правила:
- Для БУДЬ-ЯКОГО питання про курс чи конвертацію викликай ` + toolName + `.
  Ніколи не називай курс із пам'яті: він змінюється щодня.
- Інструмент повертає походження (source.kind: api/cache/mock) і дату. Назви обидва
  в відповіді — число без джерела й дати не можна перевірити.
- Якщо інструмент повернув помилку з конкретним кодом валюти — виправ аргументи
  й виклич його ще раз, не більше одного разу.
- Якщо після цього інструмент усе одно повертає помилку — скажи прямо, що курс
  отримати не вдалося, і назви причину. НЕ вигадуй число.
- Якщо користувач назвав валюту, яку неможливо однозначно зіставити з кодом
  ISO 4217 («долар» — їх багато), перепитай, а не вгадуй.`

// NewRateTool будує типізований інструмент зі схемою, ВИВЕДЕНОЮ зі структур.
//
// Що тут відбувається без жодного рядка JSON Schema від руки: functiontool
// дивиться на сигнатуру handler-а (RateInput → RateQuote) і будує обидві схеми
// сам — включно з трьома рівнями вкладеності, масивом History і описами полів
// із тегів `jsonschema`.
//
// Handler отримує ВЖЕ типізований RateInput. Це і є вся суть дня: системний код
// ніколи не бачить сирого тексту моделі — або типізована структура, або
// конкретна помилка.
func NewRateTool(p Provider, policy RepairPolicy) (tool.Tool, error) {
	handler := func(ctx agent.Context, in RateInput) (RateQuote, error) {
		// Цикл «валідація → ремонт → повторна валідація» стоїть ПЕРЕД
		// зверненням до провайдера: інакше за кожну одруківку моделі платить
		// апстрім (і ми — лімітом запитів до НБУ).
		res := TryRepair(in, policy)
		if res.Err != nil {
			return RateQuote{}, res.Err
		}

		quote, err := Quote(ctx, p, res.Input)
		if err != nil {
			return RateQuote{}, err
		}

		// Ремонт має бути ВИДИМИМ у відповіді, а не тихим. Користувач, який
		// спитав про "usd", має бачити, що йому відповіли про USD — інакше
		// одного дня тихий ремонт відповість про іншу валюту, і ніхто не
		// зрозуміє, звідки взялося число.
		if res.Repaired {
			note := "arguments were normalized before lookup: " + summarize(res.Steps)
			if quote.Note != "" {
				note = quote.Note + "; " + note
			}
			quote.Note = note
		}
		return quote, nil
	}

	t, err := functiontool.New(functiontool.Config{
		Name:        toolName,
		Description: toolDescription,
	}, handler)
	if err != nil {
		return nil, fmt.Errorf("build rate tool: %w", err)
	}
	return t, nil
}

// NewStrictRateTool — той самий інструмент, але зі ЯВНОЮ InputSchema.
//
// ++ Advanced: «порівняйте автогенерацію проти явної InputSchema».
//
// Різниця не косметична. Автогенерована схема описує ФОРМУ (рядок, рядок,
// число) і несе описи з тегів. Явна схема додає ОБМЕЖЕННЯ, яких у системі
// типів Go немає: `required`, `minLength`/`maxLength`, `pattern`, `minimum`/
// `maximum`. Модель бачить їх ДО виклику — і половина невалідних викликів не
// відбувається взагалі.
//
// Що при цьому втрачається: схема перестає слідувати за структурою. Додасте
// поле в RateInput — автогенерована оновиться сама, явна мовчки лишиться
// старою, і модель ніколи не дізнається про нове поле. Тому для явної схеми
// потрібен тест, який звіряє її з типом (TestExplicitSchemaMatchesStruct).
func NewStrictRateTool(p Provider, policy RepairPolicy) (tool.Tool, error) {
	handler := func(ctx agent.Context, in RateInput) (RateQuote, error) {
		res := TryRepair(in, policy)
		if res.Err != nil {
			return RateQuote{}, res.Err
		}
		return Quote(ctx, p, res.Input)
	}

	t, err := functiontool.New(functiontool.Config{
		Name:        toolName,
		Description: toolDescription,
		InputSchema: StrictInputSchema(),
	}, handler)
	if err != nil {
		return nil, fmt.Errorf("build strict rate tool: %w", err)
	}
	return t, nil
}

// StrictInputSchema — явна схема входу з обмеженнями.
//
// `pattern: ^[A-Za-z]{3}$` — саме те обмеження, якого не виражає тип `string`.
// Воно не скасовує доменну валідацію (схема не знає, які валюти публікує НБУ
// сьогодні), але прибирає цілий клас викликів ще до виконання.
func StrictInputSchema() *jsonschema.Schema {
	maxDays := float64(maxHistoryDays)
	zero := float64(0)
	three := 3
	return &jsonschema.Schema{
		Type:     "object",
		Required: []string{"base", "target"},
		Properties: map[string]*jsonschema.Schema{
			"base": {
				Type:        "string",
				Description: "ISO 4217 code of the base currency, three latin letters, e.g. USD",
				Pattern:     "^[A-Za-z]{3}$",
				MinLength:   &three,
				MaxLength:   &three,
			},
			"target": {
				Type:        "string",
				Description: "ISO 4217 code of the target currency, three latin letters, e.g. UAH",
				Pattern:     "^[A-Za-z]{3}$",
				MinLength:   &three,
				MaxLength:   &three,
			},
			"history_days": {
				Type:        "integer",
				Description: "How many recent business days of history to include; 0 means current rate only",
				Minimum:     &zero,
				Maximum:     &maxDays,
			},
		},
	}
}

// summarize стискає кроки ремонту в один рядок для поля Note.
func summarize(steps []RepairStep) string {
	var parts []string
	for _, s := range steps {
		if strings.HasPrefix(s.Action, "repaired") {
			parts = append(parts, strings.TrimSuffix(strings.TrimPrefix(s.Action, "repaired ("), ")"))
		}
	}
	return strings.Join(parts, "; ")
}

// NewAgent збирає агента навколо інструмента.
func NewAgent(m model.LLM, p Provider, strict bool) (agent.Agent, error) {
	var (
		t   tool.Tool
		err error
	)
	if strict {
		t, err = NewStrictRateTool(p, DefaultRepairPolicy())
	} else {
		t, err = NewRateTool(p, DefaultRepairPolicy())
	}
	if err != nil {
		return nil, err
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "currency_agent",
		Model:       m,
		Description: "Відповідає на питання про курси валют за офіційними даними НБУ.",
		Instruction: instruction,
		Tools:       []tool.Tool{t},
	})
	if err != nil {
		return nil, fmt.Errorf("build agent: %w", err)
	}
	return a, nil
}

// ToolCallLog фіксує кожен виклик інструмента — вхід, вихід і помилку.
//
// Потрібен для README: діалог «модель самовиправилася» неможливо задокументувати
// чесно, не показавши ОБИДВА виклики — той, що впав, і той, що пройшов.
type ToolCallLog struct {
	mu    sync.Mutex
	calls []ToolCall
}

// ToolCall — один виклик інструмента.
type ToolCall struct {
	Input RateInput  `json:"input"`
	Quote *RateQuote `json:"quote,omitempty"`
	Err   string     `json:"error,omitempty"`
}

// Wrap обгортає провайдера так, щоб кожен виклик потрапляв у лог.
func (l *ToolCallLog) record(in RateInput, q *RateQuote, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	call := ToolCall{Input: in, Quote: q}
	if err != nil {
		call.Err = err.Error()
	}
	l.calls = append(l.calls, call)
}

// Calls повертає копію логу.
func (l *ToolCallLog) Calls() []ToolCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ToolCall, len(l.calls))
	copy(out, l.calls)
	return out
}

// NewLoggedRateTool — інструмент, який пише кожен виклик у лог.
func NewLoggedRateTool(p Provider, policy RepairPolicy, log *ToolCallLog) (tool.Tool, error) {
	handler := func(ctx agent.Context, in RateInput) (RateQuote, error) {
		res := TryRepair(in, policy)
		if res.Err != nil {
			log.record(in, nil, res.Err)
			return RateQuote{}, res.Err
		}
		quote, err := Quote(ctx, p, res.Input)
		if err != nil {
			log.record(in, nil, err)
			return RateQuote{}, err
		}
		if res.Repaired {
			quote.Note = "arguments were normalized before lookup: " + summarize(res.Steps)
		}
		log.record(in, &quote, nil)
		return quote, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        toolName,
		Description: toolDescription,
	}, handler)
	if err != nil {
		return nil, fmt.Errorf("build logged rate tool: %w", err)
	}
	return t, nil
}

// NewLoggedAgent — агент із логованим інструментом.
func NewLoggedAgent(m model.LLM, p Provider) (agent.Agent, *ToolCallLog, error) {
	log := &ToolCallLog{}
	t, err := NewLoggedRateTool(p, DefaultRepairPolicy(), log)
	if err != nil {
		return nil, nil, err
	}
	a, err := llmagent.New(llmagent.Config{
		Name:        "currency_agent",
		Model:       m,
		Description: "Відповідає на питання про курси валют за офіційними даними НБУ.",
		Instruction: instruction,
		Tools:       []tool.Tool{t},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build agent: %w", err)
	}
	return a, log, nil
}

// BuildModel будує бекенд. Той самий набір, що в ДЗ 1.
func BuildModel(ctx context.Context, gateway bool, name string) (model.LLM, error) {
	if gateway {
		base, ok := adkenv.Key("AGENTGATEWAY_BASE_URL")
		if !ok {
			base = "http://localhost:4000/v1"
		}
		key, ok := adkenv.Key("AGENTGATEWAY_API_KEY")
		if !ok {
			return nil, errors.New("gateway-режим потребує AGENTGATEWAY_API_KEY")
		}
		return newOpenAICompatible(ctx, name, key, base)
	}
	key, ok := firstKey("GOOGLE_API_KEY", "GEMINI_API_KEY")
	if !ok {
		return nil, errors.New("живий режим потребує GOOGLE_API_KEY (або GEMINI_API_KEY) в apps/.env")
	}
	return newGemini(ctx, name, key)
}

func firstKey(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := adkenv.Key(n); ok {
			return v, true
		}
	}
	return "", false
}

// LoadEnv підтягує apps/.env, переживаючи його відсутність.
func LoadEnv() {
	if err := adkenv.Load("."); err != nil && !errors.Is(err, adkenv.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}
}
