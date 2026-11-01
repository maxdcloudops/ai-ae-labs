package main

import (
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
)

// Desk names. They are the graph node names, the ledger's section keys and the
// -fail flag's values, so they are declared once.
const (
	DeskRisk        = "risk_desk"
	DeskCompliance  = "compliance_desk"
	DeskIntegration = "integration_desk"
)

// errDeskUnavailable is the sentinel every source outage wraps. A caller
// branches on it with errors.Is; the Code below says which outage it was.
var errDeskUnavailable = errors.New("джерело не відповіло")

// deskError carries a machine-readable code next to the sentinel.
//
// The code exists because "integration_desk failed" is not actionable at 3am.
// INTEGRATION_DESK_TIMEOUT tells the on-call which system to look at, and it
// is what lands in the section, the evidence block and the ledger row.
type deskError struct {
	Code string
	Why  string
}

func (e *deskError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Why) }
func (e *deskError) Unwrap() error { return errDeskUnavailable }

// desk is one specialist: its own source, its own tool, its own model.
//
// "Its own" is the point of п.1. Three desks rather than one agent with three
// tools because the three differ in all three axes that matter: the domain
// (risk / regulation / merchants), the source that can fail independently, and
// the context each needs. One agent would carry all three contexts into every
// call and would lose the per-source failure boundary — one dead source would
// degrade the whole answer instead of one section.
type desk struct {
	Name     string
	Title    string
	ToolName string
	ToolDesc string
	// Fetch is the source. It is a plain function so a test can make it fail
	// without a model, a network or a graph.
	Fetch func() ([]Finding, error)
	// Prose is what the offline fake says after calling the tool. With a real
	// provider the model writes this itself and Prose is ignored.
	Prose string
}

// Agent builds the specialist. The instruction is deliberately narrow: call
// the tool first, answer only with what it returned, cite every fact.
//
// A specialist that may answer from its own knowledge is a specialist that can
// fill a dead source with something plausible — which is the exact failure п.6
// asks us to make impossible.
func (d desk) Agent(m model.LLM, role Role) (agent.Agent, error) {
	t, err := functiontool.New(functiontool.Config{
		Name:        d.ToolName,
		Description: d.ToolDesc,
	}, d.toolFn)
	if err != nil {
		return nil, fmt.Errorf("%s: інструмент: %w", d.Name, err)
	}
	return llmagent.New(llmagent.Config{
		Name:        d.Name + "_agent",
		Model:       m,
		Description: d.Title,
		Instruction: "Ти — " + d.Title + " у LEDGERWORKS.\n" +
			"1. ОБОВ'ЯЗКОВО спершу виклич " + d.ToolName + ". Без його відповіді не пиши нічого.\n" +
			"2. Говори ЛИШЕ те, що повернув інструмент. Нічого не додавай від себе.\n" +
			"3. Кожен факт подавай із джерелом у дужках: «твердження (джерело)».\n" +
			"4. Якщо інструмент повернув помилку — так і скажи, назви код помилки " +
			"і не вигадуй, що там могло б бути.\n" +
			"Відповідай українською, не більше трьох речень.",
		Tools: []tool.Tool{t},
		// The policy point. It covers the model's path only — the real
		// boundary is `may`, also called in Engine.branch. See policy.go.
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{policyGate(role)},
		// Single turn: the desk answers once and its history does not grow
		// into the coordinator's context.
		Mode: llmagent.ModeSingleTurn,
	})
}

// deskInput is every desk tool's argument shape.
type deskInput struct {
	Topic string `json:"topic" jsonschema:"Тема, за якою потрібні факти"`
}

// deskOutput is every desk tool's result shape.
type deskOutput struct {
	Findings []Finding `json:"findings"`
}

func (d desk) toolFn(_ agent.Context, _ deskInput) (deskOutput, error) {
	f, err := d.Fetch()
	if err != nil {
		return deskOutput{}, err
	}
	return deskOutput{Findings: f}, nil
}

// OfflineModel is the scripted stand-in: call the tool, then report.
//
// A fresh instance per run: fakellm replays an ordered script, so a shared
// instance would be exhausted by the second run and the three concurrent
// branches would steal each other's turns.
func (d desk) OfflineModel() model.LLM {
	return fakellm.New("offline-"+d.Name,
		fakellm.CallTurn(d.ToolName, map[string]any{"topic": "міграція з legacy-процесора"}),
		fakellm.TextTurn(d.Prose),
	)
}

// desks returns the three specialists. failing names the desks whose source is
// down for this run — that is how п.6 is exercised without touching the graph.
func desks(failing map[string]bool) []desk {
	return []desk{
		{
			Name: DeskRisk, Title: "спеціаліст з ризиків міграції",
			ToolName: "lookup_risk_desk",
			ToolDesc: "Факти про ризики міграції з бази інцидентів LEDGERWORKS.",
			Fetch:    guard(DeskRisk, "RISK_DESK_TIMEOUT", "incident-db не відповідає", failing, fetchRisk),
			Prose:    "Головний ризик — розсинхрон токенів на cutover (incident-db:INC-418); ранбук відкоту давно не перевіряли (runbook:cutover-rollback).",
		},
		{
			Name: DeskCompliance, Title: "спеціаліст з комплаєнсу PCI DSS і вимог НБУ",
			ToolName: "lookup_compliance_desk",
			ToolDesc: "Вимоги PCI DSS і НБУ, застосовні до міграції процесора.",
			Fetch:    guard(DeskCompliance, "COMPLIANCE_DESK_TIMEOUT", "реєстр вимог недоступний", failing, fetchCompliance),
			Prose:    "Потрібне повторне засвідчення PCI DSS 4.0 після зміни процесора (pci-dss:4.0/12.8.2) і повідомлення НБУ за 30 днів (nbu:2021-104/р.5).",
		},
		{
			Name: DeskIntegration, Title: "спеціаліст з мерчантів на legacy-інтеграції",
			ToolName: "lookup_integration_desk",
			ToolDesc: "Мерчанти на legacy-інтеграції та стан їхньої міграції.",
			Fetch:    guard(DeskIntegration, "INTEGRATION_DESK_TIMEOUT", "merchant-api не відповідає", failing, fetchIntegration),
			Prose:    "На legacy лишаються 12 мерчантів, з них A-114 просив відкласти до Q4 (merchant-api:A-114).",
		},
	}
}

// guard wraps a fixture so the -fail flag can take its source down.
func guard(name, code, why string, failing map[string]bool, fn func() ([]Finding, error)) func() ([]Finding, error) {
	return func() ([]Finding, error) {
		if failing[name] {
			return nil, &deskError{Code: code, Why: why}
		}
		return fn()
	}
}

// --- Фікстури джерел -------------------------------------------------------
//
// Кожен факт має ідентифікатор джерела. Справжня система ходила б у
// incident-db, реєстр вимог і merchant-api; форма даних та сама.

func fetchRisk() ([]Finding, error) {
	return []Finding{
		{Claim: "Cutover 2025-03 відкотили за 41 хв через розсинхрон токенів", Source: "incident-db:INC-418"},
		{Claim: "Rollback-ранбук для A-114 перевірено востаннє у вересні", Source: "runbook:cutover-rollback"},
	}, nil
}

func fetchCompliance() ([]Finding, error) {
	return []Finding{
		{Claim: "Зміна процесора вимагає повторного засвідчення PCI DSS 4.0", Source: "pci-dss:4.0/12.8.2"},
		{Claim: "НБУ треба повідомити за 30 днів до зміни процесора", Source: "nbu:2021-104/р.5"},
	}, nil
}

func fetchIntegration() ([]Finding, error) {
	return []Finding{
		{Claim: "На legacy-інтеграції лишаються 12 мерчантів", Source: "merchant-api:legacy-list"},
		{Claim: "A-114 просив відкласти міграцію до Q4", Source: "merchant-api:A-114"},
	}, nil
}
