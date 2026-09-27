// Побудова бекендів. Винесено окремо, щоб agent.go не тягнув за собою
// вендорські пакети: контракт і проводка інструмента — це не те саме, що вибір
// провайдера, і змінюються вони з різною частотою.
package main

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/genai"
)

// newOpenAICompatible будує бекенд для OpenAI-сумісного endpoint.
//
// Той самий механізм, що в ДЗ 1: agentgateway виглядає для агента як звичайний
// OpenAI endpoint, різниця — лише BaseURL. Але пам'ятайте про грабку з ДЗ 1:
// ADK Go v2.4.0 говорить Responses API, а не chat completions, тож бекенд за
// цією адресою має реалізовувати `/v1/responses`.
func newOpenAICompatible(ctx context.Context, name, key, baseURL string) (model.LLM, error) {
	m, err := openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{
		APIKey:  key,
		BaseURL: baseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("openai-compatible %s: %w", name, err)
	}
	return m, nil
}

// newGemini будує прямий бекенд Google.
func newGemini(ctx context.Context, name, key string) (model.LLM, error) {
	m, err := gemini.NewModel(ctx, name, &genai.ClientConfig{APIKey: key})
	if err != nil {
		return nil, fmt.Errorf("gemini %s: %w", name, err)
	}
	return m, nil
}
