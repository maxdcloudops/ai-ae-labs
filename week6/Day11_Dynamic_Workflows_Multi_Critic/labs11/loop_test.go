package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/manifest"
	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/usage"
)

// runOffline runs one task with the scripted maker and the rule-based critics.
func runOffline(t *testing.T, taskID, makerKind string, attempts int) (LoopResult, *usage.Meter, error) {
	t.Helper()
	task, ok := manifest.ByID(taskID)
	if !ok {
		t.Fatalf("немає завдання %q", taskID)
	}
	res, _, meter, err := runTask(context.Background(), task, nil, attempts, true, makerKind)
	return res, meter, err
}

// TestVerdictValidationBySentinel — чотири різні малформації, чотири різні
// сентинели. Перевірка через errors.Is, а не за підрядком: від зміни
// формулювання тест не має ламатись, а від зміни КЛАСУ помилки — має.
func TestVerdictValidationBySentinel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		role string
		want error
	}{
		{"проза", "виглядає добре", CriticLimits, ErrVerdictUnreadable},
		{"невідома роль вузла", `{"critic":"x","passed":true}`, "correctness", ErrVerdictUnknownCritic},
		{"підміна ролі", `{"critic":"secrets","passed":true}`, CriticLimits, ErrVerdictRoleMismatch},
		{"порожнє поле critic", `{"passed":true}`, CriticLimits, ErrVerdictRoleMismatch},
		{"провал без коментарів", `{"critic":"limits","passed":false}`, CriticLimits, ErrVerdictSilentFailure},
		{"успіх із коментарями", `{"critic":"limits","passed":true,"comments":["а"]}`, CriticLimits, ErrVerdictContradiction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseVerdict(tt.raw, tt.role)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}
	// І зворотний бік: валідний вердикт проходить.
	if _, err := ParseVerdict(`{"critic":"limits","passed":false,"comments":["немає limits.cpu"]}`, CriticLimits); err != nil {
		t.Errorf("валідний вердикт відкинуто: %v", err)
	}
}

// TestCriticRulesStayInTheirLane — кожен критик знаходить свій клас проблем і
// НЕ чіпає чужий. Коментар не з свого фокусу ламає кворум: панель починає
// тричі повідомляти про одне й те саме, а про щось інше — жодного разу.
func TestCriticRulesStayInTheirLane(t *testing.T) {
	t.Parallel()
	// Маніфест, валідний за схемою, але погано налаштований одразу за трьома
	// напрямами: без securityContext, без resources, з секретом у відкритому
	// тексті.
	bad := `apiVersion: apps/v1
kind: Deployment
metadata: {name: pay-api}
spec:
  replicas: 2
  selector: {matchLabels: {app: pay-api}}
  template:
    metadata: {labels: {app: pay-api}}
    spec:
      containers:
        - name: api
          image: registry.example/pay-api:1.0.0
          env:
            - {name: LEDGER_PASSWORD, value: "hunter2"}
`
	m, err := manifest.Parse(bad)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range panel() {
		comments := c.Rule(m)
		if len(comments) == 0 {
			t.Errorf("критик %s не знайшов нічого в маніфесті, де є проблема його класу", c.Role)
			continue
		}
		joined := strings.ToLower(strings.Join(comments, " "))
		// Перехресні згадки: критик лімітів не має говорити про root, і т.д.
		foreign := map[string][]string{
			CriticPrivileges: {"resources", "env["},
			CriticLimits:     {"runasnonroot", "privileged", "env["},
			CriticSecrets:    {"runasnonroot", "resources"},
		}
		for _, word := range foreign[c.Role] {
			if strings.Contains(joined, word) {
				t.Errorf("критик %s пише про чужий фокус (%q): %v", c.Role, word, comments)
			}
		}
	}
}

// TestCriticRulesPassTheReference — еталон має бути чистим для всіх трьох.
// Критик, який чіпляється до еталона, зробить цикл нескінченним.
func TestCriticRulesPassTheReference(t *testing.T) {
	t.Parallel()
	for _, task := range manifest.All() {
		m, err := manifest.Parse(task.Reference)
		if err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		for _, c := range panel() {
			if comments := c.Rule(m); len(comments) > 0 {
				t.Errorf("%s: критик %s чіпляється до еталона: %v", task.ID, c.Role, comments)
			}
		}
	}
}

// TestOfflineCriticJudgesRatherThanReplays — офлайн-критик не скрипт: той
// самий критик на іншому маніфесті дає інший вердикт.
func TestOfflineCriticJudgesRatherThanReplays(t *testing.T) {
	t.Parallel()
	task, _ := manifest.ByID("pay_api_deployment")
	oc := offlineCritic{role: CriticLimits, rule: ruleLimits}

	clean := oc.verdictFor(`{"code":` + jsonString(task.Reference) + `}`)
	v, err := ParseVerdict(clean, CriticLimits)
	if err != nil || !v.Passed {
		t.Fatalf("на еталоні: %+v %v", v, err)
	}

	stripped := strings.Replace(task.Reference, "          resources:\n", "          notResources:\n", 1)
	dirty := oc.verdictFor(`{"code":` + jsonString(stripped) + `}`)
	v2, err := ParseVerdict(dirty, CriticLimits)
	if err != nil {
		t.Fatalf("на зіпсованому: %v", err)
	}
	if v2.Passed {
		t.Error("критик пропустив контейнер без resources — значить він не дивився")
	}
	if clean == dirty {
		t.Error("вердикт не змінився від зміни маніфесту — це скрипт, не критик")
	}
}

// TestAggregateFailsClosed — відкинутий або відсутній вердикт НІКОЛИ не дає
// прийняття. Інакше найдешевший спосіб пройти панель — зламати критика.
func TestAggregateFailsClosed(t *testing.T) {
	t.Parallel()
	roles := []string{CriticPrivileges, CriticLimits, CriticSecrets}
	ok := func(role string) string { return `{"critic":"` + role + `","passed":true}` }

	tests := []struct {
		name   string
		joined map[string]any
		want   string
	}{
		{"усі пройшли", map[string]any{
			CriticPrivileges: ok(CriticPrivileges), CriticLimits: ok(CriticLimits), CriticSecrets: ok(CriticSecrets),
		}, ""},
		{"один вердикт — проза", map[string]any{
			CriticPrivileges: ok(CriticPrivileges), CriticLimits: "норм", CriticSecrets: ok(CriticSecrets),
		}, "limits"},
		{"критик не відзвітував", map[string]any{
			CriticPrivileges: ok(CriticPrivileges), CriticSecrets: ok(CriticSecrets),
		}, "limits"},
		{"суперечливий вердикт", map[string]any{
			CriticPrivileges: `{"critic":"privileges","passed":true,"comments":["x"]}`,
			CriticLimits:     ok(CriticLimits), CriticSecrets: ok(CriticSecrets),
		}, "privileges"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := aggregate(tt.joined, roles)
			if tt.want == "" {
				if !agg.Passed {
					t.Fatalf("усі пройшли, а кворум не склався: %+v", agg)
				}
				return
			}
			if agg.Passed {
				t.Fatalf("кворум склався попри проблему з %s: %+v", tt.want, agg)
			}
			if len(agg.Rejected) == 0 {
				t.Error("проблемний вердикт не названий у Rejected")
			}
			if !strings.Contains(strings.Join(agg.Rejected, " "), tt.want) {
				t.Errorf("Rejected = %v, want згадку %q", agg.Rejected, tt.want)
			}
		})
	}
}

// TestQuorumIsUnanimous — два «так» не перебивають одне «ні». Фокуси не
// перетинаються, тому голосування тут було б помилкою: чистий вердикт про
// привілеї нічого не каже про відсутні ліміти памʼяті.
func TestQuorumIsUnanimous(t *testing.T) {
	t.Parallel()
	agg := aggregate(map[string]any{
		CriticPrivileges: `{"critic":"privileges","passed":true}`,
		CriticLimits:     `{"critic":"limits","passed":true}`,
		CriticSecrets:    `{"critic":"secrets","passed":false,"comments":["секрет відкритим текстом"]}`,
	}, []string{CriticPrivileges, CriticLimits, CriticSecrets})
	if agg.Passed {
		t.Fatal("двоє перебили одного — кворум не одностайний")
	}
	if len(agg.Feedback) != 1 || !strings.Contains(agg.Feedback[0], "secrets") {
		t.Errorf("фідбек = %v, want один рядок від secrets", agg.Feedback)
	}
}

// TestGateRunsBeforeCritics — об'єктивна перевірка перша. Якщо maker ніколи не
// віддає валідний маніфест, критиків не викликають ЖОДНОГО разу: інакше ми
// платимо трьом моделям за опис безпеки файлу з синтаксичною помилкою.
func TestGateRunsBeforeCritics(t *testing.T) {
	_, meter, err := runOffline(t, "pay_api_deployment", "stub", 3)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	for _, c := range panel() {
		if tok, ok := meter.ByAgent()[c.Role]; ok && tok.Calls > 0 {
			t.Errorf("критика %s викликали %d разів, хоча валідатор не пройдено жодного разу", c.Role, tok.Calls)
		}
	}
	if tok := meter.ByAgent()["maker"]; tok.Calls != 3 {
		t.Errorf("maker викликали %d разів, want 3 (бюджет)", tok.Calls)
	}
}

// TestBudgetExhaustedIsLoudAndLogged — вичерпаний бюджет це сентинел І подія
// в журналі, а не тихе «нічого не знайшли».
func TestBudgetExhaustedIsLoudAndLogged(t *testing.T) {
	task, _ := manifest.ByID("ledger_cronjob")
	res, events, _, err := runTask(context.Background(), task, nil, 2, true, "stub")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if res.Accepted != "" {
		t.Error("бюджет вичерпано, але щось «прийнято»")
	}
	var found bool
	for _, ev := range events {
		if ev.LLMResponse.Content == nil {
			continue
		}
		for _, p := range ev.LLMResponse.Content.Parts {
			if p != nil && strings.Contains(p.Text, "budget_exhausted") {
				found = true
			}
		}
	}
	if !found {
		t.Error("у журналі немає події budget_exhausted")
	}
	if res.GateFailures != 2 {
		t.Errorf("GateFailures = %d, want 2 — счётчики виводяться з журналу", res.GateFailures)
	}
}

// TestFalseAcceptIsDetected — головна цифра звіту працює в обидві сторони:
// «майже правильний» maker дає хибне прийняття, еталонний — ні. Якби обидва
// давали однакове число, harness міряв би себе.
func TestFalseAcceptIsDetected(t *testing.T) {
	for _, tc := range []struct {
		maker           string
		wantFalseAccept bool
	}{
		{"buggy", true},
		{"reference", false},
	} {
		t.Run(tc.maker, func(t *testing.T) {
			for _, task := range manifest.All() {
				res, _, _, err := runTask(context.Background(), task, nil, 4, true, tc.maker)
				if err != nil {
					t.Fatalf("%s: %v", task.ID, err)
				}
				if res.Accepted == "" {
					t.Fatalf("%s: цикл нічого не прийняв", task.ID)
				}
				hold := manifest.Grade(res.Accepted, task.Holdout)
				if gotFalse := !hold.OK; gotFalse != tc.wantFalseAccept {
					t.Errorf("%s: хибне прийняття = %t, want %t (holdout: %v)",
						task.ID, gotFalse, tc.wantFalseAccept, hold.Failed)
				}
			}
		})
	}
}

// TestBrokenCriticStopsTheLoop — один невалідний вердикт це відмова й цикл
// продовжується; ДРУГИЙ від тієї самої ролі означає, що зламаний критик, а не
// чернетка, і далі крутити цикл — палити бюджет.
func TestBrokenCriticStopsTheLoop(t *testing.T) {
	t.Parallel()
	task, _ := manifest.ByID("pay_api_service")
	meter := &usage.Meter{}
	// Критик secrets завжди віддає прозу.
	broken := func(c critic) model.LLM {
		if c.Role == CriticSecrets {
			return offlineCritic{role: c.Role, broken: "виглядає нормально"}
		}
		return offlineCritic{role: c.Role, rule: c.Rule}
	}
	ag, _, err := buildAgentWith(task, offlineMaker(task, "reference"), 4, meter, broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runLoop(context.Background(), ag, task); !errors.Is(err, ErrCriticBroken) {
		t.Fatalf("err = %v, want ErrCriticBroken", err)
	}
}

// jsonString quotes a string for embedding in a JSON literal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// TestCLIOffline — весь CLI офлайн: одне завдання, eval у трьох режимах
// maker'а, і названі помилки на невідомому завданні. Це єдиний тест, що
// покриває поверхню команд, і єдиний, який зловив би прапорець, зламаний
// після рефакторингу.
func TestCLIOffline(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"одне завдання з трасою",
			[]string{"-offline", "-task", "pay_api_service", "-trace"},
			[]string{"потік подій", "loop_done", "w6p1:attempt:", "прийнятий маніфест"}},
		{"eval: майже правильний maker",
			[]string{"-offline", "-eval", "-maker", "buggy"},
			[]string{"ХИБНИХ ПРИЙНЯТТІВ: 3 із 3", "ПРОВАЛ"}},
		{"eval: еталонний maker",
			[]string{"-offline", "-eval", "-maker", "reference"},
			[]string{"ХИБНИХ ПРИЙНЯТТІВ: 0 із 3", "пройдено"}},
		{"eval: maker, що нічого не вміє",
			[]string{"-offline", "-eval", "-maker", "stub"},
			[]string{"бюджет вичерпано", "ХИБНИХ ПРИЙНЯТТІВ: 0 із 3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			if err := run(ctx, tt.args, &out); err != nil {
				t.Fatalf("run(%v): %v\n%s", tt.args, err, out.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("вивід не містить %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestCLINamesItsErrors — невідоме завдання це названа помилка, а не тихий
// дефолт на перше зі списку.
func TestCLINamesItsErrors(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	err := run(context.Background(), []string{"-offline", "-task", "nope"}, &out)
	if err == nil {
		t.Fatal("невідоме завдання не дало помилки")
	}
	if !strings.Contains(err.Error(), "невідоме завдання") {
		t.Errorf("err = %v, want «невідоме завдання»", err)
	}
	for _, id := range manifest.IDs() {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("помилка не підказує наявне завдання %q: %v", id, err)
		}
	}
}

// TestVerdictSchemaMatchesTheContract — схема лишилась як оголошений контракт;
// вона мусить описувати рівно ті поля, які перевіряє ValidateVerdict.
func TestVerdictSchemaMatchesTheContract(t *testing.T) {
	t.Parallel()
	sch := VerdictSchema()
	for _, f := range []string{"critic", "passed", "comments"} {
		if _, ok := sch.Properties[f]; !ok {
			t.Errorf("схема не описує поле %q", f)
		}
	}
	if len(sch.Required) != 2 {
		t.Errorf("Required = %v, want critic і passed", sch.Required)
	}
}

// TestMakerModelSaysWhyItIsOffline — без провайдера програма не падає, але й
// не вдає, що працює з моделлю: перший рядок називає причину.
func TestMakerModelSaysWhyItIsOffline(t *testing.T) {
	llm, label := makerModel(context.Background(), true)
	if llm != nil {
		t.Error("прапорець -offline має давати nil-модель (ознаку офлайну)")
	}
	for _, want := range []string{"ОФЛАЙН", "критики працюють за правилами"} {
		if !strings.Contains(label, want) {
			t.Errorf("перший рядок = %q, want містити %q", label, want)
		}
	}
}

// TestClipKeepsUkrainianIntact — обрізання по рунах, не по байтах.
func TestClipKeepsUkrainianIntact(t *testing.T) {
	t.Parallel()
	got := clip(strings.Repeat("ї", 200))
	if !utf8.ValidString(got) {
		t.Fatalf("clip зламав UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 89 {
		t.Errorf("рун = %d, want 89 (88 + «…»)", n)
	}
}

// TestCriticFeedbackReachesTheMaker — шлях, якого немає ні в Reference, ні в
// Buggy: панель відхиляє, її коментарі йдуть у Draft.Feedback, наступна
// чернетка їх виправляє. Без цього тесту «фідбек передається в generate» —
// речення в README, а не властивість.
func TestCriticFeedbackReachesTheMaker(t *testing.T) {
	task, _ := manifest.ByID("pay_api_deployment")
	res, events, meter, err := runTask(context.Background(), task, nil, 4, true, "insecure")
	if err != nil {
		t.Fatalf("цикл: %v", err)
	}
	if res.Accepted == "" {
		t.Fatal("цикл нічого не прийняв, хоча третя чернетка — еталон")
	}
	if res.Attempts != 3 {
		t.Errorf("спроб = %d, want 3 (гейт → панель → прийнято)", res.Attempts)
	}
	if res.GateFailures != 1 {
		t.Errorf("GateFailures = %d, want 1", res.GateFailures)
	}
	if res.PanelFailures != 1 {
		t.Errorf("PanelFailures = %d, want 1 — панель мусила відхилити рівно раз", res.PanelFailures)
	}

	// Коментарі всіх трьох критиків мусять бути в журналі: якщо фідбек збирає
	// лише перший, maker виправить один клас проблем із трьох і цикл
	// витратить спроби на те, що вже було відомо.
	var log strings.Builder
	for _, ev := range events {
		if ev.LLMResponse.Content == nil {
			continue
		}
		for _, p := range ev.LLMResponse.Content.Parts {
			if p != nil {
				log.WriteString(p.Text)
			}
		}
	}
	for _, want := range []string{"runAsNonRoot", "resources", "LEDGER_PASSWORD"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("журнал не містить зауваження про %q — фідбек неповний", want)
		}
	}
	// І холдаут мусить пройти: прийнято саме еталон.
	if rep := manifest.Grade(res.Accepted, task.Holdout); !rep.OK {
		t.Errorf("прийнятий маніфест не проходить holdout: %v", rep.Failed)
	}
	if tok := meter.ByAgent()["maker"]; tok.Calls != 3 {
		t.Errorf("maker викликали %d разів, want 3", tok.Calls)
	}
}

// TestServiceLeavesTheCriticsNothingToSay — спостереження, яке варто знати:
// у Service немає пода, тому всі три критики проходять тривіально. Панель із
// трьох голосів тихо стає панеллю з нуля.
func TestServiceLeavesTheCriticsNothingToSay(t *testing.T) {
	t.Parallel()
	task, _ := manifest.ByID("pay_api_service")
	m, err := manifest.Parse(task.Reference)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range panel() {
		if comments := c.Rule(m); len(comments) != 0 {
			t.Errorf("критик %s знайшов щось у Service: %v", c.Role, comments)
		}
	}
}

// TestCriticsRunConcurrently — ++ Advanced: панель критиків — це fan-out у
// статично оголошеному підграфі, викликаному через RunNode(WithUseSubBranch).
//
// Замір синтетичний і це сказано прямо: офлайн-критики — чисті функції й
// відповідають за мікросекунди, тому перекривати нічого. Затримка 150 мс
// імітує провайдера. Поріг узято з запасом (менше 2× замість «рівно 3×»): на
// завантаженій машині й за реальних лімітів провайдера точного множника не
// буде, а от послідовне виконання 3×150 мс від паралельного відрізняється
// однозначно.
func TestCriticsRunConcurrently(t *testing.T) {
	const delay = 150 * time.Millisecond
	task, _ := manifest.ByID("pay_api_deployment")
	meter := &usage.Meter{}
	slow := func(c critic) model.LLM {
		return offlineCritic{role: c.Role, rule: c.Rule, delay: delay}
	}
	ag, _, err := buildAgentWith(task, offlineMaker(task, "reference"), 4, meter, slow)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, _, err := runLoop(context.Background(), ag, task)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted == "" {
		t.Fatal("цикл нічого не прийняв")
	}

	// Панель відпрацювала один раз (спроба 2), три критики по 150 мс.
	sequential := 3 * delay
	t.Logf("три критики по %s: панель паралельно %s, послідовно було б ≈%s",
		delay, elapsed.Round(time.Millisecond), sequential)
	if elapsed >= sequential {
		t.Errorf("час циклу %s ≥ послідовного %s — fan-out не паралельний", elapsed, sequential)
	}
}
