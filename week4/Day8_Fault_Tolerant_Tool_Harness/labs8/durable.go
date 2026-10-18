// Durable-шар ДЗ 8: той самий агент, але його цикл живе у Temporal Workflow.
//
// Різниця з main.go не в логіці, а в тому, ХТО тримає стан. У процесі
// стан живе в пам'яті: впав процес — зник хід. У Temporal стан — це історія
// подій на сервері, і воркер після рестарту відтворює її замість того, щоб
// починати заново. Саме це перевіряє доказ crash-safe: лічильник викликів
// моделі після `kill -9` не має зрости.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/agentdb"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

const (
	// taskQueue — черга, яку слухає воркер і в яку `start` кладе хід.
	taskQueue = "labs8"
	// modelName — ім'я, за яким воркер відновлює справжню модель.
	// У workflow їде ТІЛЬКИ рядок: ключі не перетинають межу Activity.
	modelName = "labs8-model"
	// semanticErrorType — тип помилки, який Temporal не повторює.
	semanticErrorType = "SemanticError"
)

// WorkflowInput — усе, що потрібно ходу.
type WorkflowInput struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// OpenRefundCase — Activity з побічним ефектом.
//
// Чому саме Activity, а не функція в workflow: workflow має бути
// детермінованим і відтворюваним, а запис кейса — ні. Усе, що торкається
// мережі, диска чи годинника, Temporal вимагає виносити за межу — і взамін
// дає повтор, таймаут і видимість у UI.
//
// Семантична помилка повертається як ApplicationError із типом
// "SemanticError". Цей рядок перелічено в NonRetryableErrorTypes політики
// нижче, тож рушій її НЕ повторює: неправильний аргумент не стає правильним
// від повтору. Транзієнтна йде звичайною помилкою — її рушій повторить сам.
// Функція пакета, а не метод: ActivityAsTool вимагає рівно
// (context.Context, TArgs), а метод із ресивером дає три аргументи й
// відхиляється при збірці графа. Стан воркера живе в activities — його
// встановлює runWorker перед стартом.
func OpenRefundCase(ctx context.Context, in ledger.RefundCaseInput) (RefundResult, error) {
	a := activities
	if a == nil || a.desk == nil {
		return RefundResult{}, errors.New("worker не ініціалізований")
	}
	out, err := a.desk.OpenRefundCase(ctx, in)
	if err == nil {
		return RefundResult{Status: out.Status, CaseID: out.CaseID, Replayed: out.Replayed}, nil
	}

	class, msg := classifyError(err)
	switch class {
	case ClassSurfaceToModel:
		var se *ledger.SemanticError
		field := ""
		if errors.As(err, &se) {
			field = se.Field
		}
		// NonRetryable: рушій віддасть цю помилку workflow одразу, без
		// жодного повтору. Workflow перетворить її на Observation.
		return RefundResult{}, temporal.NewApplicationError(msg, semanticErrorType, field)
	default:
		// Транзієнтна або невідома — звичайна помилка, рушій повторює за
		// RetryPolicy. Класифікацію робимо тут, а не в workflow, бо саме
		// тут видно оригінальний тип помилки.
		return RefundResult{}, err
	}
}

// RememberSession — Activity, яка віддає сесію в пам'ять.
//
// Теж Activity: запис у Dgraph — це мережа, а workflow мусить лишатися
// детермінованим.
func RememberSession(ctx context.Context, in WorkflowInput) error {
	a := activities
	if a == nil || a.db == nil {
		return nil
	}
	resp, err := a.db.Sessions().Get(ctx, &session.GetRequest{
		AppName: appName, UserID: in.UserID, SessionID: in.SessionID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		// Відсутня сесія — не привід валити хід: пам'ять це покращення,
		// а не умова відповіді.
		return nil
	}
	if a.memory == nil {
		return nil
	}
	return a.memory.AddSessionToMemory(ctx, resp.Session)
}

// appName — ім'я застосунку для сесій ADK.
const appName = "ledgerworks"

// AgentWorkflow — цикл агента всередині Temporal.
//
// Виглядає як звичайний ADK-код, і це головна цінність інтеграції: агент не
// переписується. Змінюються рівно дві речі — модель стає
// googleadk.NewModel (виклик їде в Activity), а контекст іде через
// googleadk.NewContext (детерміновані час і UUID).
func AgentWorkflow(ctx workflow.Context, in WorkflowInput) (string, error) {
	// Політика повторів — на стороні рушія, а не в нашому коді. Саме вона
	// замінила `retry` зі стенд-іна: цикл for живе на сервері й переживає
	// падіння воркера.
	refundOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumAttempts:    4,
			// Ось де класифікація стає контрактом рушія, а не нашим if-ом.
			NonRetryableErrorTypes: []string{semanticErrorType},
		},
	}

	// Бюджет самокорекції живе у workflow: він детермінований (виводиться з
	// тієї самої історії подій), тому переживає replay без розбіжностей.
	b := &budget{}

	// Інструмент — звичайний functiontool, а не ActivityAsTool. Різниця
	// принципова: ActivityAsTool віддав би помилку Activity прямо моделі як
	// помилку інструмента, і семантичний збій перестав би бути Observation.
	// Нам потрібна точка перетворення — ось вона.
	refund, err := functiontool.New(functiontool.Config{
		Name: "open_refund_case",
		Description: "Opens a refund case for one transaction of one merchant. Needs transaction_id and merchant_id " +
			"exactly as the user wrote them. May return status invalid_argument with an observation: read it, fix the named field, call again.",
	}, func(actx agent.Context, in ledger.RefundCaseInput) (RefundResult, error) {
		wctx, ok := googleadk.WorkflowContext(actx)
		if !ok {
			return RefundResult{}, errors.New("tool виконується поза workflow")
		}
		var out RefundResult
		// Повтори робить РУШІЙ за політикою вище, а не наш цикл for.
		// Транзієнтний збій він повторить сам; семантичний — ні, бо його
		// тип перелічено в NonRetryableErrorTypes.
		err := workflow.ExecuteActivity(workflow.WithActivityOptions(wctx, refundOpts),
			OpenRefundCase, in).Get(wctx, &out)
		if err == nil {
			return out, nil
		}
		obs, ferr := b.observe(unwrapActivityError(err))
		if ferr != nil {
			return RefundResult{}, ferr
		}
		return RefundResult{Status: obs.Status, Field: obs.Field, Observation: obs.Observation, Attempt: obs.Attempt}, nil
	})
	if err != nil {
		return "", fmt.Errorf("build refund tool: %w", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name: "refund_agent", Description: "Opens refund cases for LEDGERWORKS.",
		Model: googleadk.NewModel(modelName), Instruction: instruction,
		Tools: []tool.Tool{refund},
	})
	if err != nil {
		return "", err
	}
	r, err := runner.New(runner.Config{
		AppName: appName, Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return "", err
	}

	adkCtx := googleadk.NewContext(ctx)
	msg := genai.NewContentFromText(in.Message, genai.RoleUser)

	var answer string
	for ev, err := range r.Run(adkCtx, in.UserID, in.SessionID, msg, agent.RunConfig{}) {
		if err != nil {
			return "", err
		}
		if ev != nil && ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p != nil && p.Text != "" {
					answer = p.Text
				}
			}
		}
	}

	// Пам'ять пишеться наприкінці ходу й теж через Activity.
	memCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
	})
	if err := workflow.ExecuteActivity(memCtx, RememberSession, in).Get(ctx, nil); err != nil {
		// Пам'ять не критична для відповіді: логуємо й віддаємо результат.
		workflow.GetLogger(ctx).Warn("remember session failed", "err", err)
	}
	return answer, nil
}

// activities — стан воркера. Пакетна змінна тут виправдана: Activity
// виконується ТІЛЬКИ в процесі воркера, і він один на процес. Workflow цю
// змінну не бачить узагалі — він оперує іменами Activity, і саме тому
// ключі та зʼєднання не перетинають межу детермінізму.
var activities *Activities

// unwrapActivityError дістає з обгорток Temporal помилку, яку розуміє
// classifyError.
//
// Рушій загортає помилку Activity в ApplicationError із типом — а наші
// сентинели (ledger.ErrSemantic) при цьому не переживають серіалізацію.
// Тому класифікуємо за типом, який Activity сама й поставила.
func unwrapActivityError(err error) error {
	var app *temporal.ApplicationError
	if errors.As(err, &app) && app.Type() == semanticErrorType {
		field := ""
		var s string
		if derr := app.Details(&s); derr == nil {
			field = s
		}
		return &ledger.SemanticError{Field: field, Reason: app.Message()}
	}
	// Усе інше — транзієнтне, бо рушій уже вичерпав повтори.
	return fmt.Errorf("%w: %v", ledger.ErrTransient, err)
}

// Activities тримає все, що живе на стороні воркера: стіл возвратів, база
// й пам'ять. Workflow їх не бачить — він бачить лише імена Activity.
type Activities struct {
	desk   *ledger.Desk
	db     *agentdb.DB
	memory interface {
		AddSessionToMemory(ctx context.Context, s session.Session) error
	}
}
