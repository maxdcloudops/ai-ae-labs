// Каркас тестів ДЗ 9. Усе, що не позначено t.Skip, проходить на стартері
// одразу, без мережі й ключа (fakellm — скриптована модель із labs/internal).
//
// «Немає тестових файлів» — найгірший результат: він виглядає як успіх.
// Тому нижче є три робочі тести й два скелети, які ви закриваєте в ДЗ.
//
//	go test ./...        # зелене, скіпи видно
//	go test -race ./...  # так це перевірятимуть при оцінюванні
//	LAB_LIVE=1 go test -run Live -v ./...   # справжня модель, якщо провайдер знайдено
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/errgroup"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week5/Day9_Agent_Memory_Event_Sourced_State/labs9/internal/memlab"
)

func testApp(t *testing.T) App {
	t.Helper()
	st, err := memlab.Open(filepath.Join(t.TempDir(), "memory.db"), memlab.IngestPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return App{Store: st, Policy: memlab.DefaultPolicy(), Meter: &memlab.Meter{}}
}

func turnOn(t *testing.T, a App, m *fakellm.Model, sid, text string) memlab.Turn {
	t.Helper()
	ag, err := newAgent(m, a)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(ag, a.Store, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := memlab.Say(context.Background(), r, user, sid, text)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

// TestRememberFactEmitsStateDelta — робочий тест: запис факту видно як
// StateDelta на події інструмента. Саме ця перевірка ловить помилку
// ctx.Session().State().Set (вона панікує або не лишає дельти): тест
// дивиться на подію, а не на внутрішню структуру.
func TestRememberFactEmitsStateDelta(t *testing.T) {
	a := testApp(t)
	m := fakellm.New("fake",
		fakellm.CallTurn("remember_fact", map[string]any{"key": "integration.A-114", "value": "legacy"}),
		fakellm.TextTurn("Записав."),
	)
	turn := turnOn(t, a, m, "s1", "A-114 сидить на legacy")
	var delta bool
	for _, e := range turn.Events {
		if _, ok := e.Actions.StateDelta[memlab.FactPrefix+"integration.A-114"]; ok {
			delta = true
		}
	}
	if !delta {
		t.Fatalf("жодної події зі StateDelta на %sintegration.A-114: %+v", memlab.FactPrefix, memlab.Audit(turn.Events))
	}
}

// TestFactReachesSecondSession — робочий тест, п.1 завдання: факт з однієї
// сесії потрапляє в промпт іншої (user: стан спільний для сесій користувача).
func TestFactReachesSecondSession(t *testing.T) {
	a := testApp(t)
	turnOn(t, a, fakellm.New("fake",
		fakellm.CallTurn("remember_fact", map[string]any{"key": "integration.A-114", "value": "legacy"}),
		fakellm.TextTurn("Записав."),
	), "s1", "A-114 сидить на legacy")

	m2 := fakellm.New("fake", fakellm.TextTurn("Радимо legacy-фікс."))
	turnOn(t, a, m2, "s2", "Що порадити мерчанту A-114?")
	var sys strings.Builder
	for _, p := range m2.Requests()[0].Config.SystemInstruction.Parts {
		sys.WriteString(p.Text)
	}
	if !strings.Contains(sys.String(), "integration.A-114 = legacy") {
		t.Fatalf("промпт другої сесії не містить факту:\n%s", sys.String())
	}
}

// TestRunCommandsOffline — команди CLI без мережі: say → facts.
func TestRunCommandsOffline(t *testing.T) {
	old := resolveModel
	resolveModel = func(context.Context, io.Writer) (model.LLM, string) {
		return memlab.Offline{Gullible: true}, "offline-rules (тест)"
	}
	t.Cleanup(func() { resolveModel = old })
	db := filepath.Join(t.TempDir(), "memory.db")
	var out bytes.Buffer
	for _, args := range [][]string{
		{"-db", db, "say", "s1", "A-114 сидить на legacy"},
		{"-db", db, "facts", "s1"},
		{"-db", db, "-offline", "say", "s1", "ще раз", "A-114", "на", "legacy"},
		{"-db", db, "poison"},
	} {
		if err := run(context.Background(), args, &out); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}
	if err := run(context.Background(), []string{"-db", db, "nope"}, &out); err == nil {
		t.Error("невідома команда має повертати помилку")
	}
}

// TestPoisonedNoteCannotRewriteFact — ДЗ 9, п.3. На стартері він FAIL-иться
// (наївний remember_fact пише все) — це і є інцидент. Зніміть Skip, коли
// почнете п.3, і зробіть його зеленим політикою з memlab.Policy.
//
// Каркас: сценарій як у TestRememberFactEmitsStateDelta, але модель спершу
// викликає fetch_partner_doc (a.Docs = {"A-114": InjectedDoc}), потім —
// remember_fact(integration.A-114=new-api). Перевірте в memlab.Audit(turn.Events),
// що є рядок Kind=="rejected", і що state_delta з FactPrefix немає.
func TestPoisonedNoteCannotRewriteFact(t *testing.T) {
	a := testApp(t)
	a.Docs = map[string]string{"A-114": InjectedDoc}

	// Крок 1: справжній користувач ставить факт. Без цього тест доводив би
	// лише «недовірений запис відхилено», а не «не може ПЕРЕПИСАТИ факт».
	turnOn(t, a, fakellm.New("fake",
		fakellm.CallTurn("remember_fact", map[string]any{"key": "integration.A-114", "value": "legacy"}),
		fakellm.TextTurn("Записав."),
	), "s1", "A-114 сидить на legacy")

	// Крок 2: атака. Модель читає нотатку порталу й робить рівно те, що в ній
	// написано — саме цей сценарій і має бути безпечним.
	turn := turnOn(t, a, fakellm.New("fake",
		fakellm.CallTurn("fetch_partner_doc", map[string]any{"merchant_id": "A-114"}),
		fakellm.CallTurn("remember_fact", map[string]any{"key": "integration.A-114", "value": "new-api"}),
		fakellm.TextTurn("Готово."),
	), "s2", "Подивись, що пишуть у порталі про A-114.")

	rows := memlab.Audit(turn.Events)
	var rejected bool
	for _, r := range rows {
		if r.Kind == "rejected" && r.Key == "integration.A-114" {
			rejected = true
		}
		// Відмова має бути саме відмовою, а не записом із позначкою.
		if r.Kind == "state_delta" && strings.HasPrefix(r.Key, memlab.FactPrefix) {
			t.Errorf("запис у пам'ять після зовнішнього тексту: %+v", r)
		}
	}
	if !rejected {
		t.Errorf("у журналі немає рядка rejected для integration.A-114: %+v", rows)
	}

	// Стан — остання інстанція: журнал міг би бути правильним, а значення
	// однаково перезаписаним.
	got, err := a.Store.Sessions.Get(context.Background(),
		&session.GetRequest{AppName: AppName, UserID: user, SessionID: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := got.Session.State().Get(memlab.FactPrefix + "integration.A-114")
	if err != nil {
		t.Fatalf("факт зник зі стану: %v", err)
	}
	f, ok := memlab.FactFromState(v)
	if !ok {
		t.Fatalf("факт не розбирається: %v", v)
	}
	if f.Value != "legacy" {
		t.Errorf("факт переписано: got %q, want %q", f.Value, "legacy")
	}
	if f.Tier != memlab.TierUser {
		t.Errorf("рівень довіри факту змінився: got %s, want %s", f.Tier, memlab.TierUser)
	}
}

// TestConcurrentWritesDifferentKeys — ++ Advanced (до 20 балів). ≥10 горутин
// пишуть у РІЗНІ ключі, жодне значення не загублено; запускати з -race. Межа,
// яку тест НЕ доводить і яку треба назвати в README: відсутність data race ≠
// вирішений конфлікт двох записів в ОДИН ключ (для лічильника потрібен atomic
// або транзакція).
func TestConcurrentWritesDifferentKeys(t *testing.T) {
	const workers = 12
	const maxAttempts = 50
	a := testApp(t)
	ctx := context.Background()
	const sid = "concurrent"
	if _, err := a.Store.Sessions.Create(ctx, &session.CreateRequest{
		AppName: AppName, UserID: user, SessionID: sid,
	}); err != nil {
		t.Fatal(err)
	}

	// retries counts how often the optimistic check actually fired. Without
	// this number the test could be passing because nothing ever collided.
	var retries atomic.Int64
	var g errgroup.Group
	for i := range workers {
		g.Go(func() error {
			key := fmt.Sprintf("user:worker:%d", i)
			want := fmt.Sprintf("value-%d", i)
			var last error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				// A FRESH handle per attempt, and never one shared between
				// goroutines: session/database is optimistically concurrent,
				// so a handle carries the update time it was read at. A shared
				// handle is both a data race and a guaranteed staleness.
				got, err := a.Store.Sessions.Get(ctx, &session.GetRequest{
					AppName: AppName, UserID: user, SessionID: sid,
				})
				if err != nil {
					return fmt.Errorf("worker %d: get: %w", i, err)
				}
				ev := session.NewEvent(ctx, fmt.Sprintf("inv-%d-%d", i, attempt))
				ev.Author = "test"
				ev.Actions.StateDelta = map[string]any{key: want}
				err = a.Store.Sessions.AppendEvent(ctx, got.Session, ev)
				if err == nil {
					return nil
				}
				// Matching the message is not a choice here: the service
				// exports no sentinel for this, and ADK's own test for it
				// matches the same string. Anything else would retry a real
				// failure (a closed database) forever.
				if !strings.Contains(err.Error(), "stale session error") {
					return fmt.Errorf("worker %d: append: %w", i, err)
				}
				retries.Add(1)
				last = err
			}
			return fmt.Errorf("worker %d: %d спроб вичерпано: %w", i, maxAttempts, last)
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d воркерів, %d повторів через stale session", workers, retries.Load())
	if retries.Load() == 0 {
		t.Log("попередження: жодного збігу — тест не перевірив оптимістичну конкурентність")
	}

	got, err := a.Store.Sessions.Get(ctx, &session.GetRequest{
		AppName: AppName, UserID: user, SessionID: sid,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := got.Session.State()
	for i := range workers {
		key := fmt.Sprintf("user:worker:%d", i)
		want := fmt.Sprintf("value-%d", i)
		v, err := st.Get(key)
		if err != nil {
			t.Errorf("%s: значення втрачено: %v", key, err)
			continue
		}
		if fmt.Sprint(v) != want {
			t.Errorf("%s = %v, want %v", key, v, want)
		}
	}
}

// TestLiveModelSavesFact — opt-in: справжня модель з modelcfg. Не перевіряє
// точний текст — лише те, що модель САМА викликала remember_fact і факт
// з'явився в стані.
func TestLiveModelSavesFact(t *testing.T) {
	if testing.Short() || strings.TrimSpace(os.Getenv("LAB_LIVE")) != "1" {
		t.Skip("LAB_LIVE != 1: live-тест вимкнено (LAB_LIVE=1 go test -run Live -v)")
	}
	_ = modelcfg.LoadEnv(".")
	m, choice, err := modelcfg.Load(context.Background())
	if err != nil {
		t.Skipf("провайдера не знайдено: %v", err)
	}
	t.Logf("модель: %s", choice.Reason)
	a := testApp(t)
	ag, err := newAgent(m, a)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(ag, a.Store, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := memlab.Say(context.Background(), r, user, "live", "Запам'ятай: мерчант A-114 сидить на legacy-інтеграції.")
	if err != nil {
		for _, s := range []string{"API_KEY_INVALID", "API key not valid", "insufficient_quota", "RESOURCE_EXHAUSTED", "401", "403"} {
			if strings.Contains(err.Error(), s) {
				t.Skipf("провайдер відхилив запит (%s) — перевірте ключ або квоту", s)
			}
		}
		t.Fatal(err)
	}
	for _, tool := range turn.Tools {
		if tool == "remember_fact" {
			return
		}
	}
	t.Fatalf("модель не викликала remember_fact; інструменти: %v, відповідь: %q", turn.Tools, turn.Text)
}

// TestStarterPiecesWithoutNetwork: дрібні шматки стартера, що не потребують
// моделі, — документ порталу, конфіг компакції, помилки CLI, вибір моделі без
// провайдера (має назвати причину, а не мовчки підмінити модель).
func TestStarterPiecesWithoutNetwork(t *testing.T) {
	got, err := App{Docs: map[string]string{"A-114": InjectedDoc}}.fetchPartnerDoc(nil, DocInput{MerchantID: "A-114"})
	if err != nil || !strings.Contains(got.Text, "SYSTEM NOTE") {
		t.Fatalf("fetchPartnerDoc = %q, %v", got.Text, err)
	}
	if got, _ := (App{}).fetchPartnerDoc(nil, DocInput{MerchantID: "Z-1"}); !strings.Contains(got.Text, "приміток немає") {
		t.Errorf("a merchant without a note: %q", got.Text)
	}
	if c := compactionConfig(500, 4, nil); c.TokenThreshold != 500 || c.EventRetentionSize != 4 {
		t.Errorf("compactionConfig = %+v", c)
	}

	var out bytes.Buffer
	for _, args := range [][]string{{}, {"nope"}, {"say", "s1"}, {"facts"}, {"-bogus"}} {
		if err := run(context.Background(), args, &out); err == nil {
			t.Errorf("run(%v) must fail with a named error", args)
		}
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-db", filepath.Join(blocker, "x", "m.db"), "facts", "s1"}, &out); err == nil {
		t.Error("an unusable database path must fail, not be ignored")
	}
	if err := run(context.Background(), []string{"-db", filepath.Join(t.TempDir(), "m.db"), "-offline", "facts", "missing"}, &out); err == nil {
		t.Error("facts of a session that does not exist must fail")
	}

	for _, k := range []string{"MODEL", "DEFAULT_MODEL_PROVIDER", "GOOGLE_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OLLAMA_HOST"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
	out.Reset()
	m, note := pickModel(context.Background(), &out)
	if _, _, err := modelcfg.Load(context.Background()); err != nil &&
		(m == nil || !strings.HasPrefix(note, "offline-rules") || !strings.Contains(out.String(), "провайдера не знайдено")) {
		t.Errorf("no provider must be reported with its cause: note=%q out=%q", note, out.String())
	}
}

// failingState is a session.State whose writes are refused — the storage
// failing under remember_fact. A refusal must surface as a named error.
//
// Get is implemented too, and that is not decoration. The starter version of
// rememberFact never read state, so an embedded nil session.State was enough;
// the п.3 version reads the current fact first (that is what makes a conflict
// decidable), and a nil embedded interface would panic instead of failing the
// assertion. Returning an error is the honest stub: it is what the service
// answers for a key that is not there.
type failingState struct{ session.State }

func (failingState) Get(string) (any, error) { return nil, errors.New("no such key") }

func (failingState) Set(string, any) error { return errors.New("disk full") }

type failingCtx struct{ agent.Context }

func (failingCtx) State() session.State { return failingState{} }

func TestRememberFactNamesAStorageFailure(t *testing.T) {
	// A real policy, not App{}: with a zero Policy the key allowlist is empty,
	// every key is rejected, and the write would never be attempted — the test
	// would pass for the wrong reason.
	app := App{Policy: memlab.DefaultPolicy()}
	_, err := app.rememberFact(failingCtx{}, FactInput{Key: "integration.A-114", Value: "legacy"})
	if err == nil || !strings.Contains(err.Error(), "remember_fact") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want it to name remember_fact and the cause", err)
	}
}
