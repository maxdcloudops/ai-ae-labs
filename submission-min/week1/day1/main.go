// ДЗ 1 (базовий шлях) — агент-планувальник вихідних у Львові.
//
// Це стартовий шаблон із заповненими TODO й нічим більше. Уся «магія» —
// в інструкції агента; бенчмарк робиться зміною ОДНІЄЇ змінної оточення
// (MODEL) і повторним запуском, а не кодом:
//
//	echo "План на суботу?" | MODEL=gemini-2.5-flash go run . console
//	echo "План на суботу?" | MODEL=gemini-2.5-pro   go run . console
//
// Латентність кожного виклику друкує timedLLM у stderr — її й заносимо в
// таблицю README.
package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/geminitool"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/adkenv"
)

// instruction — власна доменна інструкція (TODO №2 зі стартового шаблону).
//
// Чотири правила: місто й формат відповіді, обов'язковий пошук для того, що
// змінюється щодня, чесність щодо переданого контексту, і відмова поза доменом.
const instruction = `Ти — планувальник вихідних для міста ЛЬВІВ. Відповідай українською.

ФОРМАТ — завжди три блоки і рядок бюджету:
Ранок: <що робити, де, скільки часу>
День: <...>
Вечір: <...>
Бюджет: <оцінка в гривнях, діапазон>

ПРАВИЛА:
1. Погоду, афішу, години роботи й ціни ОБОВ'ЯЗКОВО перевіряй пошуком —
   це дані, що змінюються щодня. Не називай їх із пам'яті.
2. Якщо пошук недоступний — скажи це прямо і дай план, стійкий до погоди.
   Не вигадуй ні прогноз, ні події.
3. Якщо тобі передали довідник у запиті — відповідай ЛИШЕ за ним. Чого в
   ньому немає, того не добудовуй здогадами: скажи, чого саме бракує.
4. Ти працюєш ТІЛЬКИ з дозвіллям, подіями, їжею, погодою й маршрутами у
   Львові. Усе інше — не твій домен: коротко відмов і назви, що ти вмієш.`

func main() {
	ctx := context.Background()

	// apps/.env підхоплюється сам; явний export має пріоритет.
	if err := adkenv.Load("."); err != nil && !errors.Is(err, adkenv.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}

	m, choice, searchOK, err := buildModel(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Модель: %s", choice)

	// TODO №1 зі стартового шаблону — міні-бенчмарк. Коду він не потребує:
	// модель береться зі змінної MODEL, тож прогін на 2–3 конфігураціях — це
	// три запуски однієї команди. Якби модель була в коді, ви порівнювали б
	// дві різні програми, а не дві моделі.
	m = timedLLM{m}

	var tools []tool.Tool
	if searchOK {
		// GoogleSearch працює лише на бекенді Gemini: OpenAI-сумісний
		// маршрут відхилить незнайомий tool.
		tools = append(tools, geminitool.GoogleSearch{})
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "lviv_weekend_planner",
		Model:       m,
		Description: "Планувальник вихідних у Львові: ранок/день/вечір + бюджет.",
		Instruction: instruction,
		Tools:       tools,
	})
	if err != nil {
		log.Fatalf("не вдалося створити агента: %v", err)
	}

	l := full.NewLauncher()
	cfg := &launcher.Config{AgentLoader: agent.NewSingleLoader(a)}
	if err := l.Execute(ctx, cfg, os.Args[1:]); err != nil {
		log.Fatalf("запуск не вдався: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

// buildModel обирає бекенд за наявними credentials.
//
// Третє значення — чи доступний вбудований пошук: він є тільки в Gemini.
func buildModel(ctx context.Context) (model.LLM, string, bool, error) {
	name := os.Getenv("MODEL")

	if key, ok := firstKey("GOOGLE_API_KEY", "GEMINI_API_KEY"); ok {
		if name == "" {
			name = "gemini-2.5-flash"
		}
		m, err := gemini.NewModel(ctx, name, &genai.ClientConfig{APIKey: key})
		return m, "gemini → " + name, true, err
	}

	// Без ключа — будь-який OpenAI-сумісний endpoint (локальний шлюз, Ollama).
	if base, ok := adkenv.Key("OPENAI_BASE_URL"); ok {
		if name == "" {
			name = "mock/mock-gpt"
		}
		key, _ := adkenv.Key("OPENAI_API_KEY")
		m, err := openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{
			APIKey: key, BaseURL: base,
		})
		return m, "openai-compatible → " + name, false, err
	}

	return nil, "", false, errors.New(
		"провайдера не налаштовано: впишіть GOOGLE_API_KEY у apps/.env " +
			"або задайте OPENAI_BASE_URL для локального endpoint")
}

func firstKey(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := adkenv.Key(n); ok {
			return v, true
		}
	}
	return "", false
}

// timedLLM друкує латентність кожного виклику — з неї й складається таблиця
// бенчмарку в README.
type timedLLM struct{ model.LLM }

func (t timedLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		start := time.Now()
		var in, out int32
		for resp, err := range t.LLM.GenerateContent(ctx, req, stream) {
			if resp != nil && resp.UsageMetadata != nil {
				in = resp.UsageMetadata.PromptTokenCount
				out = resp.UsageMetadata.CandidatesTokenCount
			}
			if !yield(resp, err) {
				return
			}
		}
		log.Printf("[llm] model=%s total=%s tokens in/out=%d/%d",
			req.Model, time.Since(start).Round(time.Millisecond), in, out)
	}
}
