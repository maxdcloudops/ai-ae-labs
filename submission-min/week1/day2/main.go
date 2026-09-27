// ДЗ 2 (базовий шлях) — типізований інструмент курсу валют.
//
// Увесь базовий шлях в одному файлі: контракт, валідація, провайдер (НБУ,
// без ключа) і проводка в агента через functiontool.New.
//
//	go run . console              # діалог з агентом
//	go run . quote USD UAH 3      # прямий виклик інструмента, без моделі
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/adkenv"
)

// --- Контракт -----------------------------------------------------------

var (
	errInvalidCode     = errors.New("invalid currency code")
	errUnknownCurrency = errors.New("unknown currency code")
	errUpstream        = errors.New("rate provider unavailable")
)

// RateInput — вхід. Тег `jsonschema` — це ОПИС поля, а не список обмежень:
// `jsonschema:"required,enum=UAH"` зробив би описом літеральний рядок
// "required,enum=UAH". Обмеження — у валідації нижче.
//
// `omitempty` на history_days обов'язковий: у Go немає опційних полів, тож
// автогенератор схеми позначає ВСІ поля як required, і виклик без цього поля
// відхиляється ще до handler-а.
type RateInput struct {
	Base        string `json:"base" jsonschema:"ISO 4217 code of the base currency, e.g. USD"`
	Target      string `json:"target" jsonschema:"ISO 4217 code of the target currency, e.g. UAH"`
	HistoryDays int    `json:"history_days,omitempty" jsonschema:"Recent business days of history to include, 0 to 14"`
}

// SourceKind — enum походження числа (рівень 3 вкладеності).
type SourceKind string

const (
	SourceAPI  SourceKind = "api"
	SourceMock SourceKind = "mock"
)

// Source — рівень 3: звідки взялося конкретне число.
type Source struct {
	Kind SourceKind `json:"kind" jsonschema:"Where this number came from: api or mock"`
	Name string     `json:"name" jsonschema:"Provider identifier, e.g. nbu"`
}

// HistoricalPoint — рівень 2, елемент МАСИВУ. Кожна точка несе власний
// Source: сьогоднішній курс може бути з API, вчорашній — з іншого джерела,
// і один Source на всю відповідь брехав би про половину чисел.
type HistoricalPoint struct {
	Date   string  `json:"date" jsonschema:"Rate date, YYYY-MM-DD"`
	Rate   float64 `json:"rate" jsonschema:"Rate on that date"`
	Source Source  `json:"source" jsonschema:"Provenance of this data point"`
}

// RateOutput — рівень 1. Три рівні + масив + enum:
// RateOutput → history[] → source.kind.
type RateOutput struct {
	Base    string            `json:"base"`
	Target  string            `json:"target"`
	Rate    float64           `json:"rate" jsonschema:"How many units of target one unit of base buys"`
	AsOf    string            `json:"as_of" jsonschema:"Rate date, YYYY-MM-DD"`
	Source  Source            `json:"source" jsonschema:"Provenance of the current rate"`
	History []HistoricalPoint `json:"history" jsonschema:"Recent daily rates, oldest first"`
}

// normalizeCode валідує форму коду й нормалізує регістр.
//
// Помилка навмисно називає КОНКРЕТНЕ значення: саме цей текст побачить
// модель, і саме за ним вона виправить власний виклик. «Bad request» не дав
// би їй нічого, і наступна спроба була б такою самою.
func normalizeCode(code string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(code))
	if len([]rune(c)) != 3 {
		return "", fmt.Errorf("%w: %q must be three letters (ISO 4217)", errInvalidCode, code)
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%w: %q must be three LATIN letters (ISO 4217)", errInvalidCode, code)
		}
	}
	return c, nil
}

// --- Провайдер: НБУ, публічний, без ключа -------------------------------

const nbuURL = "https://bank.gov.ua/NBUStatService/v1/statdirectory/exchange"

type nbuRow struct {
	Rate         float64 `json:"rate"`
	CC           string  `json:"cc"`
	ExchangeDate string  `json:"exchangedate"` // DD.MM.YYYY
}

// client із таймаутом: http.DefaultClient таймауту не має взагалі, і один
// завислий апстрім тече горутиною назавжди.
var client = &http.Client{Timeout: 10 * time.Second}

func fetchNBU(ctx context.Context, url string) ([]nbuRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstream, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %s", errUpstream, resp.Status)
	}
	var rows []nbuRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", errUpstream, err)
	}
	return rows, nil
}

func isoDate(s string) string {
	if t, err := time.Parse("02.01.2006", s); err == nil {
		return t.Format("2006-01-02")
	}
	return s
}

// Convert рахує крос-курс через гривню — це вісь, у якій НБУ публікує таблицю.
func Convert(ctx context.Context, in RateInput) (RateOutput, error) {
	base, err := normalizeCode(in.Base)
	if err != nil {
		return RateOutput{}, fmt.Errorf("field \"base\": %w", err)
	}
	target, err := normalizeCode(in.Target)
	if err != nil {
		return RateOutput{}, fmt.Errorf("field \"target\": %w", err)
	}
	if in.HistoryDays < 0 || in.HistoryDays > 14 {
		return RateOutput{}, fmt.Errorf("field \"history_days\": %d is outside 0..14", in.HistoryDays)
	}

	rows, err := fetchNBU(ctx, nbuURL+"?json")
	if err != nil {
		return RateOutput{}, err
	}
	rates := make(map[string]float64, len(rows))
	for _, r := range rows {
		rates[strings.ToUpper(r.CC)] = r.Rate
	}

	baseUAH, err := toUAH(base, rates)
	if err != nil {
		return RateOutput{}, err
	}
	targetUAH, err := toUAH(target, rates)
	if err != nil {
		return RateOutput{}, err
	}

	src := Source{Kind: SourceAPI, Name: "nbu"}
	out := RateOutput{
		Base: base, Target: target,
		Rate:    baseUAH / targetUAH,
		AsOf:    isoDate(rows[0].ExchangeDate),
		Source:  src,
		History: []HistoricalPoint{}, // не nil: у JSON nil-слайс стає null
	}

	// Історія: окремий запит на день. Дні без котирувань (вихідні, свята)
	// пропускаємо — заповнити їх учорашнім числом означало б вигадати курс.
	for i := in.HistoryDays; i >= 1; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i)
		b, ok1 := rateOn(ctx, base, day)
		t, ok2 := rateOn(ctx, target, day)
		if !ok1 || !ok2 || t <= 0 {
			continue
		}
		out.History = append(out.History, HistoricalPoint{
			Date: day.Format("2006-01-02"), Rate: b / t, Source: src,
		})
	}
	return out, nil
}

func rateOn(ctx context.Context, code string, day time.Time) (float64, bool) {
	if code == "UAH" {
		return 1, true
	}
	rows, err := fetchNBU(ctx, fmt.Sprintf("%s?valcode=%s&date=%s&json", nbuURL, code, day.Format("20060102")))
	if err != nil || len(rows) == 0 || rows[0].Rate <= 0 {
		return 0, false
	}
	return rows[0].Rate, true
}

func toUAH(code string, rates map[string]float64) (float64, error) {
	if code == "UAH" {
		return 1, nil
	}
	rate, ok := rates[code]
	if !ok {
		return 0, fmt.Errorf("%w: %s is not published by the NBU", errUnknownCurrency, code)
	}
	if rate <= 0 {
		// Нуль дав би +Inf — падаємо гучно, а не віддаємо моделі безглузде
		// число, яке вона впевнено покаже як факт.
		return 0, fmt.Errorf("%w: %s has non-positive rate %v", errUpstream, code, rate)
	}
	return rate, nil
}

// --- Агент --------------------------------------------------------------

// Опис інструмента — те, за чим модель вирішує, викликати чи ні. Вона не
// читає ваш код; вона читає цей рядок.
const toolDescription = `Returns the official NBU exchange rate between two ISO 4217 currencies,
optionally with recent daily history. Call this for ANY question about a rate or conversion.
Never state a rate from memory: rates change daily. If the user names a currency you cannot
map to a three-letter ISO 4217 code, ask them to clarify — several currencies are called "dollar".`

const instruction = `Ти відповідаєш на питання про курси валют для українських користувачів.

- Для будь-якого питання про курс викликай get_exchange_rate. Курс із пам'яті не називай.
- Інструмент повертає джерело й дату — назви обидва: число без них не перевіриш.
- Якщо інструмент повернув помилку з конкретним кодом валюти — виправ аргументи
  й виклич його ще раз, не більше одного разу.
- Якщо й після цього помилка — скажи прямо, що курс отримати не вдалося. НЕ вигадуй число.`

func main() {
	ctx := context.Background()
	if err := adkenv.Load("."); err != nil && !errors.Is(err, adkenv.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}

	// Прямий виклик інструмента, без моделі: доводить, що логіка тестується
	// без агента, ключа й мережевих ігор із LLM.
	if len(os.Args) > 1 && os.Args[1] == "quote" {
		if err := runQuote(ctx, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	m, choice, err := buildModel(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Модель: %s", choice)

	rateTool, err := functiontool.New(functiontool.Config{
		Name:        "get_exchange_rate",
		Description: toolDescription,
	}, func(ctx agent.Context, in RateInput) (RateOutput, error) {
		// Handler отримує ВЖЕ типізований RateInput — у цьому вся суть дня:
		// системний код ніколи не бачить сирого тексту моделі.
		return Convert(ctx, in)
	})
	if err != nil {
		log.Fatalf("не вдалося створити інструмент: %v", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "currency_agent",
		Model:       m,
		Description: "Відповідає на питання про курси валют за офіційними даними НБУ.",
		Instruction: instruction,
		Tools:       []tool.Tool{rateTool},
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

func runQuote(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: go run . quote <BASE> <TARGET> [HISTORY_DAYS]")
	}
	days := 0
	if len(args) > 2 {
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("HISTORY_DAYS має бути числом: %w", err)
		}
		days = n
	}
	out, err := Convert(ctx, RateInput{Base: args[0], Target: args[1], HistoryDays: days})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

func buildModel(ctx context.Context) (model.LLM, string, error) {
	name := os.Getenv("MODEL")
	if key, ok := firstKey("GOOGLE_API_KEY", "GEMINI_API_KEY"); ok {
		if name == "" {
			name = "gemini-2.5-flash"
		}
		m, err := gemini.NewModel(ctx, name, &genai.ClientConfig{APIKey: key})
		return m, "gemini → " + name, err
	}
	if base, ok := adkenv.Key("OPENAI_BASE_URL"); ok {
		if name == "" {
			name = "mock/mock-gpt"
		}
		key, _ := adkenv.Key("OPENAI_API_KEY")
		m, err := openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{APIKey: key, BaseURL: base})
		return m, "openai-compatible → " + name, err
	}
	return nil, "", errors.New(
		"провайдера не налаштовано: GOOGLE_API_KEY у apps/.env або OPENAI_BASE_URL. " +
			"Інструмент при цьому працює й без моделі: go run . quote USD UAH 3")
}

func firstKey(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := adkenv.Key(n); ok {
			return v, true
		}
	}
	return "", false
}
