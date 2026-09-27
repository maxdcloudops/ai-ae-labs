// Проводка між ADK-агентом і ADK-free харнесом.
//
// Тут живе рівно одна функція-шов: ADKRunner повертає RunFunc, яку харнес
// викликає, не знаючи ні про агента, ні про сесії, ні про провайдера.
package main

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
)

// ADKRunner будує RunFunc на справжньому ADK-агенті.
//
// Ключова деталь — кеш моделей: бекенд створюється ОДИН раз на конфігурацію, а
// агент і Meter — на кожен прогін. Причина в обох напрямках:
//
//   - будувати бекенд на кожен прогін означає міряти ще й час створення
//     HTTP-клієнта, який до моделі не має стосунку;
//   - перевикористовувати Meter між прогонами означає, що другий вимір тягне
//     виміри першого, і медіана поїде.
//
// Кожен прогін іде у власній сесії (labrun.Run створює нову), тож другий
// промпт не платить за історію першого. Без цього кожен наступний прогін
// дорожчий за попередній, і ви виміряєте зростання контексту, а не модель.
func ADKRunner(offline bool, searchEnabled bool) RunFunc {
	var (
		mu     sync.Mutex
		models = map[string]model.LLM{}
	)

	return func(ctx context.Context, cfg Config, p Prompt) (Observation, error) {
		mu.Lock()
		m, ok := models[cfg.Label]
		if !ok {
			var err error
			if offline {
				m = offlineModel(cfg)
			} else {
				m, err = BuildModel(ctx, cfg)
			}
			if err != nil {
				mu.Unlock()
				return Observation{}, err
			}
			models[cfg.Label] = m
		}
		mu.Unlock()

		// GoogleSearch працює лише на бекенді Gemini: OpenAI-сумісний маршрут
		// (гейтвей, Ollama) відхилить незнайомий tool, і падіння виглядало б як
		// помилка агента. Тому пошук підключаємо тільки там, де він справді є.
		withSearch := searchEnabled && cfg.Backend == BackendGemini

		a, meter, guard, err := NewAgent(m, withSearch)
		if err != nil {
			return Observation{}, err
		}

		res, err := labrun.Run(ctx, a, p.Text)
		if err != nil {
			return Observation{}, fmt.Errorf("%s / %s: %w", cfg.Label, p.Name, err)
		}

		return Observation{
			Calls:   meter.Calls(),
			Answer:  strings.Join(res.Texts(), "\n"),
			Refused: guard.Refused() > 0,
		}, nil
	}
}

// offlineAnswer — відповідь скриптованої моделі на звичайний запит плану.
//
// Вона навмисно ВИКОНУЄ контракт інструкції (три блоки + бюджет): офлайн-режим
// існує, щоб перевірити харнес — метрику, арифметику, агрегацію, оцінку
// формату, — і скриптована відповідь, яка контракт порушує, перевіряла б лише
// те, що оцінка формату вміє ставити нуль.
//
// Що НЕ можна читати з офлайн-прогону: латентність (запиту в мережу не було) і
// «якість» (текст написав автор лаби, а не модель). У таблиці це позначено
// режимом у футері, а не приміткою дрібним шрифтом.
const offlineAnswer = `Ранок: Високий замок — підйом на огляд міста, 1.5 години, вхід вільний.
День: обід у Старому місті, далі Аптека-музей, ~2 години.
Вечір: Львівська опера або концерт у філармонії, за наявності квитків.
Бюджет: 700–1100 грн на особу.

Застереження: прогноз погоди й актуальну афішу я не підтверджував пошуком у цьому прогоні.`

// offlineRefusalAnswer — що скриптована модель відповідає на context-stress.
const offlineRefusalAnswer = `У переданому довіднику немає годин роботи на неділю для галереї "Дзиґа" —
у ньому є лише субота (сб 11:00–19:00, вхід 120 грн). Назвати неділю я не можу,
не вигадавши її. Потрібен або рядок довідника з неділею, або пошук по сайту локації.`

// scriptedModel — скриптована модель, яка вибирає відповідь ЗА СЦЕНАРІЄМ.
//
// Чому не fakellm.New зі списком turn-ів: список віддає відповіді по порядку, і
// на одному запиті context-stress ви отримуєте ту відповідь, яка випала за
// індексом, а не ту, що доречна. Виглядає це так, наче агент проігнорував
// переданий довідник — тобто офлайн-режим починає брехати саме про ту
// поведінку, яку його просили продемонструвати.
//
// Тут модель дивиться на текст запиту й відповідає чесно там, де чесна
// відповідь — «цих даних мені не передали». Це все ще скрипт, а не модель, і
// README це каже прямо; але скрипт, який відповідає НА ПИТАННЯ, а не на
// порядковий номер.
type scriptedModel struct{ name string }

// Name реалізує model.LLM.
func (m scriptedModel) Name() string { return m.name }

// GenerateContent реалізує model.LLM.
//
// Usage заповнюється навмисно: без нього офлайн-прогін дає 0 токенів, нульову
// вартість і таблицю, у якій колонка «$» нічого не перевіряє. Оцінка груба
// (слова, не BPE) — і саме тому вона зветься оцінкою.
func (m scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		question := lastUserText(req)
		answer := offlineAnswer
		if asksBeyondGivenContext(question) {
			answer = offlineRefusalAnswer
		}

		in := int32(len(strings.Fields(promptText(req))))
		out := int32(len(strings.Fields(answer)))
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: answer}},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     in,
				CandidatesTokenCount: out,
				TotalTokenCount:      in + out,
			},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// asksBeyondGivenContext розпізнає context-stress: у запиті є довідник, а
// питають про те, чого в ньому немає.
func asksBeyondGivenContext(question string) bool {
	lower := strings.ToLower(question)
	return strings.Contains(lower, "довідник") && strings.Contains(lower, "неділю")
}

// promptText збирає весь текст запиту — разом із системною інструкцією.
//
// Саме вона і є той невидимий рахунок, який платиться в КОЖНОМУ виклику: у цій
// лабі інструкція займає більше токенів, ніж сам запит користувача.
func promptText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			if p != nil {
				b.WriteString(p.Text)
				b.WriteString(" ")
			}
		}
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteString(" ")
			}
		}
	}
	return b.String()
}

// offlineModel віддає скриптовану модель.
func offlineModel(cfg Config) model.LLM {
	return scriptedModel{name: cfg.Model}
}
