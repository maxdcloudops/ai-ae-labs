// ВИ: той самий цикл у workflow-графі ADK. Крок — вузол (NewFunctionNode),
// цикл — динамічний вузол (NewDynamicNode), що викликає крок через RunNode.
// Еталон API: sources/github/adk-go/examples/workflow/dynamic/basic/main.go.
package main

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week4/Day7_ReAct_Loop_Internals/labs7/internal/react"
)

// StepResult — те, що вузол-крок пише в event log.
type StepResult struct {
	Iteration int    `json:"iteration"`
	Thought   string `json:"thought"`
	Calls     int    `json:"calls"`
	Final     bool   `json:"final"`
}

// NewGraphAgent збирає агента Start → react_loop.
//
// Та сама логіка, що в react.go, але крок — вузол графа, а цикл — динамічний
// вузол. Різниця не в поведінці, а в тому, що кожна ітерація лишає подію в
// event log: мануальний цикл треба інструментувати самому, графовий
// інструментований за побудовою.
//
// Ціна цього — стан. `StepResult` їде в лог і має бути серіалізовним, тож
// повний `react.Step` (із Calls і genai.Content усередині) у нього не
// покладеш. Крок пише результат у `last`, а цикл читає його звідти. Це
// прихована зв'язність між вузлами, якої в мануальній версії немає, і саме
// вона — головна практична відмінність двох реалізацій.
func NewGraphAgent(p react.Planner, reg react.Registry, b react.Budget) (agent.Agent, error) {
	b = b.Normalized()
	history := react.Seed("")
	// last — міст між вузлом-кроком і циклом. Один агент = один прогін:
	// workflowagent не ділить цей стан між паралельними інвокаціями, і для
	// кількох одночасних запитів потрібен агент на запит.
	var last react.Step

	step := workflow.NewFunctionNode("plan_step",
		func(ctx agent.Context, turn int) (StepResult, error) {
			s, err := p.NextStep(ctx, history)
			if err != nil {
				return StepResult{}, err
			}
			last = s
			return StepResult{Iteration: turn, Thought: s.Thought, Calls: len(s.Calls), Final: s.Final}, nil
		}, workflow.NodeConfig{})

	loop := workflow.NewDynamicNode[string, string]("react_loop",
		func(ctx agent.Context, task string, _ func(*session.Event) error) (string, error) {
			history = react.Seed(task)
			for i := 1; ; i++ {
				// Той самий текст і той самий сентинел, що в react.go:
				// тести перевіряють обидві версії через errors.Is, і
				// розбіжність тут означала б, що «еквівалентні» реалізації
				// насправді відрізняються в єдиному місці, яке ловить збій.
				if i > b.MaxIterations {
					return "", fmt.Errorf("вичерпано ліміт %d ітерацій: %w", b.MaxIterations, ErrMaxIterations)
				}
				r, err := workflow.RunNode[StepResult](ctx, step, i)
				if err != nil {
					return "", fmt.Errorf("ітерація %d: %w", i, err)
				}
				if r.Final {
					// Answer, а не Thought: думка — це міркування вголос,
					// відповідь — результат. Повернути Thought означало б
					// віддати користувачу чернетку.
					return last.Answer, nil
				}
				obs := make([]react.Observation, 0, len(last.Calls))
				for _, call := range last.Calls {
					obs = append(obs, reg.Invoke(ctx, call))
				}
				history = history.Append(last, obs)
			}
		}, workflow.NodeConfig{})

	return workflowagent.New(workflowagent.Config{
		Name:        "react_graph_agent",
		Description: "ReAct-цикл як динамічний вузол workflow-графа з лімітом ітерацій.",
		Edges:       workflow.Chain(workflow.Start, loop),
	})
}
