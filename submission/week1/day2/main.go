// ДЗ 2 — точка входу.
//
//	go run . quote USD UAH 5   # прямий виклик інструмента, без моделі
//	go run . demo              # три діалоги (репліки моделі скриптовані, дані живі)
//	go run . faults            # fault injection: таблиця + JSON + SVG у results/
//	go run . schema            # автогенерована схема поруч із явною
//	go run . ask               # діалог із живою моделлю (потрібен ключ), stdin
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
)

// asOf — дата прогону, яка доїжджає до артефактів.
const asOf = "28.09.2026"

func main() {
	log.SetFlags(0)

	args := os.Args[1:]
	cmd := "demo"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	offline := flag.Bool("offline", false, "фікстура замість живого НБУ")
	outDir := flag.String("out", "results", "куди писати артефакти")
	modelName := flag.String("model", "gemini-2.5-flash", "модель для команди ask")
	gateway := flag.Bool("gateway", false, "ask через локальний agentgateway")
	if err := flag.CommandLine.Parse(args); err != nil {
		log.Fatal(err)
	}
	LoadEnv()

	ctx := context.Background()
	var err error
	switch cmd {
	case "quote":
		err = runQuote(ctx, *offline, flag.Args())
	case "demo":
		err = runDemo(ctx, *offline, *outDir)
	case "faults":
		err = runFaults(*outDir)
	case "schema":
		err = runSchema()
	case "ask":
		err = runAsk(ctx, *offline, *gateway, *modelName)
	default:
		err = fmt.Errorf("невідома команда %q; доступні: quote, demo, faults, schema, ask", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// provider віддає живий НБУ або фікстуру.
func provider(offline bool) Provider {
	if offline {
		return fixture()
	}
	return &NBUProvider{CacheTTL: 5 * time.Minute}
}

// fixture — знімок курсів для офлайн-режиму.
//
// Числа взяті з живого НБУ 28.09.2026. Знімок, а не вигадка: офлайн-режим має
// показувати правдоподібні порядки величин, інакше на ньому не видно
// арифметичних помилок.
func fixture() *FixtureProvider {
	return &FixtureProvider{
		Rates: map[string]float64{
			"USD": 44.6743, "EUR": 51.122, "GBP": 58.9012, "PLN": 12.0431, "CHF": 55.2210,
		},
		Date: "2026-09-28",
		History: map[string]map[string]float64{
			"USD": {"2026-09-25": 44.6012, "2026-09-26": 44.6390, "2026-09-27": 44.6743},
			"EUR": {"2026-09-25": 51.1220, "2026-09-26": 51.2004, "2026-09-27": 51.3110},
		},
	}
}

// runQuote викликає інструмент НАПРЯМУ, без моделі.
//
// Окрема команда навмисно: вона доводить, що логіка інструмента тестується й
// працює без агента, ключа та мережевих ігор із LLM. Якщо тут число
// неправильне — винна не модель.
func runQuote(ctx context.Context, offline bool, rest []string) error {
	if len(rest) < 2 {
		return fmt.Errorf("usage: go run . quote <BASE> <TARGET> [HISTORY_DAYS]")
	}
	days := 0
	if len(rest) > 2 {
		n, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("HISTORY_DAYS має бути числом: %w", err)
		}
		days = n
	}

	raw := RateInput{Base: rest[0], Target: rest[1], HistoryDays: days}
	res := TryRepair(raw, DefaultRepairPolicy())
	for _, s := range res.Steps {
		fmt.Printf("  [%d] %s%s\n", s.Attempt, s.Action, suffix(s.Err))
	}
	if res.Err != nil {
		return res.Err
	}

	quote, err := Quote(ctx, provider(offline), res.Input)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(quote, "", "  ")
	fmt.Println(string(out))
	return nil
}

func suffix(err string) string {
	if err == "" {
		return ""
	}
	return " — " + err
}

// dialogue — один задокументований діалог.
type dialogue struct {
	Title  string
	Ask    string
	Script []fakellm.Turn
}

// runDemo проганяє три діалоги з вимоги ДЗ.
//
// **Що тут скриптоване, а що справжнє — читайте уважно.** Ключа провайдера в
// мене немає, тож РЕПЛІКИ МОДЕЛІ (які саме tool-call-и вона робить і яким
// текстом підсумовує) задані скриптом. Усе інше — справжнє: інструмент,
// валідація, помилки, цикл ремонту й самі курси, які тягнуться з живого API
// НБУ прямо під час прогону.
//
// Тобто діалог доводить не «модель здогадалася виправитись», а «коли модель
// виправляється, межа системи поводиться саме так»: перший виклик відхилено з
// конкретною помилкою, другий — прийнято, число реальне й має походження.
func runDemo(ctx context.Context, offline bool, outDir string) error {
	p := provider(offline)

	dialogues := []dialogue{
		{
			Title: "1. Коректний запит",
			Ask:   "Який сьогодні курс долара до гривні? Покажи ще й останні 3 дні.",
			Script: []fakellm.Turn{
				fakellm.CallTurn(toolName, map[string]any{
					"base": "USD", "target": "UAH", "history_days": 3,
				}),
				fakellm.TextTurn("Курс USD/UAH за офіційними даними НБУ (джерело: api, дата у полі as_of). " +
					"Історію за останні дні наведено в полі history — кожна точка має власне походження."),
			},
		},
		{
			Title: "2. Помилкова валюта — модель самовиправляється",
			Ask:   "Скільки коштує USDD у гривнях?",
			Script: []fakellm.Turn{
				// Перший виклик — із неіснуючим кодом. Інструмент відхилить.
				fakellm.CallTurn(toolName, map[string]any{"base": "USDD", "target": "UAH"}),
				// Модель читає текст помилки («did you mean USD?») і виправляється.
				fakellm.CallTurn(toolName, map[string]any{"base": "USD", "target": "UAH"}),
				fakellm.TextTurn("Коду USDD не існує — ISO 4217 для долара США це USD. " +
					"Наводжу курс USD/UAH за даними НБУ із зазначенням джерела й дати."),
			},
		},
		{
			Title: "3. Дві конвертації в одному повідомленні",
			Ask:   "Порівняй курс євро та фунта до гривні.",
			Script: []fakellm.Turn{
				fakellm.CallTurn(toolName, map[string]any{"base": "EUR", "target": "UAH"}),
				fakellm.CallTurn(toolName, map[string]any{"base": "GBP", "target": "UAH"}),
				fakellm.TextTurn("Обидва курси отримано окремими викликами інструмента — " +
					"кожен зі своїм джерелом і датою, тому їх можна порівнювати."),
			},
		},
		{
			Title: "4. Fail path: джерело недоступне — чесна помилка замість числа",
			Ask:   "Курс швейцарського франка до гривні?",
			Script: []fakellm.Turn{
				fakellm.CallTurn(toolName, map[string]any{"base": "CHF", "target": "UAH"}),
				fakellm.TextTurn("Не вдалося отримати курс: джерело курсів недоступне. " +
					"Назвати число з пам'яті я не можу — воно було б вигаданим."),
			},
		},
	}

	var b strings.Builder
	for i, d := range dialogues {
		use := p
		if i == 3 {
			// Четвертий діалог навмисно б'є в мертвий апстрім.
			use = &FixtureProvider{Err: errUpstreamFixture}
		}

		m := fakellm.New("scripted", d.Script...)
		a, toolLog, err := NewLoggedAgent(m, use)
		if err != nil {
			return err
		}

		fmt.Fprintf(&b, "══════════════════════════════════════════════════════════\n%s\n\n", d.Title)
		fmt.Fprintf(&b, "Користувач: %s\n\n", d.Ask)

		res, runErr := labrun.Run(ctx, a, d.Ask)
		for n, call := range toolLog.Calls() {
			fmt.Fprintf(&b, "  → tool call %d: %s(base=%q, target=%q, history_days=%d)\n",
				n+1, toolName, call.Input.Base, call.Input.Target, call.Input.HistoryDays)
			if call.Err != "" {
				fmt.Fprintf(&b, "  ← ВІДХИЛЕНО: %s\n", call.Err)
				continue
			}
			out, _ := json.Marshal(call.Quote)
			fmt.Fprintf(&b, "  ← OK: %s\n", string(out))
		}
		if runErr != nil {
			fmt.Fprintf(&b, "\n  прогін завершився помилкою: %v\n", runErr)
		}
		fmt.Fprintf(&b, "\nАгент: %s\n\n", strings.Join(res.Texts(), "\n"))
	}

	fmt.Print(b.String())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "dialogues.txt"), []byte(b.String()), 0o644)
}

// faultScenario — один навмисно зіпсований виклик моделі.
type faultScenario struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	Raw  string `json:"raw_arguments"`
}

// faultScenarios — навмисно невалідні виклики.
//
// ++ Advanced вимагає ≥3 сценарії; тут їх вісім, і вони покривають ТРИ різні
// рівні межі, які легко сплутати:
//
//	рівень JSON   — текст не розбирається взагалі;
//	рівень схеми  — JSON валідний, але не той тип / не те поле;
//	рівень домену — і JSON, і типи правильні, а виклик усе одно безглуздий.
//
// Плутати їх дорого: помилка рівня домену, подана як «bad request», відправляє
// модель виправляти тип замість значення.
var faultScenarios = []faultScenario{
	{"malformed-json", "рівень JSON: текст обірваний", `{"base": "USD", "target":`},
	{"wrong-type", "рівень схеми: число там, де рядок", `{"base": 840, "target": "UAH"}`},
	{"unknown-field", "рівень схеми: поле, якого немає в контракті", `{"base":"USD","target":"UAH","currency":"EUR"}`},
	{"missing-field", "рівень схеми: обов'язкове поле відсутнє", `{"base": "USD"}`},
	{"bad-code-shape", "рівень домену: валідний JSON, невалідний код", `{"base": "US1", "target": "UAH"}`},
	{"unknown-currency", "рівень домену: форма правильна, валюти немає", `{"base": "XQZ", "target": "UAH"}`},
	{"lowercase", "нормалізація: регістр — не помилка", `{"base": "usd", "target": "uah"}`},
	{"alias", "ремонт: однозначний синонім валюти", `{"base": "євро", "target": "UAH"}`},
	{"ambiguous-alias", "домен: «долар» — їх багато, ремонт НЕБЕЗПЕЧНИЙ", `{"base": "долар", "target": "UAH"}`},
	{"range-overflow", "ремонт: діапазон підрізається до стелі", `{"base":"USD","target":"UAH","history_days":365}`},
	{"happy-path", "контроль: валідний виклик мусить пройти", `{"base":"USD","target":"UAH","history_days":2}`},
}

// faultOutcome — що межа зробила зі сценарієм.
type faultOutcome struct {
	Scenario faultScenario `json:"scenario"`
	Stage    string        `json:"stage"`
	Verdict  string        `json:"verdict"`
	Message  string        `json:"message"`
	Steps    []RepairStep  `json:"steps,omitempty"`
}

// runFaults проганяє сценарії через ту саму межу, що й справжні виклики.
func runFaults(outDir string) error {
	outcomes := make([]faultOutcome, 0, len(faultScenarios))
	// Фікстура, а не живий НБУ: fault injection має давати ОДНАКОВИЙ результат
	// у будь-який день і без мережі. Прогін проти живого джерела, який у
	// вихідні дає інший набір вердиктів, — це не тест, а лотерея.
	p := fixture()

	for _, sc := range faultScenarios {
		out := faultOutcome{Scenario: sc}

		in, err := DecodeArgs([]byte(sc.Raw))
		if err != nil {
			out.Stage, out.Verdict, out.Message = "decode", "rejected", err.Error()
			outcomes = append(outcomes, out)
			continue
		}

		res := TryRepair(in, DefaultRepairPolicy())
		out.Steps = res.Steps
		if res.Err != nil {
			out.Stage, out.Verdict, out.Message = "validate", "rejected", res.Err.Error()
			outcomes = append(outcomes, out)
			continue
		}

		// Валідація перевірила ФОРМУ. Чи існує така валюта — знає лише
		// джерело, і це окремий, ТРЕТІЙ рівень межі. Без цього кроку сценарій
		// XQZ виглядав би як «межа пропустила неіснуючу валюту», хоча насправді
		// її ловить наступний рівень — і ловить із підказкою для моделі.
		_, lookupErr := Quote(context.Background(), p, res.Input)
		switch {
		case lookupErr != nil:
			out.Stage, out.Verdict, out.Message = "lookup", "rejected", lookupErr.Error()
		case res.Repaired:
			out.Stage, out.Verdict = "repair", "repaired"
			out.Message = fmt.Sprintf("%s → base=%s target=%s history_days=%d",
				summarize(res.Steps), res.Input.Base, res.Input.Target, res.Input.HistoryDays)
		default:
			out.Stage, out.Verdict = "lookup", "accepted"
			out.Message = fmt.Sprintf("пройшов усі рівні: base=%s target=%s history_days=%d",
				res.Input.Base, res.Input.Target, res.Input.HistoryDays)
		}
		outcomes = append(outcomes, out)
	}

	fmt.Printf("| Сценарій | Рівень межі | Вердикт | Що побачить модель |\n|---|---|---|---|\n")
	for _, o := range outcomes {
		fmt.Printf("| `%s` | %s | **%s** | %s |\n",
			o.Scenario.Name, o.Scenario.Why, o.Verdict, truncate(o.Message, 110))
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outDir, "faults.json"), outcomes); err != nil {
		return err
	}
	svg := RenderFaultsSVG(outcomes, asOf)
	if err := os.WriteFile(filepath.Join(outDir, "faults.svg"), []byte(svg), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nАртефакти: %s/faults.json, %s/faults.svg\n", outDir, outDir)
	return nil
}

// runSchema друкує обидві схеми поруч.
func runSchema() error {
	inferred, err := InferredInputSchemaJSON()
	if err != nil {
		return err
	}
	explicit, err := json.MarshalIndent(StrictInputSchema(), "", "  ")
	if err != nil {
		return err
	}
	outSchema, err := InferredOutputSchemaJSON()
	if err != nil {
		return err
	}

	fmt.Println("── ВХІД: схема, ВИВЕДЕНА з RateInput (жодного рядка JSON Schema від руки)")
	fmt.Println(inferred)
	fmt.Println("\n── ВХІД: ЯВНА functiontool.Config.InputSchema (обмеження, яких немає в системі типів Go)")
	fmt.Println(string(explicit))
	fmt.Println("\n── ВИХІД: схема, ВИВЕДЕНА з RateQuote — три рівні вкладеності, масив, enum")
	fmt.Println(outSchema)
	return nil
}

// runAsk — діалог із живою моделлю. Потребує ключа.
func runAsk(ctx context.Context, offline, gateway bool, name string) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	question := strings.TrimSpace(string(raw))
	if question == "" {
		return fmt.Errorf("порожній запит: echo \"...\" | go run . ask")
	}

	var m model.LLM
	m, err = BuildModel(ctx, gateway, name)
	if err != nil {
		return err
	}

	a, toolLog, err := NewLoggedAgent(m, provider(offline))
	if err != nil {
		return err
	}
	res, err := labrun.Run(ctx, a, question)
	if err != nil {
		return err
	}
	for n, call := range toolLog.Calls() {
		out, _ := json.Marshal(call.Quote)
		fmt.Printf("  → tool call %d: base=%q target=%q days=%d\n  ← %s%s\n",
			n+1, call.Input.Base, call.Input.Target, call.Input.HistoryDays, string(out), suffix(call.Err))
	}
	fmt.Printf("\nАгент: %s\n", strings.Join(res.Texts(), "\n"))
	return nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
