// Package event define o contrato de ingestão do mirante: os eventos que um
// agente emite enquanto atende uma requisição, e a visão montada (Run/Step)
// que o servidor reconstrói a partir deles e manda pro dashboard.
//
// O contrato é JSON plano de propósito — qualquer agente (Go via pkg/mirante,
// ou Python/Node com um POST simples) consegue emitir sem depender do SDK.
package event

import (
	"encoding/json"
	"time"
)

// Tipos de evento aceitos em POST /v1/events.
const (
	RunStart   = "run_start"   // requisição chegou no agente
	Decision   = "decision"    // LLM decidiu (chamar tool(s) ou responder)
	ToolCall   = "tool_call"   // tool começou a executar
	ToolResult = "tool_result" // tool terminou (ok ou erro)
	FlagEvt    = "flag"        // sinal explícito vindo do agente (ex: LLM-judge)
	RunEnd     = "run_end"     // agente respondeu
	ToolsEvt   = "tools"       // agente conectou num MCP server e listou as tools (sem run)
)

// Níveis de flag. Hallucination vira bolinha vermelha; Uncertain, amarela.
const (
	LevelHallucination = "hallucination"
	LevelUncertain     = "uncertain"
)

// ToolSpec descreve uma tool disponível pro modelo naquele run. É a base pra
// detectar tool inexistente e argumentos fora do schema.
type ToolSpec struct {
	Server      string         `json:"server,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	// ReadOnly vem das anotações MCP (readOnlyHint/destructiveHint). false =
	// a tool altera algo (conta como "ação" para acao_nao_executada).
	ReadOnly *bool `json:"read_only,omitempty"`
}

// Alternative é uma tool que o modelo considerou e descartou.
type Alternative struct {
	Tool  string   `json:"tool"`
	Score *float64 `json:"score,omitempty"`
	Why   string   `json:"why,omitempty"`
}

// Flag é um sinal de alucinação/incerteza anexado a um step ou ao run.
type Flag struct {
	Level  string `json:"level"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"` // "mirante" (heurística) ou "agent"
}

// Event é o envelope único de ingestão. Só os campos relevantes ao Type vêm
// preenchidos.
type Event struct {
	Type     string    `json:"type"`
	RunID    string    `json:"run_id"`
	SpanID   string    `json:"span_id,omitempty"`
	ParentID string    `json:"parent_id,omitempty"`
	Agent    string    `json:"agent"`
	Time     time.Time `json:"time"`

	// run_start
	Input string     `json:"input,omitempty"`
	User  string     `json:"user,omitempty"`
	Tools []ToolSpec `json:"tools,omitempty"`

	// decision
	Model        string        `json:"model,omitempty"`
	Reasoning    string        `json:"reasoning,omitempty"`
	Chosen       []string      `json:"chosen,omitempty"`
	Alternatives []Alternative `json:"alternatives,omitempty"`
	TokensIn     int           `json:"tokens_in,omitempty"`
	TokensOut    int           `json:"tokens_out,omitempty"`

	// tool_call
	Server    string          `json:"server,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Rationale string          `json:"rationale,omitempty"`

	// decision / tool_call
	Confidence *float64 `json:"confidence,omitempty"`

	// tool_result
	Result     json.RawMessage `json:"result,omitempty"`
	DurationMs float64         `json:"duration_ms,omitempty"`

	// tool_result / run_end
	Error string `json:"error,omitempty"`

	// run_end
	Output string `json:"output,omitempty"`

	// flag
	Flag *Flag `json:"flag,omitempty"`
}

// Step é um passo montado de um Run: uma decisão do LLM ou uma chamada de tool.
type Step struct {
	SpanID       string          `json:"span_id"`
	ParentID     string          `json:"parent_id,omitempty"`
	Kind         string          `json:"kind"` // "decision" | "tool"
	Server       string          `json:"server,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Model        string          `json:"model,omitempty"`
	Reasoning    string          `json:"reasoning,omitempty"`
	Chosen       []string        `json:"chosen,omitempty"`
	Alternatives []Alternative   `json:"alternatives,omitempty"`
	Rationale    string          `json:"rationale,omitempty"`
	Confidence   *float64        `json:"confidence,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	TokensIn     int             `json:"tokens_in,omitempty"`
	TokensOut    int             `json:"tokens_out,omitempty"`
	Start        time.Time       `json:"start"`
	DurationMs   float64         `json:"duration_ms"`
	Status       string          `json:"status"` // running | ok | error
	Flags        []Flag          `json:"flags,omitempty"`
}

// Run é uma requisição completa atendida por um agente.
type Run struct {
	ID         string     `json:"id"`
	Agent      string     `json:"agent"`
	User       string     `json:"user,omitempty"`
	Input      string     `json:"input"`
	Output     string     `json:"output,omitempty"`
	Error      string     `json:"error,omitempty"`
	Tools      []ToolSpec `json:"tools,omitempty"`
	Start      time.Time  `json:"start"`
	DurationMs float64    `json:"duration_ms"`
	Status     string     `json:"status"` // running | ok | error
	Steps      []*Step    `json:"steps"`
	Flags      []Flag     `json:"flags,omitempty"`
}

// Worst devolve o nível de flag mais grave do run inteiro (run + steps), ou "".
func (r *Run) Worst() string {
	worst := ""
	check := func(fs []Flag) {
		for _, f := range fs {
			if f.Level == LevelHallucination {
				worst = LevelHallucination
			} else if f.Level == LevelUncertain && worst == "" {
				worst = LevelUncertain
			}
		}
	}
	check(r.Flags)
	for _, s := range r.Steps {
		check(s.Flags)
	}
	return worst
}
