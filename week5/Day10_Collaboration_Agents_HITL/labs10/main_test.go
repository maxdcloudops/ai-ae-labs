// Каркас тестів ДЗ 10. Він компілюється й проходить одразу, тому перший
// `go test` у цій лабі показує «ok … [skipped]», а не «[no test files]».
//
// Різниця не косметична. «Немає тестових файлів» виглядає як успіх — і
// рівно тому нікого не змушує подивитись уважніше. Це та сама помилка,
// яку ми в Блоці 4 називаємо про затвор: результат без названої причини
// гірший за помилку.
//
// Усі три тести нижче — скелети під конкретні пункти ДЗ. Зніміть t.Skip,
// коли писатимете тіло.
//
//	go test ./...                    # зелено, скіпи видно
//	go test -run Approval -v ./...
//
// Жоден тест цієї лаби не має вимагати GOOGLE_API_KEY: те, що варто
// перевіряти — топологія графа, політика затвора, стримування збою —
// перевіряється без моделі. Якщо ваш тест не запускається без ключа,
// ви тестуєте Gemini, а не свій код.
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// testEngine builds the real graph over a throwaway ledger. Each test gets its
// own SQLite file, so tests stay parallel-safe and never share a decision.
func testEngine(t *testing.T, failing map[string]bool, window time.Duration) (Engine, *approvals.Store) {
	t.Helper()
	store, err := approvals.Open(filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ds := desks(failing)
	return Engine{
		Plan: Plan{
			Topic:      "Міграція з legacy-процесора",
			Recipients: []string{"board@ledgerworks.example"},
			Deadline:   "понеділок 18:00",
		},
		Store:    store,
		Desks:    ds,
		ModelFor: func(d desk) model.LLM { return d.OfflineModel() },
		Window:   window,
	}, store
}

// startAndPark runs the graph until it parks, and returns the pending request.
func startAndPark(t *testing.T, e Engine, sid string) turnResult {
	t.Helper()
	ag, err := e.Agent()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := turn(context.Background(), ag, e.Store.Sessions(), sid, userText(e.Plan.Topic))
	if err != nil {
		t.Fatal(err)
	}
	if tr.pending == nil {
		t.Fatalf("ран не припаркувався — затвор не спрацював: %+v", tr.outputs)
	}
	return tr
}

// resumeWith unparks the run and returns the send node's output.
func resumeWith(t *testing.T, e Engine, sid, id, answer string) string {
	t.Helper()
	ag, err := e.Agent()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := turn(context.Background(), ag, e.Store.Sessions(), sid, resumeMessage(id, answer))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(tr.outputs, "\n")
}

// TestJoinNodeWaitsForAllBranches — ДЗ 10, п.2 (fan-out / fan-in, 20 балів).
//
// Що довести: JoinNode — це БАР'ЄР. Він активується лише тоді, коли
// відзвітували ВСІ попередники, перелічені в AddFanIn. Найчастіша
// помилка — перелічити не всіх, і тоді граф тихо йде далі з неповним
// входом.
//
// Каркас: зібрати граф із двох гілок, прогнати, перевірити, що вхід
// JoinNode містить обидва ключі. Зразок топології —
// samples/adk/adk-go/examples/workflow/complex/main.go, рядки 221/230/231.
func TestJoinNodeWaitsForAllBranches(t *testing.T) {
	t.Parallel()

	// A deliberately uneven pair: the slow branch finishes last. If the join
	// were not a barrier, merge would run on the fast branch alone and the
	// test would see one key.
	fast := workflow.NewFunctionNode("fast", func(_ agent.Context, in string) (string, error) {
		return "fast:" + in, nil
	}, workflow.NodeConfig{})
	slow := workflow.NewFunctionNode("slow", func(_ agent.Context, in string) (string, error) {
		time.Sleep(80 * time.Millisecond)
		return "slow:" + in, nil
	}, workflow.NodeConfig{})

	join := workflow.NewJoinNode("join_two")
	seen := make(chan map[string]any, 1)
	merge := workflow.NewFunctionNode("merge_two", func(_ agent.Context, m map[string]any) (string, error) {
		seen <- m
		return fmt.Sprintf("%d", len(m)), nil
	}, workflow.NodeConfig{})

	b := workflow.NewEdgeBuilder()
	start := workflow.NewFunctionNode("start_two", func(_ agent.Context, in string) (string, error) {
		return in, nil
	}, workflow.NodeConfig{})
	b.Add(workflow.Start, start)
	b.AddFanOut(start, fast, slow)
	b.AddFanIn(join, fast, slow)
	b.Add(join, merge)

	ag, err := workflowagent.New(workflowagent.Config{Name: "join_test", Edges: b.Build()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := approvals.Open(filepath.Join(t.TempDir(), "join.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := turn(context.Background(), ag, store.Sessions(), "join", userText("x")); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	select {
	case got = <-seen:
	default:
		t.Fatal("merge не виконався — join не активувався жодного разу")
	}
	for _, want := range []string{"fast", "slow"} {
		if _, ok := got[want]; !ok {
			t.Errorf("вхід join не містить %q: %v (join пропустив гілку)", want, keysOf(got))
		}
	}
	if len(got) != 2 {
		t.Errorf("ключів у join: %v, want рівно 2", keysOf(got))
	}
}

// TestFanInMustBeAJoinNode — той самий бар'єр з іншого боку: якщо звести дві
// безумовні гілки в звичайний вузол, ADK відмовляється збирати граф. Це
// найдешевша перевірка «я перелічив не всіх» — вона падає ще до моделі.
func TestFanInMustBeAJoinNode(t *testing.T) {
	t.Parallel()
	a := workflow.NewFunctionNode("a", func(_ agent.Context, in string) (string, error) { return in, nil }, workflow.NodeConfig{})
	bb := workflow.NewFunctionNode("b", func(_ agent.Context, in string) (string, error) { return in, nil }, workflow.NodeConfig{})
	sink := workflow.NewFunctionNode("sink", func(_ agent.Context, in string) (string, error) { return in, nil }, workflow.NodeConfig{})

	eb := workflow.NewEdgeBuilder()
	eb.Add(workflow.Start, a)
	eb.Add(workflow.Start, bb)
	eb.Add(a, sink)
	eb.Add(bb, sink) // дві безумовні гілки в НЕ-join
	_, err := workflow.New("bad_fan_in", eb.Build())
	if !errors.Is(err, workflow.ErrUnsupportedFanIn) {
		t.Fatalf("err = %v, want ErrUnsupportedFanIn", err)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSpecialistFailureDoesNotCorruptTheAnswer — ДЗ 10, п.6 (containment).
//
// Що довести: збій одного спеціаліста стає ВИДИМИМ структурованим
// результатом (status=unverified + код помилки), а не зникає під
// правдоподібним абзацом координатора. Це рівно той рядок логу з
// холодного відкриття: risk_desk status=error code=CRM_TIMEOUT.
//
// Сильна форма твердження: перед відправкою неперевірений розділ має
// БЛОКУВАТИ approve. Тест, який лише перевіряє, що помилка десь
// залогована, цього не доводить.
func TestSpecialistFailureDoesNotCorruptTheAnswer(t *testing.T) {
	t.Parallel()
	e, store := testEngine(t, map[string]bool{DeskIntegration: true}, time.Minute)
	ctx := context.Background()

	tr := startAndPark(t, e, "fail")
	question := humanPrompt(tr.pending)

	// 1. Збій став ВИДИМИМ: статус і код у питанні людині.
	for _, want := range []string{
		DeskIntegration + "=" + StatusUnverified,
		"INTEGRATION_DESK_TIMEOUT",
		"ПРОГАЛИНА",
	} {
		if !strings.Contains(question, want) {
			t.Errorf("у доказі для людини немає %q:\n%s", want, question)
		}
	}

	// 2. Документ не вигадано: жодного факту від мертвого джерела.
	if strings.Contains(question, "merchant-api:legacy-list") {
		t.Error("у документі є факт зі столу, джерело якого не відповіло")
	}

	// 3. Інші два розділи не пошкоджені — збій стримано однією гілкою.
	for _, want := range []string{DeskRisk + "=" + StatusVerified, DeskCompliance + "=" + StatusVerified} {
		if !strings.Contains(question, want) {
			t.Errorf("збій одного стола зачепив інший: немає %q", want)
		}
	}

	// 4. СИЛЬНА ФОРМА: навіть approve не надсилає документ.
	rec, err := store.Decide(ctx, tr.pending.ID, approvals.Decision{
		Status: approvals.Approved, By: "oksana", Reason: "схвалено людиною",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != approvals.Approved {
		t.Fatalf("статус у журналі = %s, want approved", rec.Status)
	}
	out := resumeWith(t, e, "fail", tr.pending.ID, "approve")
	if !strings.Contains(out, "НЕ надіслано") {
		t.Fatalf("документ із неперевіреним розділом надіслано: %q", out)
	}
	if !strings.Contains(out, DeskIntegration) || !strings.Contains(out, "INTEGRATION_DESK_TIMEOUT") {
		t.Errorf("відмова не називає, який саме розділ і чому: %q", out)
	}

	// 5. Схвалення НЕ витрачено: дія не відбулась, і це видно в журналі.
	after, err := store.Get(ctx, tr.pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ConsumedAt != nil {
		t.Error("схвалення позначено використаним, хоча документ не надсилали")
	}
}

// TestApprovalTimeoutMeansRefusal — ДЗ 10, п.7 (вікно рішення, 5 балів).
//
// Що довести: мовчання протягом вікна рішення — це ВІДМОВА, і відмова з
// названою причиною. Затвор, який чекає вічно, — не затвор, а зависла
// задача: дія не сталася, і ніхто про це не дізнався.
//
// Каркас: відкрити канал відповідей, у який ніхто ніколи не напише, дати
// коротке вікно (десятки мілісекунд — тест не має тривати 2 хвилини) і
// вимагати відмови. Перевіряти причину через errors.Is, а не порівнянням
// рядків.
//
// Чотири шляхи мають вести у відмову, кожен зі СВОЄЮ причиною: таймаут,
// скасований контекст, закритий канал, нерозпізнане слово. «Людина
// відмовила», «людини не було» і «UI надіслав щось незрозуміле» — три
// різні події в журналі при однаковому наслідку.
//
// Спершу спробуйте самі; цей тест стоїть на t.Skip, доки ви його не напишете.
func TestApprovalTimeoutMeansRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Вікно — десятки мілісекунд: тест не має тривати дві хвилини.
	const window = 50 * time.Millisecond
	e, store := testEngine(t, nil, window)
	tr := startAndPark(t, e, "timeout")

	if _, err := store.Get(ctx, tr.pending.ID); err != nil {
		t.Fatalf("запит не записано в журнал ДО паузи: %v", err)
	}

	// Ніхто не відповідає. Чекаємо вікно і прогоняємо розгортку.
	time.Sleep(2 * window)
	expired, err := store.Expire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("прострочено запитів: %d, want 1", len(expired))
	}
	rec := expired[0]
	if rec.Status != approvals.TimedOut {
		t.Errorf("статус = %s, want %s", rec.Status, approvals.TimedOut)
	}
	if rec.DecidedBy != "system:timeout" {
		t.Errorf("DecidedBy = %q, want system:timeout — відмова має автора", rec.DecidedBy)
	}
	if !strings.Contains(rec.Reason, "ніхто не відповів") {
		t.Errorf("причина = %q, want названу причину", rec.Reason)
	}

	// Мовчання = відмова: дія не відбулась.
	out := resumeWith(t, e, "timeout", tr.pending.ID, "timeout")
	if !strings.Contains(out, "НЕ надіслано") || !strings.Contains(out, string(approvals.TimedOut)) {
		t.Fatalf("після таймауту документ не має надсилатись: %q", out)
	}

	// Пізніше «approve» уже нічого не змінює: рішення ухвалюється один раз.
	if _, err := store.Decide(ctx, tr.pending.ID, approvals.Decision{
		Status: approvals.Approved, By: "oksana",
	}); !errors.Is(err, approvals.ErrAlreadyDecided) {
		t.Errorf("err = %v, want ErrAlreadyDecided", err)
	}
}

// TestThreeRefusalsAreThreeRecords — п.4: однаковий наслідок, різні записи.
//
// «Людина відмовила», «людини не було» і «UI надіслав незрозуміле» ведуть в
// одну точку — документ не надіслано, — але в журналі це три різні події з
// трьома причинами. Журнал, у якому все це «не схвалено», не дає відповісти
// на питання «то нас відмовили чи ми просто проспали дедлайн?».
func TestThreeRefusalsAreThreeRecords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		answer     string
		wantStatus approvals.Status
		wantBy     string
		wantReason string
	}{
		{"людина відмовила", "deny", approvals.Denied, "oksana", "людина відмовила"},
		{"UI надіслав незрозуміле", "ок?", approvals.Invalid, "oksana", "нерозпізнана відповідь"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, store := testEngine(t, nil, time.Minute)
			tr := startAndPark(t, e, "refusal")
			d, err := parseAnswer("oksana", tt.answer)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := store.Decide(context.Background(), tr.pending.ID, d)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status != tt.wantStatus {
				t.Errorf("статус = %s, want %s", rec.Status, tt.wantStatus)
			}
			if rec.DecidedBy != tt.wantBy {
				t.Errorf("DecidedBy = %q, want %q", rec.DecidedBy, tt.wantBy)
			}
			if !strings.Contains(rec.Reason, tt.wantReason) {
				t.Errorf("причина = %q, want містити %q", rec.Reason, tt.wantReason)
			}
			out := resumeWith(t, e, "refusal", tr.pending.ID, tt.answer)
			if !strings.Contains(out, "НЕ надіслано") {
				t.Errorf("документ надіслано попри %s: %q", tt.wantStatus, out)
			}
		})
	}
}

// TestApprovalIsSpentOnce — схвалення витрачається відправкою.
//
// Повторний resume того самого запиту не має надіслати документ удвічі:
// Spend — умовний UPDATE, і другий раз він не знаходить рядка.
func TestApprovalIsSpentOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, store := testEngine(t, nil, time.Minute)
	tr := startAndPark(t, e, "once")
	if _, err := store.Decide(ctx, tr.pending.ID, approvals.Decision{
		Status: approvals.Approved, By: "oksana", Reason: "схвалено людиною",
	}); err != nil {
		t.Fatal(err)
	}

	first := resumeWith(t, e, "once", tr.pending.ID, "approve")
	if !strings.Contains(first, "надіслано") || strings.Contains(first, "НЕ надіслано") {
		t.Fatalf("перша відправка не відбулась: %q", first)
	}
	second := resumeWith(t, e, "once", tr.pending.ID, "approve")
	if !strings.Contains(second, "НЕ надіслано") {
		t.Fatalf("документ надіслано двічі: %q", second)
	}
	if !strings.Contains(second, "вже використано") {
		t.Errorf("друга відмова не називає причину: %q", second)
	}
}

// TestRiskToolReturnsOnlySourcedFindings — ранній win, без моделі: кожен факт
// інструмента має джерело. Спеціаліст, який переказує факт без джерела, —
// початок вигаданого розділу.
func TestRiskToolReturnsOnlySourcedFindings(t *testing.T) {
	out, err := lookupRisk(nil, riskInput{Topic: "міграція"})
	if err != nil || len(out.Findings) == 0 {
		t.Fatalf("lookupRisk: %v %+v", err, out)
	}
	for _, f := range out.Findings {
		if f.Source == "" {
			t.Errorf("факт без джерела: %+v", f)
		}
	}
}

// TestCLIWholeScenarioOffline drives run() the way a person does: start in one
// call, decide in another, then the audit. It is the only test that covers the
// command surface — and the only one that would catch a flag parsed on the
// wrong side of the verb, which is a mistake that does not show up in the graph
// tests at all.
func TestCLIWholeScenarioOffline(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cli.db")
	ctx := context.Background()
	var out strings.Builder

	// Flags BEFORE the verb and AFTER it, on purpose: both must work.
	if err := runArgs(ctx, &out, "-offline", "start", "-db", db, "-session", "cli", "-trace", "Міграція"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(out.String(), "потік подій") {
		t.Error("-trace після глагола не застосувався")
	}
	id := firstInterruptID(out.String())
	if id == "" {
		t.Fatalf("у виводі start немає InterruptID:\n%s", out.String())
	}

	out.Reset()
	if err := runArgs(ctx, &out, "pending", "-db", db); err != nil {
		t.Fatalf("pending: %v", err)
	}
	if !strings.Contains(out.String(), id) {
		t.Errorf("pending не показує запит %s:\n%s", id, out.String())
	}

	out.Reset()
	if err := runArgs(ctx, &out, "decide", "-db", db, "-session", "cli", "-id", id, "-by", "oksana", "-offline", "approve"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !strings.Contains(out.String(), "надіслано") || strings.Contains(out.String(), "НЕ надіслано") {
		t.Errorf("після approve документ не надіслано:\n%s", out.String())
	}

	out.Reset()
	if err := runArgs(ctx, &out, "audit", "-db", db); err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, want := range []string{"approved", "oksana", id} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("журнал не містить %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runArgs(ctx, &out, "expire", "-db", db); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if !strings.Contains(out.String(), "прострочених запитів немає") {
		t.Errorf("expire після рішення не має нічого просрочувати:\n%s", out.String())
	}
}

// TestCLIRefusesIncompleteCommands — помилки CLI мають бути названими, а не
// тихим успіхом: «рішення без імені» і «рішення без id» не повинні проходити.
func TestCLIRefusesIncompleteCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"невідома команда", []string{"nope"}, "невідома команда"},
		{"decide без -id", []string{"decide", "-by", "oksana", "approve"}, "потрібен -id"},
		{"decide без -by", []string{"decide", "-id", "x", "approve"}, "потрібен -by"},
		{"decide без відповіді", []string{"decide", "-id", "x", "-by", "oksana"}, "потрібна відповідь"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Own database file per subtest. Sharing one file broke the
			// parallel run on AutoMigrate ("table sessions already exists"):
			// two first-time opens of the same new SQLite file race. The CLI
			// opens it once per process, so this is a property of the test,
			// but it IS a property of approvals.Open — see README.
			db := filepath.Join(t.TempDir(), "bad.db")
			var out strings.Builder
			err := runArgs(ctx, &out, append(tt.args, "-db", db)...)
			if err == nil {
				t.Fatalf("помилки не було, а має бути %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want містити %q", err, tt.want)
			}
		})
	}
}

// TestEarlyWinOffline — `go run .` без провайдера все одно друкує свої два
// рядки: назву інструмента і відповідь.
func TestEarlyWinOffline(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := runArgs(context.Background(), &out, "-offline"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lookup_risk_desk", "incident-db:INC-418"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ранній win не містить %q:\n%s", want, out.String())
		}
	}
}

// TestClipKeepsUkrainianIntact — clip ріже по рунах, не по байтах. Перша
// версія різала по len() і друкувала «п�» замість обрізаного слова.
func TestClipKeepsUkrainianIntact(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ї", 100)
	got := clip(long)
	if !utf8.ValidString(got) {
		t.Fatalf("clip зламав UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 73 { // 72 руни + «…»
		t.Errorf("рун у результаті: %d, want 73", n)
	}
	if got := clip("а\nб"); !strings.Contains(got, "⏎") {
		t.Errorf("перенос рядка не замінено: %q", got)
	}
}

// runArgs is run() with a variadic argument list, for readable tests.
func runArgs(ctx context.Context, out *strings.Builder, args ...string) error {
	return run(ctx, args, out)
}

// firstInterruptID pulls the first approval id out of command output.
func firstInterruptID(s string) string {
	i := strings.Index(s, "approval-")
	if i < 0 {
		return ""
	}
	id := s[i:]
	if j := strings.IndexAny(id, " \n\t)"); j >= 0 {
		id = id[:j]
	}
	return id
}

// TestCLIExpireUnparksTheRun — шлях «ніхто не відповів» через CLI, а не через
// Store напряму: expire має і записати відмову, і розпаркувати ран.
func TestCLIExpireUnparksTheRun(t *testing.T) {
	db := filepath.Join(t.TempDir(), "exp.db")
	ctx := context.Background()
	var out strings.Builder
	if err := runArgs(ctx, &out, "start", "-db", db, "-session", "exp", "-offline", "-window", "40ms", "Міграція"); err != nil {
		t.Fatal(err)
	}
	if firstInterruptID(out.String()) == "" {
		t.Fatalf("ран не припаркувався:\n%s", out.String())
	}
	time.Sleep(120 * time.Millisecond)

	out.Reset()
	if err := runArgs(ctx, &out, "expire", "-db", db, "-session", "exp", "-offline"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{string(approvals.TimedOut), "ніхто не відповів", "НЕ надіслано"} {
		if !strings.Contains(got, want) {
			t.Errorf("вивід expire не містить %q:\n%s", want, got)
		}
	}
}

// TestDeskErrorIsUnavailable — сентинел працює через обгортки: саме на нього
// спирається гілка, яка НЕ питає модель про мертве джерело.
func TestDeskErrorIsUnavailable(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("гілка: %w", &deskError{Code: "X_TIMEOUT", Why: "не відповідає"})
	if !errors.Is(err, errDeskUnavailable) {
		t.Error("errors.Is(err, errDeskUnavailable) = false — класифікація зламається")
	}
	var de *deskError
	if !errors.As(err, &de) || de.Code != "X_TIMEOUT" {
		t.Errorf("errors.As не дістав код: %+v", de)
	}
	if !strings.Contains(err.Error(), "X_TIMEOUT") {
		t.Errorf("текст помилки не називає код: %v", err)
	}
}

// TestMergeFallsBackToConfiguredOrder — якщо жодна гілка не принесла плану,
// порядок береться з конфігурації, а не з ітерації мапи. Документ лишається
// детермінованим, і в README сказано, що порядок не з цього рану.
func TestMergeFallsBackToConfiguredOrder(t *testing.T) {
	t.Parallel()
	e, _ := testEngine(t, nil, time.Minute)
	joined := map[string]any{}
	for _, d := range e.Desks {
		// Гілки без плану: саме той випадок, коли fallback має спрацювати.
		joined[d.Name] = branchOut{Section: Section{
			Desk: d.Name, Status: StatusVerified,
			Findings: []Finding{{Claim: "x", Source: "s:1"}},
		}}
	}
	got, err := e.mergeFn(nil, joined)
	if err != nil {
		t.Fatal(err)
	}
	want := deskNames(e.Desks)
	if len(got.Report.Sections) != len(want) {
		t.Fatalf("розділів: %d, want %d", len(got.Report.Sections), len(want))
	}
	for i, name := range want {
		if got.Report.Sections[i].Desk != name {
			t.Errorf("розділ %d = %s, want %s", i, got.Report.Sections[i].Desk, name)
		}
	}
}

// TestMergeTurnsAMissingBranchIntoAGap — стіл, який є в плані, але не
// відзвітував, стає прогалиною. Коротший документ виглядав би як повний.
func TestMergeTurnsAMissingBranchIntoAGap(t *testing.T) {
	t.Parallel()
	e, _ := testEngine(t, nil, time.Minute)
	plan := e.Plan
	plan.Order = deskNames(e.Desks)
	joined := map[string]any{
		DeskRisk: branchOut{Plan: plan, Section: Section{Desk: DeskRisk, Status: StatusVerified}},
	}
	got, err := e.mergeFn(nil, joined)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Report.Sections) != len(plan.Order) {
		t.Fatalf("розділів: %d, want %d — зниклий стіл має стати прогалиною", len(got.Report.Sections), len(plan.Order))
	}
	gaps := got.Report.Gaps()
	if len(gaps) != 2 {
		t.Fatalf("прогалин: %d, want 2", len(gaps))
	}
	for _, g := range gaps {
		if g.Code != "DESK_MISSING" {
			t.Errorf("%s: код = %q, want DESK_MISSING", g.Desk, g.Code)
		}
	}
	if got.Report.Verified() {
		t.Error("звіт із прогалинами вважається перевіреним")
	}
}

// TestPolicyGateIsNotOnlyOnTheModelPath — ++ Advanced, policy-точка.
//
// Головне твердження не «колбек спрацював», а «дані не потрапили в документ».
// Колбек закриває лише шлях через модель; гілка читає джерело напряму, тому
// політика перевіряється і там. Тест дивиться на РЕЗУЛЬТАТ: чи є в звіті
// джерела, яких роль не має права бачити.
func TestPolicyGateIsNotOnlyOnTheModelPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role        Role
		wantGaps    int
		wantSources int
		forbidden   string
	}{
		{RoleCoordinator, 0, 6, ""},
		{RoleAnalyst, 1, 4, "merchant-api"},
		{RoleGuest, 3, 0, "incident-db"},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			e, _ := testEngine(t, nil, time.Minute)
			e.Role = tt.role
			tr := startAndPark(t, e, "role")
			question := humanPrompt(tr.pending)

			if tt.forbidden != "" && strings.Contains(question, tt.forbidden) {
				t.Errorf("роль %s бачить джерело %q, якого не має права бачити:\n%s",
					tt.role, tt.forbidden, question)
			}
			if tt.role != RoleCoordinator && !strings.Contains(question, policyDeniedCode) {
				t.Errorf("прогалина не названа як %s:\n%s", policyDeniedCode, question)
			}
		})
	}
}

// TestPolicyIsFailClosed — невідома роль не має прав. Друкарська помилка в
// конфізі не повинна відкривати доступ.
func TestPolicyIsFailClosed(t *testing.T) {
	t.Parallel()
	if may("coordinatro", "lookup_risk_desk") {
		t.Error("невідома роль отримала доступ — політика fail-open")
	}
	if !may(RoleCoordinator, "lookup_risk_desk") {
		t.Error("coordinator не має доступу до lookup_risk_desk")
	}
	if may(RoleAnalyst, "lookup_integration_desk") {
		t.Error("analyst отримав доступ до merchant-даних")
	}
}

// TestPolicyGateShortCircuitsTheTool — колбек повертає ВІДМОВУ як результат,
// а не nil: модель має дізнатися, що її відмовили, і чому.
func TestPolicyGateShortCircuitsTheTool(t *testing.T) {
	t.Parallel()
	gate := policyGate(RoleAnalyst)
	d := desks(nil)[2] // integration_desk
	tl, err := functiontool.New(functiontool.Config{Name: d.ToolName, Description: d.ToolDesc}, d.toolFn)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gate(nil, tl, map[string]any{"topic": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("колбек пропустив заборонений виклик")
	}
	if got["code"] != policyDeniedCode {
		t.Errorf("code = %v, want %s", got["code"], policyDeniedCode)
	}
	if !strings.Contains(fmt.Sprint(got["reason"]), d.ToolName) {
		t.Errorf("причина не називає інструмент: %v", got["reason"])
	}

	allowed, err := policyGate(RoleCoordinator)(nil, tl, nil)
	if err != nil || allowed != nil {
		t.Errorf("дозволений виклик заблоковано: %v %v", allowed, err)
	}
}
