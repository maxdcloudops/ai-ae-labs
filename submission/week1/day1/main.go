// ДЗ 1 — точка входу.
//
// Дві команди:
//
//	go run . bench      # бенчмарк: таблиця + JSON + SVG у results/
//	go run . console    # один діалог з агентом (читає stdin)
//	go run . faults     # fault injection: справжні 500-ки, ретрай з backoff
//
// Обидві працюють БЕЗ ключа:
//
//	go run . bench -offline            # скриптована модель, нуль пререквізитів
//	go run . bench                     # через локальний agentgateway → ./mockresponses
//
// Із ключем провайдера це той самий код на живих моделях — див. README, §Як
// запустити.
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
	"strings"
	"time"
)

// benchAsOf — дата прогону. Живе в коді, а не в коментарі, бо доїжджає до
// кожного рядка таблиці й до графіка: alias, ціна й доступність моделей
// змінюються швидше за дедлайн ДЗ.
const benchAsOf = "27.09.2026"

// gatewayConfigs — конфігурації, які їдуть через локальний agentgateway.
//
// Бекенд за всіма ОДИН І ТОЙ САМИЙ (./mockresponses), і це не недогляд, а умова
// експерименту: коли модель однакова, різниця в рядку вартості може походити
// ТІЛЬКИ з прайсу й з кількості токенів. Саме так перевіряється, що арифметика
// харнесу зійшлася з арифметикою гейтвея — двох незалежних обчислювачів.
//
// Останній рядок (mock-sloppy) має ТОЙ САМИЙ прайс, що baseline, і навмисно
// іншу поведінку: він ігнорує контракт відповіді. Без нього колонка «формат»
// була б стовпчиком однакових четвірок, тобто не вимірювала б нічого.
//
// Ставки мають збігатися з inline-каталогом гейтвея
// (demo/1_ai-gateway/config/agentgateway.yaml, блок modelCatalog): це два
// НЕЗАЛЕЖНІ обчислювачі однієї величини, і розбіжність між ними — сигнал, а не
// шум.
var gatewayConfigs = []Config{
	{
		Label:            "mock-gpt (baseline, через gateway)",
		Model:            "mock/mock-gpt",
		Backend:          BackendGateway,
		InputUSDPerMTok:  0.15,
		OutputUSDPerMTok: 0.60,
		AsOf:             benchAsOf,
	},
	{
		Label:            "mock-gpt-mini (дешевий прайс)",
		Model:            "mock/mock-gpt-mini",
		Backend:          BackendGateway,
		InputUSDPerMTok:  0.05,
		OutputUSDPerMTok: 0.20,
		AsOf:             benchAsOf,
	},
	{
		Label:            "mock-gpt-pro (премійний прайс)",
		Model:            "mock/mock-gpt-pro",
		Backend:          BackendGateway,
		InputUSDPerMTok:  3.00,
		OutputUSDPerMTok: 15.00,
		AsOf:             benchAsOf,
	},
	{
		// Той самий прайс, що в baseline, і навмисно ІНША поведінка: цей
		// маршрут повертає вільний текст замість контракту «Ранок/День/
		// Вечір/Бюджет». Рядок існує, щоб колонка «формат» мала розкид:
		// стовпчик, у якому всі значення однакові, не вимірює нічого.
		Label:            "mock-sloppy (той самий прайс, ігнорує контракт)",
		Model:            "mock/mock-sloppy",
		Backend:          BackendGateway,
		InputUSDPerMTok:  0.15,
		OutputUSDPerMTok: 0.60,
		AsOf:             benchAsOf,
	},
}

// liveConfigs — той самий бенчмарк на живих провайдерах.
//
// Дві конфігурації однієї моделі — це шлях, який ДЗ дозволяє при одному ключі:
// різний reasoning effort дає різні вихідні токени, а отже й різну вартість
// однієї задачі. Ставки — плейсхолдери: звіряйте з прайс-листом на дату свого
// прогону, інакше колонка «$» перетворюється на художній твір.
var liveConfigs = []Config{
	{
		Label:            "gemini-2.5-flash",
		Model:            "gemini-2.5-flash",
		Backend:          BackendGemini,
		InputUSDPerMTok:  0.30,
		OutputUSDPerMTok: 2.50,
		AsOf:             benchAsOf,
	},
	{
		Label:            "gemini-2.5-pro",
		Model:            "gemini-2.5-pro",
		Backend:          BackendGemini,
		InputUSDPerMTok:  1.25,
		OutputUSDPerMTok: 10.00,
		AsOf:             benchAsOf,
	},
}

// benchPrompts — чотири сценарії, і кожен перевіряє іншу властивість.
//
// Перші два — це робота агента (план і план під дощем). Третій — доменна межа:
// правильна відповідь тут «ні», і ExpectRefusal каже харнесу не рахувати це
// невдачею. Четвертий — context-stress: усі потрібні дані передані в промпті,
// КРІМ одного, і чесна відповідь — назвати, чого саме не вистачає.
var benchPrompts = []Prompt{
	{
		Name:         "saturday-plan",
		Text:         "Склади план на суботу у Львові для двох людей, бюджет до 1000 грн.",
		ExpectFormat: true,
	},
	{
		Name:         "rain-fallback",
		Text:         "А якщо в суботу цілий день дощ? Переплануй.",
		ExpectFormat: true,
	},
	{
		Name:          "out-of-domain",
		Text:          "Порахуй мені податок на доходи ФОП 3 групи за квартал і склади декларацію.",
		ExpectRefusal: true,
	},
	{
		Name: "context-stress",
		Text: `Ось наш внутрішній довідник локацій (інших даних у тебе немає):
1. Кава "Світло" — Краківська 12, пн–сб 08:00–20:00, кава 60–90 грн.
2. Галерея "Дзиґа" — Вірменська 35, сб 11:00–19:00, вхід 120 грн.
3. Пекарня "Хлібна" — Дорошенка 4, щодня 07:00–19:00, сніданок 150 грн.
4. Оглядова "Ратуша" — площа Ринок 1, сб 10:00–18:00, вхід 100 грн.
5. Бар "Підвал" — Староєврейська 22, чт–сб 18:00–02:00, коктейль 220 грн.

Питання: у котрій годині в НЕДІЛЮ відкривається галерея "Дзиґа" і скільки коштує вхід у неділю?`,
	},
}

func main() {
	log.SetFlags(0)

	// Підкоманду знімаємо ДО flag.Parse, а не після.
	//
	// Це не косметика: flag.Parse зупиняється на першому аргументі, який не є
	// прапорцем, і все після нього лишає в flag.Args(). Тобто при розборі
	// «спочатку прапорці, потім підкоманда» рядок `bench -n 5` тихо
	// ігнорував -n і друкував таблицю на дефолтних трьох прогонах, чесно
	// підписану «n=3». Найгірший різновид помилки: нічого не падає, а число в
	// звіті не те, яке ви просили.
	args := os.Args[1:]
	cmd := "bench"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	offline := flag.Bool("offline", false, "скриптована модель замість провайдера: нуль ключів, нуль мережі")
	live := flag.Bool("live", false, "живі вендорські провайдери замість локального гейтвея")
	n := flag.Int("n", 3, "прогонів на пару «конфігурація × промпт»")
	parallel := flag.Int("parallel", 3, "стеля одночасних запитів")
	composeDir := flag.String("gateway-dir", "../../../demo/1_ai-gateway",
		"тека docker compose гейтвея — звідти читається access-log для другого виміру вартості")
	outDir := flag.String("out", "results", "куди писати bench.json / bench.md / bench.svg")
	if err := flag.CommandLine.Parse(args); err != nil {
		log.Fatal(err)
	}

	if err := LoadEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}

	ctx := context.Background()
	var err error
	switch cmd {
	case "bench":
		err = runBench(ctx, *offline, *live, *n, *parallel, *composeDir, *outDir)
	case "console":
		err = runConsole(ctx, *offline, *live)
	case "faults":
		err = runFaults(ctx, *composeDir)
	default:
		err = fmt.Errorf("невідома команда %q; доступні: bench, console, faults", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// runBench проганяє бенчмарк і пише три артефакти: JSON, markdown і SVG.
func runBench(ctx context.Context, offline, live bool, n, parallel int, composeDir, outDir string) error {
	cfgs := gatewayConfigs
	mode := "локальний ланцюг agentgateway → ./mockresponses, без ключів провайдера. Латентність і токени — справжні для ЛАНЦЮГА; текст відповіді — фейковий, тож «якість» тут не вимірюється."
	if live {
		cfgs = liveConfigs
		mode = "живі вендорські провайдери: латентність і токени — від провайдера."
	}
	if offline {
		mode = "офлайн (скриптована модель): латентність НЕ вимірює провайдера; токени й вартість — з usage скриптованої моделі."
	}

	start := time.Now()
	samples, err := RunAll(ctx, cfgs, benchPrompts, n, parallel, DefaultRetryPolicy(), ADKRunner(offline, live))
	if err != nil {
		return err
	}
	sums := Summarize(cfgs, samples)

	// Другий вимір вартості. Помилка тут не фатальна: гейтвей —
	// необов'язкова частина ДЗ, і таблиця без його колонки лишається таблицею.
	if !offline && !live {
		stats, gwErr := FetchGatewayStats(ctx, composeDir, start)
		if gwErr != nil {
			fmt.Fprintf(os.Stderr, "гейтвей не опитано (%v): колонка «$ гейтвей» буде порожня\n", gwErr)
		} else {
			sums = JoinGatewayCost(sums, stats)
		}
	}

	table := Markdown(sums)
	fmt.Print(table)
	fmt.Printf("\nРежим: %s\nПрогонів: %d на пару, конфігурацій: %d, промптів: %d, стеля паралелізму: %d\n",
		mode, n, len(cfgs), len(benchPrompts), parallel)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("створити %s: %w", outDir, err)
	}

	payload := struct {
		AsOf      string    `json:"as_of"`
		RanAt     time.Time `json:"ran_at"`
		Mode      string    `json:"mode"`
		N         int       `json:"n_per_pair"`
		Parallel  int       `json:"parallel"`
		Configs   []Config  `json:"configs"`
		Prompts   []Prompt  `json:"prompts"`
		Summaries []Summary `json:"summaries"`
		Samples   []Sample  `json:"samples"`
	}{benchAsOf, start, mode, n, parallel, cfgs, benchPrompts, sums, samples}

	if err := writeJSON(filepath.Join(outDir, "bench.json"), payload); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "bench.md"), []byte(table), 0o644); err != nil {
		return err
	}
	svg := RenderSVG(ChartInput{
		Title:     "Cross-Model Benchmark Harness — ДЗ 1",
		Mode:      mode,
		AsOf:      benchAsOf,
		Summaries: sums,
		Note:      "Бекенд за всіма рядками один і той самий: різниця у вартості походить із прайсу й кількості токенів, а не з моделі.",
	})
	if err := os.WriteFile(filepath.Join(outDir, "bench.svg"), []byte(svg), 0o644); err != nil {
		return err
	}
	fmt.Printf("Артефакти: %s/bench.json, %s/bench.md, %s/bench.svg\n", outDir, outDir, outDir)
	return nil
}

// runConsole проганяє один діалог: читає запит зі stdin, друкує відповідь.
//
// Окрема команда, а не launcher ADK: для README потрібні саме ТРАНСКРИПТИ, і
// «прочитати stdin → надрукувати відповідь» дає їх одним пайпом, без
// інтерактиву та без зайвого шуму в лозі.
func runConsole(ctx context.Context, offline, live bool) error {
	cfgs := gatewayConfigs
	if live {
		cfgs = liveConfigs
	}
	cfg := cfgs[0]

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("читання stdin: %w", err)
	}
	question := strings.TrimSpace(string(raw))
	if question == "" {
		return fmt.Errorf("порожній запит: передайте текст через stdin, напр. echo \"…\" | go run . console")
	}

	run := ADKRunner(offline, live)
	obs, err := run(ctx, cfg, Prompt{Name: "console", Text: question})
	if err != nil {
		return err
	}

	fmt.Printf("── конфігурація: %s (%s)\n", cfg.Label, cfg.Model)
	fmt.Printf("── запит:\n%s\n\n── відповідь:\n%s\n\n", question, obs.Answer)
	fmt.Printf("── межа домену спрацювала: %t · викликів моделі: %d",
		obs.Refused, len(obs.Calls))
	var in, out int
	for _, c := range obs.Calls {
		in += c.InTokens
		out += c.OutTokens
	}
	fmt.Printf(" · токени in/out: %d/%d · вартість: $%.8f\n", in, out, cfg.CostUSD(in, out))
	return nil
}

// failConfig — маршрут, який бекенд ЗАВЖДИ віддає з HTTP 500.
//
// Не окремий рядок бенчмарку, а окрема команда: у таблиці «модель × вартість»
// рядок, який ніколи не відповідає, не має сенсу, а от довести, що ретрай
// працює на справжній 5xx, — має.
var failConfig = Config{
	Label:            "mock-fail (бекенд завжди 500)",
	Model:            "mock/mock-fail",
	Backend:          BackendGateway,
	InputUSDPerMTok:  0.15,
	OutputUSDPerMTok: 0.60,
	AsOf:             benchAsOf,
}

// runFaults доводить, що ретрай із backoff працює на СПРАВЖНІЙ 5xx.
//
// Різниця між цією командою й юніт-тестом ретраїв принципова: тест підставляє
// помилку в клієнті, тобто перевіряє лише власну логіку. Тут 500 приходить із
// бекенда через гейтвей, тож перевіряється весь ланцюг — включно з тим, що
// гейтвей записав кожну спробу в access-log окремим рядком. Чотири рядки зі
// статусом 500 у логу — це і є доказ, що ретраїв було чотири, а не один.
func runFaults(ctx context.Context, composeDir string) error {
	policy := DefaultRetryPolicy()
	run := ADKRunner(false, false)

	var timeline []string
	attempt := 0
	start := time.Now()

	attempts, err := Retry(ctx, policy, func() error {
		attempt++
		t0 := time.Now()
		_, runErr := run(ctx, failConfig, benchPrompts[0])
		status := "ok"
		if runErr != nil {
			status = "помилка"
		}
		timeline = append(timeline, fmt.Sprintf("  спроба %d за %5s → %s: transient=%t",
			attempt, time.Since(t0).Round(time.Millisecond), status, IsTransient(runErr)))
		return runErr
	})
	total := time.Since(start)

	fmt.Printf("── fault injection: %s (%s)\n", failConfig.Label, failConfig.Model)
	for _, line := range timeline {
		fmt.Println(line)
	}
	fmt.Printf("── спроб: %d (стеля %d) · загальний час із паузами backoff: %s\n",
		attempts, policy.MaxAttempts, total.Round(time.Millisecond))

	if err == nil {
		return fmt.Errorf("бекенд mock-fail відповів успішно — це означає, що запит пішов не туди")
	}
	fmt.Printf("── фінальна помилка (її й побачить користувач, без вигаданої відповіді):\n   %v\n", err)

	// Другий доказ — з боку межі системи, а не з боку агента.
	if stats, gwErr := FetchGatewayStats(ctx, composeDir, start); gwErr == nil {
		if stat, ok := stats["mock-fail"]; ok {
			fmt.Printf("── гейтвей побачив: запитів %d, статуси %v, вартість $%.8f\n",
				stat.Requests, stat.Statuses, stat.CostUSD)
		}
	} else {
		fmt.Fprintf(os.Stderr, "гейтвей не опитано: %v\n", gwErr)
	}
	return nil
}

// writeJSON пише артефакт із відступами: JSON бенчмарку читають люди в diff,
// а не лише парсери.
func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("створити %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("записати %s: %w", path, err)
	}
	return nil
}
