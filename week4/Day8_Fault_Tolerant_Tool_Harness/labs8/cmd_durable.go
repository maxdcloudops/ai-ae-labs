package main

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/worker"

	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/agentdb"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// dbPath — файл SQLite з сесіями, кейсами й лічильником викликів моделі.
//
// Лічильник живе ПОЗА процесом навмисно: у цьому весь доказ crash-safe.
// Якби він був у пам'яті, `kill -9` обнулив би його разом із процесом, і
// порівнювати було б нічого.
func dbPath() string {
	if p := os.Getenv("LABS8_DB"); p != "" {
		return p
	}
	return filepath.Join(".data", "agent.db")
}

// openDB відкриває SQLite, створюючи теку за потреби.
func openDB() (*agentdb.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath()), 0o755); err != nil {
		return nil, err
	}
	return agentdb.Open(dbPath())
}

// runWorker піднімає воркер: саме він тримає справжню модель, стіл возвратів
// і базу. Workflow бачить лише імена — ключі не перетинають межу Activity.
func runWorker(ctx context.Context, faults *ledger.FaultPlan) error {
	db, err := openDB()
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath(), err)
	}
	defer db.Close()

	m, err := pickModel(ctx)
	if err != nil {
		return err
	}
	// countingModel рахує КОЖЕН виклик моделі в SQLite. Це і є той лічильник,
	// який після kill -9 не має зрости: Temporal відтворює історію замість
	// повторного звернення до моделі.
	counted := &countingModel{LLM: m, db: db}

	// LEDGER_LATENCY робить виклик інструмента повільним, щоб його встигли
	// вбити посеред роботи — це і є стенд для доказу crash-safe.
	var latency time.Duration
	if v := os.Getenv("LEDGER_LATENCY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("LEDGER_LATENCY=%q: %w", v, err)
		}
		latency = d
	}
	activities = &Activities{desk: &ledger.Desk{Cases: db.Cases(), Faults: faults, Latency: latency}, db: db}

	plugin, err := googleadk.NewPlugin(googleadk.Config{
		Models: map[string]googleadk.ModelFactory{
			modelName: func(context.Context, string) (model.LLM, error) { return counted, nil },
		},
	})
	if err != nil {
		return err
	}

	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{Plugins: []worker.Plugin{plugin}})
	w.RegisterWorkflow(AgentWorkflow)
	w.RegisterActivity(OpenRefundCase)
	w.RegisterActivity(RememberSession)

	fmt.Printf("worker: слухає %s, база %s\n", taskQueue, dbPath())
	return w.Run(worker.InterruptCh())
}

// runStart кладе хід у чергу й чекає результату.
func runStart(ctx context.Context, sessionID, message string) error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()

	// WorkflowID = сесія: повторний `start` із тим самим -session продовжує
	// ту саму розмову, а не починає нову.
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "labs8-" + sessionID + "-" + fmt.Sprint(time.Now().UnixNano()),
		TaskQueue: taskQueue,
	}, AgentWorkflow, WorkflowInput{UserID: "taras", SessionID: sessionID, Message: message})
	if err != nil {
		return err
	}
	fmt.Printf("[ev:started] workflow=%s run=%s\n", run.GetID(), run.GetRunID())

	var answer string
	if err := run.Get(ctx, &answer); err != nil {
		return fmt.Errorf("[ev:failed] %w", err)
	}
	fmt.Printf("[ev:success] %s\n", answer)
	return nil
}

// runCounter друкує лічильник викликів моделі з SQLite.
func runCounter(ctx context.Context) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	calls, err := db.CountLLMCalls(ctx)
	if err != nil {
		return err
	}
	cases, err := db.CountCases(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("llm_calls=%d cases=%d db=%s\n", calls, cases, dbPath())
	return nil
}

// dial підключається до Temporal, пояснюючи, що робити, якщо сервера немає.
func dial() (client.Client, error) {
	c, err := client.Dial(client.Options{HostPort: temporalAddr()})
	if err != nil {
		return nil, fmt.Errorf("temporal at %s (is `temporal server start-dev` running?): %w", temporalAddr(), err)
	}
	return c, nil
}

func temporalAddr() string {
	if a := os.Getenv("TEMPORAL_ADDRESS"); a != "" {
		return a
	}
	return "localhost:7233"
}

// countingModel — обгортка, що рахує виклики моделі в SQLite.
//
// Лічильник інкрементується ПЕРЕД викликом: нас цікавить «скільки разів ми
// пішли до моделі», а не «скільки разів вона відповіла». Падіння посеред
// виклику має бути враховане, інакше доказ crash-safe рахував би не те.
type countingModel struct {
	model.LLM
	db *agentdb.DB
}

func (c *countingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if err := c.db.RecordLLMCall(ctx, c.LLM.Name()); err != nil {
		log.Printf("counter: %v", err)
	}
	return c.LLM.GenerateContent(ctx, req, stream)
}

// parseFaults читає список збоїв із рядка.
func parseFaults(s string) (*ledger.FaultPlan, error) {
	faults := &ledger.FaultPlan{}
	for _, f := range strings.Split(s, ",") {
		switch strings.TrimSpace(f) {
		case "":
		case "503":
			faults.Push(ledger.Unavailable)
		case "timeout":
			faults.Push(ledger.Timeout)
		case "429":
			faults.Push(ledger.RateLimited)
		default:
			return nil, fmt.Errorf("unknown fault %q (want 503, timeout, 429)", f)
		}
	}
	return faults, nil
}
