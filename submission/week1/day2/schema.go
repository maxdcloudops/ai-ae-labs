// Схеми: те, що модель насправді бачить перед викликом.
//
// Обидві функції тут викликають рівно той самий `jsonschema.For[T]`, яким
// користується сам functiontool (див. tool/functiontool/function.go:272). Це
// важливо: якби я будував схему для друку іншим способом, команда `schema`
// показувала б не те, що їде в модель, — найгірший різновид документації.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// InferredInputSchemaJSON — схема входу, виведена з RateInput.
func InferredInputSchemaJSON() (string, error) {
	return schemaJSON[RateInput]()
}

// InferredOutputSchemaJSON — схема виходу, виведена з RateQuote.
//
// Саме її варто відкрити очима: три рівні вкладеності
// (RateQuote → history[] → source), масив і enum згенеровані з Go-структур, без
// жодного рядка JSON Schema від руки.
func InferredOutputSchemaJSON() (string, error) {
	return schemaJSON[RateQuote]()
}

func schemaJSON[T any]() (string, error) {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		return "", fmt.Errorf("infer schema: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal schema: %w", err)
	}
	return string(b), nil
}
