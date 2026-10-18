package main

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"
)

// newGraph збирає agentic retrieval-граф.
//
//	Start → cache_check ─┬─ "hit"  → serve_cached
//	                     └─ "miss" → classify ─┬─ vector_search    ─┐
//	                                           ├─ graph_filter      ├─ join → rerank → format
//	                                           └─ graph_traversal  ─┘
//
// Три речі, які варто прочитати як рішення, а не як схему.
//
// **Кеш стоїть ПЕРШИМ.** Поставити його після classify означало б платити за
// планування на кожному повторі. Hit має коштувати один embed запиту — і все.
//
// **Гілки — fan-out, а не вибір.** Класифікатор емітує клас запиту, але всі
// три гілки виконуються паралельно, і кожна сама вирішує, чи їй є що сказати:
// graph_traversal на single-hop просто повертає порожньо. Так виходить дешевше,
// ніж будувати маршрутизацію й помилятися в ній: гілка коштує мілісекунди,
// а пропущений hop — половини відповіді.
//
// **JoinNode — єдиний легальний спосіб звести гілки.** Більш ніж одне
// безумовне вхідне ребро у звичайний вузол ADK відхиляє
// (ErrUnsupportedFanIn); бар'єр мусить бути JoinNode, і його вихід —
// map[string]any з ключем за іменем попередника.
func newGraph(r *retriever) (agent.Agent, error) {
	// Без retry: вузли локальні, повтор не змінить результат.
	cfg := workflow.NodeConfig{}

	cacheCheck := workflow.NewEmittingFunctionNode("cache_check", r.cacheCheck, cfg)
	serveCached := workflow.NewFunctionNode("serve_cached", r.serveFromCache, cfg)
	classify := workflow.NewEmittingFunctionNode("classify", r.classify, cfg)

	vectorSearch := workflow.NewFunctionNode("vector_search", r.vectorSearch, cfg)
	graphFilter := workflow.NewFunctionNode("graph_filter", r.graphFilter, cfg)
	graphTraversal := workflow.NewFunctionNode("graph_traversal", r.graphTraversal, cfg)

	join := workflow.NewJoinNode("join")
	rerank := workflow.NewFunctionNode("rerank", r.mergeAndRerank, cfg)
	format := workflow.NewFunctionNode("format", r.format, cfg)

	edges := workflow.NewEdgeBuilder().
		Add(workflow.Start, cacheCheck).
		AddRoute(cacheCheck, serveCached, workflow.StringRoute("hit")).
		AddRoute(cacheCheck, classify, workflow.Default).
		AddFanOut(classify, vectorSearch, graphFilter, graphTraversal).
		AddFanIn(join, vectorSearch, graphFilter, graphTraversal).
		Add(join, rerank).
		Add(rerank, format).
		Build()

	return workflowagent.New(workflowagent.Config{
		Name:        "agentic_graphrag",
		Description: "Agentic GraphRAG: семантичний кеш, планувальник, три гілки retrieval, re-ranking і відповідь із provenance.",
		Edges:       edges,
	})
}
