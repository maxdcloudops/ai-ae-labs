package main

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/improve"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/ledgerkb"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/offline"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/sandbox"
)

// baseStrategy is the "zero" strategy: it says nothing about reading documents
// or quoting figures, so the worker answers from snippets. The whole
// improvement curve starts here.
const baseStrategy = `# Дослідницька стратегія

Відповідай українською, коротко.
Спочатку шукай у базі знань LEDGERWORKS.
`

// goodStrategy has the two levers the offline worker reacts to.
const goodStrategy = baseStrategy + `
Для кожного питання читай найкращий документ інструментом kb_read, а не відповідай зі сніпетів.
Цитуй кожну цифру (every figure) з прочитаного документа, а не лише перше речення.
`

func testWorkspace(t *testing.T, strategy string) *sandbox.Sandbox {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, sandbox.StrategyFile), []byte(strategy), 0o644); err != nil {
		t.Fatal(err)
	}
	box, err := openSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// TestPipelineRunsEveryStage — п.1: кожен крок зшивки має бути в графі й
// відпрацювати. Тест дивиться на журнал стадій, а не на текст відповіді:
// відповідь залежить від моделі, а топологія — ні.
func TestPipelineRunsEveryStage(t *testing.T) {
	t.Parallel()
	var stages []string
	p := Pipeline{
		KB: ledgerkb.New(), Model: offline.Worker{}, Strategy: goodStrategy,
		AutoApprove: true,
		Log:         func(s string) { stages = append(stages, s) },
	}
	ag, err := p.Agent()
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := runOnce(context.Background(), ag,
		"An incident is classified SEV1. What are the paging time and the update cadence?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("фінальний звіт порожній")
	}
	joined := strings.Join(stages, " | ")
	for _, want := range []string{"plan:", "kb_branch:", "web_branch:", "draft_report:", "critic_loop:", "final_report:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("стадія %q не відпрацювала:\n%s", want, joined)
		}
	}
	if !strings.Contains(text, "Джерела:") {
		t.Errorf("у звіті немає джерел:\n%s", text)
	}
}

// TestBothBranchesReachTheJoin — fan-in барʼєр: чернетка не має права
// будуватись на одній гілці.
func TestBothBranchesReachTheJoin(t *testing.T) {
	t.Parallel()
	var stages []string
	p := Pipeline{
		KB: ledgerkb.New(), Model: offline.Worker{}, Strategy: goodStrategy,
		AutoApprove: true, SkipCritics: true,
		Log: func(s string) { stages = append(stages, s) },
	}
	ag, err := p.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runOnce(context.Background(), ag, "chargeback evidence deadline and fee"); err != nil {
		t.Fatal(err)
	}
	var kb, web bool
	for _, s := range stages {
		kb = kb || strings.HasPrefix(s, "kb_branch:")
		web = web || strings.HasPrefix(s, "web_branch:")
	}
	if !kb || !web {
		t.Fatalf("до join дійшли не обидві гілки: kb=%t web=%t (%v)", kb, web, stages)
	}
}

// TestHITLRefusalsAreNamed — три різні відповіді людини, три різні записи, і
// жодна з них, крім approve, не публікує звіт.
func TestHITLRefusalsAreNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		answer, wantStatus string
		published          bool
	}{
		{"approve", "approved", true},
		{"deny", "denied", false},
		{"ок?", "invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.answer, func(t *testing.T) {
			t.Parallel()
			p := Pipeline{KB: ledgerkb.New(), Model: offline.Worker{}, Strategy: goodStrategy}
			d := Draft{Plan: Plan{Query: "q", Strategy: goodStrategy}, Text: "звіт із цифрою 7 днів", Sources: []string{"REF-001"}}
			// Гейт перевіряємо напряму: пауза в графі — окремий тест, а тут
			// важливо, що відповідь людини правильно класифікується.
			a, err := p.approveFromReply(d, tt.answer)
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != tt.wantStatus {
				t.Errorf("статус = %q, want %q", a.Status, tt.wantStatus)
			}
			final, err := p.finalFn(nil, a)
			if err != nil {
				t.Fatal(err)
			}
			if published := !strings.HasPrefix(final, "НЕ опубліковано"); published != tt.published {
				t.Errorf("опубліковано = %t, want %t: %q", published, tt.published, final)
			}
			if !tt.published && !strings.Contains(final, a.Reason) {
				t.Errorf("відмова не називає причину: %q", final)
			}
		})
	}
}

// TestPipelineParksWithoutAnAnswer — без AutoApprove ран мусить стати на паузу,
// і фінальний вузол не має відпрацювати. Дія без схвалення не відбувається.
func TestPipelineParksWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	var stages []string
	p := Pipeline{
		KB: ledgerkb.New(), Model: offline.Worker{}, Strategy: goodStrategy,
		SkipCritics: true,
		Log:         func(s string) { stages = append(stages, s) },
	}
	ag, err := p.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runOnce(context.Background(), ag, "retention of transaction records"); err != nil {
		t.Fatal(err)
	}
	for _, s := range stages {
		if strings.HasPrefix(s, "final_report:") {
			t.Errorf("фінальний вузол відпрацював без схвалення: %v", stages)
		}
	}
}

// TestCriticVerdictValidation — несумісний вердикт не «якось інтерпретується».
func TestCriticVerdictValidation(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, raw, role, want string }{
		{"проза", "виглядає добре", "sources", "JSON"},
		{"підміна ролі", `{"critic":"coverage","passed":true}`, "sources", "роль"},
		{"провал без коментарів", `{"critic":"sources","passed":false}`, "sources", "коментар"},
		{"успіх із коментарями", `{"critic":"sources","passed":true,"comments":["x"]}`, "sources", "comments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseReportVerdict(tt.raw, tt.role); err == nil {
				t.Fatal("невалідний вердикт прийнято")
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want містити %q", err, tt.want)
			}
		})
	}
	if v, err := parseReportVerdict(`{"critic":"sources","passed":true}`, "sources"); err != nil || !v.Passed {
		t.Errorf("валідний вердикт відкинуто: %+v %v", v, err)
	}
}

// TestCriticRulesCatchRealProblems — правила критиків мусять ловити те, що
// написано в їхньому фокусі, і пропускати нормальну чернетку.
func TestCriticRulesCatchRealProblems(t *testing.T) {
	t.Parallel()
	ok := Draft{Text: "Еvidence due within 7 calendar days, fee 15 EUR.", Sources: []string{"REF-002"}}
	if c := ruleSources(ok); len(c) != 0 {
		t.Errorf("sources чіпляється до нормальної чернетки: %v", c)
	}
	if c := ruleCoverage(ok); len(c) != 0 {
		t.Errorf("coverage чіпляється до нормальної чернетки: %v", c)
	}
	if c := ruleSources(Draft{Text: ok.Text}); len(c) == 0 {
		t.Error("sources пропустив чернетку без джерел")
	}
	if c := ruleCoverage(Draft{Text: "Загалом усе добре і зрозуміло, деталі в документі.", Sources: []string{"x"}}); len(c) == 0 {
		t.Error("coverage пропустив відповідь без жодної цифри")
	}
	if c := ruleCoverage(Draft{Text: "коротко", Sources: []string{"x"}}); len(c) == 0 {
		t.Error("coverage пропустив відповідь із семи символів")
	}
}

// TestMetricIsPureAndSourceless — принцип (в): Score це чиста функція без
// виклику моделі. Той самий текст — та сама оцінка, і жодного провайдера.
func TestMetricIsPureAndSourceless(t *testing.T) {
	t.Parallel()
	cases := ledgerkb.Dev()
	answer := "A second approval by a compliance officer is required; processing time is 5 business days; the window is 120 days."
	f1, t1 := ledgerkb.Score(answer, cases[0].Facts)
	f2, t2 := ledgerkb.Score(answer, cases[0].Facts)
	if f1 != f2 || t1 != t2 {
		t.Fatalf("Score не детермінований: (%d,%d) vs (%d,%d)", f1, t1, f2, t2)
	}
	if f1 != t1 {
		t.Errorf("повна відповідь дала %d з %d: %v", f1, t1, ledgerkb.Missing(answer, cases[0].Facts))
	}
	if f, _ := ledgerkb.Score("нічого по суті", cases[0].Facts); f != 0 {
		t.Errorf("порожня по суті відповідь дала %d фактів", f)
	}
}

// TestLeakCheckRefusesACheatingStrategy — стратегія, яка цитує очікувані
// відповіді, набрала б 1.0 і нічого не довела.
func TestLeakCheckRefusesACheatingStrategy(t *testing.T) {
	t.Parallel()
	sc := scorerFor(ledgerkb.New(), offline.Worker{})
	if leaks := sc.LeakCheck(baseStrategy); len(leaks) != 0 {
		t.Errorf("чиста стратегія позначена як витік: %v", leaks)
	}
	cheat := baseStrategy + "\nЗавжди відповідай: 120 days, 7 calendar days, 15 EUR.\n"
	if leaks := sc.LeakCheck(cheat); len(leaks) == 0 {
		t.Error("стратегія з відповідями з eval-набору не визнана витоком")
	}
}

// TestImprovementCurveIsReal — найважливіший тест капстона: метрика зростає
// тому, що агент ІНАКШЕ працює, а не тому, що так написано в скрипті.
//
// База відповідає зі сніпетів і не знаходить нічого; стратегія з двома
// важелями читає документ і цитує цифри.
func TestImprovementCurveIsReal(t *testing.T) {
	sc := scorerFor(ledgerkb.New(), offline.Worker{})
	ctx := context.Background()
	before, err := sc.Score(ctx, baseStrategy)
	if err != nil {
		t.Fatal(err)
	}
	after, err := sc.Score(ctx, goodStrategy)
	if err != nil {
		t.Fatal(err)
	}
	if before.Dev != 0 {
		t.Errorf("база dev = %.4f, want 0: інакше крива починається не з нуля", before.Dev)
	}
	if after.Dev <= before.Dev {
		t.Fatalf("dev не зріс: %.4f → %.4f", before.Dev, after.Dev)
	}
	if after.Heldout <= before.Heldout {
		t.Errorf("heldout не зріс: %.4f → %.4f — покращення лише на dev це ще не покращення",
			before.Heldout, after.Heldout)
	}
	if after.Total != before.Total {
		t.Errorf("кількість очікуваних фактів змінилась (%d → %d) — метрика не порівнювана",
			before.Total, after.Total)
	}
}

// TestOfflineProposerNeverRepeatsACandidate — повтор тієї самої стратегії
// купує ідентичний вимір за цілий експеримент бюджету.
func TestOfflineProposerNeverRepeatsACandidate(t *testing.T) {
	t.Parallel()
	p := &OfflineProposer{}
	seen := map[string]bool{}
	current := baseStrategy
	for i := 0; i < len(rungs)+2; i++ {
		prop, err := p.Propose(context.Background(), current, improve.Measurement{}, nil)
		if err != nil {
			if i < 2 {
				t.Fatalf("пропонент здався на кроці %d: %v", i+1, err)
			}
			return // честная остановка — это нормальный конец лестницы
		}
		if seen[prop.Content] {
			t.Fatalf("кандидат повторився на кроці %d", i+1)
		}
		seen[prop.Content] = true
		if !strings.HasPrefix(prop.Content, strings.TrimRight(current, "\n")) {
			t.Error("кандидат не є поточною стратегією плюс рядки — пропонент переписує весь файл")
		}
		if prop.Summary == "" {
			t.Error("пропозиція без diff_summary: журнал стане нечитабельним")
		}
	}
}

// TestEndToEndImprovementWritesTheLog — бюджет 5 означає 5 записів у
// experiments.log, і кожен — JSON-line з полями, які дивиться рецензент.
func TestEndToEndImprovementWritesTheLog(t *testing.T) {
	box := testWorkspace(t, baseStrategy)
	var out strings.Builder
	if err := cmdImprove(context.Background(), &out, box, ledgerkb.New(), offline.Worker{}, 5, false); err != nil {
		t.Fatalf("cmdImprove: %v\n%s", err, out.String())
	}
	data, err := os.ReadFile(filepath.Join(box.Root(), "experiments.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Fatalf("записів у журналі = %d, want 5 (бюджет)", len(lines))
	}
	keeps := 0
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("рядок %d не JSON-line: %v", i+1, err)
		}
		for _, field := range []string{"prev_metric", "new_metric", "verdict", "diff_summary"} {
			if _, ok := m[field]; !ok {
				t.Errorf("рядок %d без поля %q", i+1, field)
			}
		}
		if m["verdict"] == string(improve.Keep) {
			keeps++
		}
	}
	if keeps == 0 {
		t.Error("жодного keep: крива не зросла, і капстон нічого не демонструє")
	}

	// Стратегія на диску мусить містити важелі з прийнятого кандидата.
	final, err := box.ReadStrategy()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final), "kb_read") {
		t.Errorf("прийняту стратегію не записано на диск:\n%s", final)
	}
	// Дискарди мусять лежати в experiments/ — єдиному іншому мутабельному місці.
	entries, err := os.ReadDir(box.ExperimentsPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("жодного дискарду не збережено в experiments/")
	}
}

// TestReportOnlyLeavesTheWorkspaceAlone — L1-режим нічого не змінює.
func TestReportOnlyLeavesTheWorkspaceAlone(t *testing.T) {
	box := testWorkspace(t, baseStrategy)
	var out strings.Builder
	if err := cmdImprove(context.Background(), &out, box, ledgerkb.New(), offline.Worker{}, 2, true); err != nil {
		t.Fatal(err)
	}
	final, err := box.ReadStrategy()
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != baseStrategy {
		t.Errorf("report-only змінив strategy.md:\n%s", final)
	}
	if !strings.Contains(out.String(), "report-only") {
		t.Error("вивід не каже, що це report-only")
	}
}

// TestCLICommandsOffline — поверхня команд: eval, generalize, одноразовий
// запит. Без ключа й без мережі.
func TestCLICommandsOffline(t *testing.T) {
	box := testWorkspace(t, goodStrategy)
	ctx := context.Background()
	kb := ledgerkb.New()
	tests := []struct {
		name string
		fn   func(io.Writer) error
		want []string
	}{
		{"eval", func(w io.Writer) error { return cmdEval(ctx, w, kb, offline.Worker{}, goodStrategy) },
			[]string{"dev", "heldout", "токени"}},
		{"generalize", func(w io.Writer) error { return cmdGeneralize(ctx, w, kb, offline.Worker{}, goodStrategy) },
			[]string{"генералізація", "разом"}},
		{"один запит", func(w io.Writer) error {
			return cmdOnce(ctx, w, kb, offline.Worker{}, goodStrategy, "SEV1 paging time")
		}, []string{"final_report", "токени"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			if err := tt.fn(&out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("вивід не містить %q:\n%s", want, out.String())
				}
			}
		})
	}
	_ = box
}

// TestRunRefusesWithoutAStrategy — без strategy.md програма мусить сказати, що
// саме створити, а не впасти з ENOENT.
func TestRunRefusesWithoutAStrategy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var out strings.Builder
	err := run(context.Background(), []string{"-offline", "-root", root, "-eval"}, &out)
	if err == nil {
		t.Fatal("відсутня стратегія не дала помилки")
	}
	if !strings.Contains(err.Error(), sandbox.StrategyFile) {
		t.Errorf("помилка не називає файл: %v", err)
	}
}

// TestRunDispatchesEveryCommand — поверхня `run`: кожна команда доходить до
// своєї гілки, і прапорці працюють. Це єдиний тест, що покриває CLI, і єдиний,
// який зловив би прапорець, зламаний рефакторингом.
func TestRunDispatchesEveryCommand(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"eval", []string{"-eval"}, []string{"dev", "heldout"}},
		{"generalize", []string{"-generalize"}, []string{"генералізація"}},
		{"один запит", []string{"-q", "SEV1 paging"}, []string{"final_report"}},
		{"improve report-only", []string{"-improve", "2", "-report-only"}, []string{"report-only", "базова метрика"}},
		{"improve", []string{"-improve", "2"}, []string{"справжній прогін", "журнал"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, sandbox.StrategyFile), []byte(goodStrategy), 0o644); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			args := append([]string{"-offline", "-root", root}, tt.args...)
			if err := run(ctx, args, &out); err != nil {
				t.Fatalf("run(%v): %v\n%s", args, err, out.String())
			}
			if !strings.Contains(out.String(), offline.Name) {
				t.Error("перший рядок не називає, що це не модель")
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("вивід не містить %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestPickModelSaysWhatItIs — без провайдера програма мусить сказати і що
// працює, і чого цей прогін НЕ доводить.
func TestPickModelSaysWhatItIs(t *testing.T) {
	t.Parallel()
	llm, label := pickModel(context.Background(), true)
	if !isOfflineModel(llm) {
		t.Error("прапорець -offline не дав правилову заглушку")
	}
	for _, want := range []string{offline.Name, "НЕ те, що LLM покращує"} {
		if !strings.Contains(label, want) {
			t.Errorf("рядок моделі = %q, want містити %q", label, want)
		}
	}
	if isOfflineModel(nil) != true {
		t.Error("nil-модель мусить вважатись офлайновою")
	}
}

// TestStripFenceAndClip — дрібні хелпери, у яких легко зробити тиху помилку:
// обрізання по байтах ламає UTF-8, а фенс без мови не знімається.
func TestStripFenceAndClip(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"просто текст", "просто текст"},
		{"```md\n# стратегія\n```", "# стратегія\n"},
		{"пояснення\n```\nвміст\n```\nхвіст", "вміст\n"},
	}
	for _, c := range cases {
		if got := stripFence(c.in); got != c.want {
			t.Errorf("stripFence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := clipLine(strings.Repeat("ї", 100), 10)
	if !utf8.ValidString(long) {
		t.Errorf("clipLine зламав UTF-8: %q", long)
	}
	if n := utf8.RuneCountInString(long); n != 11 {
		t.Errorf("рун = %d, want 11", n)
	}
	if got := clipLine("а\nб", 10); got != "а б" {
		t.Errorf("clipLine не прибрав перенос: %q", got)
	}
	if got := dedup([]string{"a", "a", "b", "b", "c"}); len(got) != 3 {
		t.Errorf("dedup = %v", got)
	}
}

// TestKbBranchReportsAGapInsteadOfInventing — порожня база знань дає прогалину
// з причиною, а не вигаданий розділ.
func TestKbBranchReportsAGapInsteadOfInventing(t *testing.T) {
	t.Parallel()
	p := Pipeline{KB: ledgerkb.NewFrom(nil), Model: offline.Worker{}}
	out, err := p.kbFn(nil, Plan{Query: "будь-що"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Ev.Gap == "" {
		t.Error("порожня база знань не дала прогалини")
	}
	if len(out.Ev.Items) != 0 {
		t.Errorf("з порожньої бази зʼявились факти: %+v", out.Ev.Items)
	}
}

// TestCriticLoopStopsAtItsBudget — цикл критиків має власний бюджет і віддає
// чернетку з незакритими зауваженнями, а не крутиться вічно.
func TestCriticLoopStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	// Чернетка без джерел: критик sources ніколи не пройде, бо редрафт джерел
	// не додає — саме той випадок, для якого бюджет і існує.
	p := Pipeline{KB: ledgerkb.New(), Model: offline.Worker{}, Strategy: goodStrategy}
	d := Draft{Plan: Plan{Query: "SEV1 paging time", Strategy: goodStrategy}, Text: "щось", Sources: nil}
	// Вузол усередині викликає агента, тож context.Background() тут не
	// підходить за типом — потрібен agent.Context.
	got, err := p.criticFn(newMockCtx(), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feedback) == 0 {
		t.Fatal("цикл вийшов без жодного незакритого зауваження")
	}
	joined := strings.Join(got.Feedback, " ")
	if !strings.Contains(joined, "бюджет") {
		t.Errorf("вихід не названий як вичерпаний бюджет: %v", got.Feedback)
	}
}

// newMockCtx returns a minimal agent.Context. Actions has a pointer receiver,
// so the mock must be addressable — hence the &.
func newMockCtx() agent.Context {
	m := agent.NewStrictContextMock(context.Background())
	return &m
}

// fixedModel is a model.LLM that always answers the same text. It stands in for
// a provider so the live-only paths are reachable without a key.
type fixedModel struct{ text string }

func (fixedModel) Name() string { return "fixed-test-model" }

func (f fixedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText(f.text, genai.RoleModel)}, nil)
	}
}

// TestLiveProposerTakesTheModelsTextAsTheCandidate — пропонент на моделі не
// пише файл сам: він віддає текст, а записує рушій через песочницю. Тому
// принцип (а) тримається, навіть якщо модель попросить щось інше.
func TestLiveProposerTakesTheModelsTextAsTheCandidate(t *testing.T) {
	t.Parallel()
	box := testWorkspace(t, baseStrategy)
	p := &LiveProposer{Model: fixedModel{text: "```md\n# нова стратегія\nЧитай kb_read.\n```"}, Sandbox: box}
	prop, err := p.Propose(context.Background(), baseStrategy,
		improve.Measurement{Dev: 0.1, Found: 2, Total: 24}, []string{"window", "fee"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prop.Content, "```") {
		t.Errorf("огорожу не знято: %q", prop.Content)
	}
	if !strings.Contains(prop.Content, "kb_read") {
		t.Errorf("кандидат не містить тексту моделі: %q", prop.Content)
	}
	if prop.Summary == "" {
		t.Error("пропозиція без diff_summary")
	}
	// Файл на диску НЕ змінився: пропонент лише пропонує.
	got, err := box.ReadStrategy()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != baseStrategy {
		t.Errorf("пропонент сам записав стратегію:\n%s", got)
	}

	// Порожня відповідь моделі — названа помилка, а не порожня стратегія.
	empty := &LiveProposer{Model: fixedModel{text: "   "}, Sandbox: box}
	if _, err := empty.Propose(context.Background(), baseStrategy, improve.Measurement{}, nil); err == nil {
		t.Error("порожня відповідь моделі прийнята як кандидат")
	}
}

// TestCriticModelPrefersTheProviderWhenThereIsOne — з живою моделлю критик це
// llmagent на ній, а не правило.
func TestCriticModelPrefersTheProviderWhenThereIsOne(t *testing.T) {
	t.Parallel()
	live := Pipeline{Model: fixedModel{text: `{"critic":"sources","passed":true}`}}
	if got := live.criticModel(reportCritics()[0]); got.Name() != "fixed-test-model" {
		t.Errorf("з провайдером критик використав %q", got.Name())
	}
	off := Pipeline{Model: offline.Worker{}}
	if got := off.criticModel(reportCritics()[0]); !strings.HasPrefix(got.Name(), "rule-critic-") {
		t.Errorf("без провайдера критик використав %q", got.Name())
	}
}
