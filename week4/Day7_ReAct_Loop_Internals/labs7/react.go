// ВИ: мануальний ReAct-цикл. Тут немає імпортів ADK — це `for` і `if` над
// інтерфейсом Planner та реєстром інструментів.
//
// Словник (ToolCall, Step, Planner, Budget, Trace, Registry, Observation) уже
// є в internal/react — прочитайте його перед тим, як писати цикл.
package main

import (
	"context"
	"fmt"

	"github.com/dimetron/ai-eng-course/labs/week4/Day7_ReAct_Loop_Internals/labs7/internal/react"
)

// Run виконує цикл Thought → Action → Observation, доки Step.Final не стане
// true або не вичерпано Budget.
//
// Увесь «агент» — це оцей for. Жодної магії всередині немає: планувальник
// каже, що робити, реєстр це виконує, історія несе результат у наступний крок.
// Коли фреймворк робить те саме, він робить рівно це.
//
// Trace повертається і разом із помилкою — навмисно. О третій ночі питання
// звучить «де воно застрягло», і відповідає на нього тільки слід, а не текст
// помилки.
func Run(ctx context.Context, p react.Planner, reg react.Registry, task string, b react.Budget) (string, react.Trace, error) {
	b = b.Normalized()
	history := react.Seed(task)
	var trace react.Trace

	for i := 1; ; i++ {
		// Ліміт перевіряється ПЕРЕД викликом планувальника: інакше остання
		// ітерація встигає заплатити за крок, який ми вже вирішили не робити.
		if i > b.MaxIterations {
			return "", trace, fmt.Errorf("вичерпано ліміт %d ітерацій: %w", b.MaxIterations, ErrMaxIterations)
		}
		trace.Iterations = i

		step, err := p.NextStep(ctx, history)
		if err != nil {
			return "", trace, fmt.Errorf("ітерація %d: %w", i, err)
		}
		trace.Steps = append(trace.Steps, step)
		trace.Usage = trace.Usage.Add(step.Usage)

		if step.Final {
			// Порожній зріз, а не пропуск: Steps і Observations мусять
			// лишатися однакової довжини, інакше читач сліду зсунеться на
			// крок і припише спостереження не тій ітерації.
			trace.Observations = append(trace.Observations, nil)
			return step.Answer, trace, nil
		}

		// Порядок спостережень = порядок викликів, і кожне несе CallID свого
		// виклику. Це і є кореляція one-or-many: модель може попросити три
		// інструменти одним кроком, і сплутати відповіді не можна.
		obs := make([]react.Observation, 0, len(step.Calls))
		for _, call := range step.Calls {
			obs = append(obs, reg.Invoke(ctx, call))
		}
		trace.Observations = append(trace.Observations, obs)

		// Без Append наступний крок не побачить результату й повторить той
		// самий виклик — це найчастіший спосіб отримати нескінченний цикл.
		history = history.Append(step, obs)
	}
}
