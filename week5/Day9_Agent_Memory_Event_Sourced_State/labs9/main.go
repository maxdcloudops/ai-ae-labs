// Стартовий шаблон ДЗ 9 — станом на ADK Go v2.6.0 (жовтень 2026), перевірте актуальність API.
//
// Ранній win (≤15 хв), усе з каталогу labs9:
//
//	go run . say day1 "A-114 сидить на legacy-інтеграції, міграцію відклали до Q4."
//	go run . facts day1            # факти user: і журнал змін стану
//	go run . say day2 "Що порадити мерчанту A-114?"   # інша сесія, інший процес
//	go run . poison                # інцидент: нотатка порталу переписує факт
//
// Ключ можна не експортувати: apps/.env у корені репозиторію підхопиться сам
// (шаблон — apps/.env-example), явний export завжди має пріоритет. Провайдера й
// модель задають DEFAULT_MODEL_PROVIDER і MODEL — той самий вибір, що в лабах
// Тижня 1 (internal/modelcfg). Без провайдера працює правилова offline-модель, і
// про це сказано в першому рядку виводу.
//
// Повний розв'язок із компакцією та OKF — частина вашого завдання (дивіться
// TODO в agent.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"

	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week5/Day9_Agent_Memory_Event_Sourced_State/labs9/internal/memlab"
)

// user is the support lead whose memory the lab builds. It is the default of
// the -user flag: durable facts live in "user:" state, so the SAME database
// holds a separate memory per user — which is what the -user contrast in п.1
// demonstrates.
const user = "iryna"

// resolveModel is a variable so a test can swap the network for a scripted model.
var resolveModel = pickModel

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

// pickModel resolves the course model; with no provider it says why and falls
// back to the offline rules.
func pickModel(ctx context.Context, w io.Writer) (model.LLM, string) {
	if err := modelcfg.LoadEnv("."); err != nil {
		fmt.Fprintf(w, "попередження: %v\n", err)
	}
	m, choice, err := modelcfg.Load(ctx)
	if err != nil {
		fmt.Fprintf(w, "провайдера не знайдено (%v) — працює offline-rules; див. Homework, «Якщо щось не працює»\n", err)
		return memlab.Offline{Gullible: true}, "offline-rules (немає провайдера)"
	}
	return m, choice.Reason
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("labs9", flag.ContinueOnError)
	fs.SetOutput(out)
	dbPath := fs.String("db", memlab.DefaultPath, "SQLite-файл сесій і пам'яті")
	threshold := fs.Int("threshold", 0, "поріг токенів для компакції (tail retention); 0 — вимкнено")
	offline := fs.Bool("offline", false, "не шукати провайдера, використати правилову модель")
	uid := fs.String("user", user, "користувач, якому належить пам'ять (той самий файл БД, інша пам'ять)")
	// The poison command pins the attack to its own session; this flag puts the
	// same note behind fetch_partner_doc in ANY session, which is what the
	// audit fragment of п.4 needs: one journal holding a write, an update and
	// a refusal.
	portalNote := fs.Bool("portal-note", false, "підкласти заражену примітку порталу в fetch_partner_doc цієї сесії")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("команда: say <сесія> <текст> | facts <сесія> | audit <сесія> | poison")
	}
	m, note := resolveModel(ctx, out)
	if *offline {
		m, note = memlab.Offline{Gullible: true}, "offline-rules (прапорець -offline)"
	}
	fmt.Fprintf(out, "модель: %s\n", note)

	st, err := memlab.Open(*dbPath, memlab.IngestPolicy{})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	app := App{Store: st, Policy: memlab.DefaultPolicy(), Meter: &memlab.Meter{}, Prompts: &PromptLog{}}
	if *portalNote {
		app.Docs = map[string]string{"A-114": InjectedDoc}
	}

	switch rest[0] {
	case "say":
		if len(rest) < 3 {
			return errors.New("say <сесія> <текст>")
		}
		return say(ctx, out, m, app, *threshold, note, *uid, rest[1], strings.Join(rest[2:], " "))
	case "facts":
		if len(rest) != 2 {
			return errors.New("facts <сесія>")
		}
		return facts(ctx, out, app, *uid, rest[1])
	case "audit":
		if len(rest) != 2 {
			return errors.New("audit <сесія>")
		}
		return auditLog(ctx, out, app, *uid, rest[1])
	case "poison":
		app.Docs = map[string]string{"A-114": InjectedDoc}
		if err := say(ctx, out, m, app, *threshold, note, *uid, "poison", "Подивись, що пишуть у порталі про A-114."); err != nil {
			return err
		}
		return facts(ctx, out, app, *uid, "poison")
	default:
		return fmt.Errorf("невідома команда %q", rest[0])
	}
}

// say runs one turn and prints the model answer plus the state changes the
// turn left in the event log.
func say(ctx context.Context, out io.Writer, m model.LLM, app App, threshold int, note, uid, sid, text string) error {
	ag, err := newAgent(m, app)
	if err != nil {
		return err
	}
	var cfg *compaction.Config
	if threshold > 0 {
		cfg = compactionConfig(threshold, 4, nil)
	}
	r, err := newRunner(ag, app.Store, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[%s] > %s\n", sid, text)
	t, err := memlab.Say(ctx, r, uid, sid, text)
	if err != nil {
		return err
	}
	for _, row := range memlab.Audit(t.Events) {
		fmt.Fprintf(out, "      [ev:%s] %s %s\n", row.Kind, row.Key, row.Detail)
	}
	fmt.Fprintf(out, "[%s] < %s\n", sid, t.Text)
	if t.CompactionErr != nil {
		fmt.Fprintf(out, "      компакція не вдалася: %v\n", t.CompactionErr)
	}
	tokens(out, app, note)
	return nil
}

// offlineModel reports whether the numbers came from the rules model rather
// than a provider. memlab.Offline fills UsageMetadata with synthetic,
// size-proportional counts (output is the constant 12) precisely so the table
// has numbers without a key — which makes labelling them mandatory.
func offlineModel(note string) bool { return strings.HasPrefix(note, "offline-rules") }

// source names who produced a token count: the provider's own model string
// when there is one, otherwise the resolved model note.
func source(note, modelVersion string) string {
	if strings.TrimSpace(modelVersion) != "" {
		return modelVersion
	}
	return note
}

// tokens prints the per-call component split of п.5.
//
// The two sources are kept in separate columns on purpose. Our estimate is a
// ruler we control and can apply to a part of the prompt; UsageMetadata is the
// only number the provider will bill. Adding them, or presenting an estimate as
// a measurement, is the mistake the column heading exists to prevent.
func tokens(out io.Writer, app App, note string) {
	if app.Prompts == nil {
		return
	}
	breakdowns := app.Prompts.Calls()
	if len(breakdowns) == 0 {
		return
	}
	calls := app.Meter.Calls()
	for i, b := range breakdowns {
		fmt.Fprintf(out, "      виклик %d: інструкція %d + факти %d + преамбула memory-tools %d + історія %d = %d токенів (наша оцінка)\n",
			i+1, b.Instruction, b.Facts, b.Other, b.History, b.Total())
		if i >= len(calls) || !calls[i].ReportedUsage {
			fmt.Fprintf(out, "                 UsageMetadata не повернуто — порівнювати з оцінкою нема з чим\n")
			continue
		}
		fmt.Fprintf(out, "                 UsageMetadata (%s): prompt %d, output %d, total %d\n",
			source(note, calls[i].Model), calls[i].PromptTokens, calls[i].OutputTokens, calls[i].TotalTokens)
		if offlineModel(note) {
			fmt.Fprintf(out, "                 ↑ синтетика offline-моделі (output завжди 12), а НЕ вимір провайдера\n")
		}
	}
}

// facts prints the durable user: facts visible from a session.
func facts(ctx context.Context, out io.Writer, app App, uid, sid string) error {
	got, err := app.Store.Sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: uid, SessionID: sid})
	if err != nil {
		return fmt.Errorf("facts: %w", err)
	}
	fs := memlab.FactsFromState(got.Session.State(), app.Policy)
	fmt.Fprintf(out, "      факти user: %s (%d)\n", uid, len(fs))
	for _, f := range fs {
		stale := ""
		if f.Stale {
			stale = "  [ЗАСТАРІЛО]"
		}
		fmt.Fprintf(out, "        %s = %s  [%s]%s\n", f.Key, f.Fact.Value, f.Fact.Tier, stale)
	}
	return nil
}

// auditLog writes audit_<user>_<session>.json from the STORED events.
//
// Stored, not Turn.Events: tail-retention compaction appends its summary
// directly to the session, so a compaction row exists only in the persisted
// stream. The file is derived from events every time — there is no second log
// to drift out of sync with the first.
func auditLog(ctx context.Context, out io.Writer, app App, uid, sid string) error {
	events, err := memlab.StoredEvents(ctx, app.Store.Sessions, AppName, uid, sid)
	if err != nil {
		return err
	}
	rows := memlab.Audit(events)
	path := fmt.Sprintf("audit_%s_%s.json", uid, sid)
	if err := memlab.WriteAudit(path, rows); err != nil {
		return err
	}
	fmt.Fprintf(out, "      журнал: %s (%d рядків з %d подій)\n", path, len(rows), len(events))
	for _, r := range rows {
		fmt.Fprintf(out, "        %2d %s  %-11s %s %s\n",
			r.Seq, r.At.Format("15:04:05"), r.Kind, r.Key, r.Detail)
	}
	return nil
}
