package detect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chwiee/mirante/internal/event"
)

func conf(v float64) *float64 { return &v }

func codes(fs []event.Flag) string {
	var c []string
	for _, f := range fs {
		c = append(c, f.Level+":"+f.Code)
	}
	return strings.Join(c, ",")
}

var scan = event.ToolSpec{Server: "k8s-ts-mcp", Name: "scan_cluster", Schema: map[string]any{
	"type": "object", "required": []any{"cluster_id"}, "additionalProperties": false,
	"properties": map[string]any{"cluster_id": map[string]any{"type": "string"}, "tail": map[string]any{"type": "integer"},
		"mode": map[string]any{"type": "string", "enum": []any{"fast", "deep"}}},
}}

func TestToolCall(t *testing.T) {
	cases := []struct {
		name string
		step event.Step
		want string
	}{
		{"ok", event.Step{Server: "k8s-ts-mcp", Tool: "scan_cluster", Args: json.RawMessage(`{"cluster_id":"a"}`), Confidence: conf(0.9)}, ""},
		{"tool inventada", event.Step{Server: "k8s-ts-mcp", Tool: "restart_cluster", Args: json.RawMessage(`{}`)}, "hallucination:tool_inexistente"},
		{"arg faltando e arg inventado", event.Step{Tool: "scan_cluster", Args: json.RawMessage(`{"cluster":"a"}`)},
			"hallucination:args_invalidos,hallucination:args_invalidos"},
		{"tipo e enum", event.Step{Tool: "scan_cluster", Args: json.RawMessage(`{"cluster_id":"a","tail":1.5,"mode":"turbo"}`)},
			"hallucination:args_invalidos,hallucination:args_invalidos"},
		{"baixa confiança", event.Step{Tool: "scan_cluster", Args: json.RawMessage(`{"cluster_id":"a"}`), Confidence: conf(0.3)}, "uncertain:baixa_confianca"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.step
			s.Kind = "tool"
			run := &event.Run{Tools: []event.ToolSpec{scan}, Steps: []*event.Step{&s}}
			if got := codes(Default.ToolCall(run, &s)); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestToolCallSemToolsDeclaradasNaoAcusa(t *testing.T) {
	s := &event.Step{Kind: "tool", Tool: "qualquer"}
	if f := Default.ToolCall(&event.Run{Steps: []*event.Step{s}}, s); len(f) != 0 {
		t.Fatalf("sem run_start.tools não dá pra afirmar nada, veio %v", f)
	}
}

func TestChamadaRepetida(t *testing.T) {
	a := &event.Step{Kind: "tool", Tool: "search_knowledge", Args: json.RawMessage(`{"query":"x"}`)}
	b := &event.Step{Kind: "tool", Tool: "search_knowledge", Args: json.RawMessage(`{ "query": "x" }`)}
	run := &event.Run{Steps: []*event.Step{a, b}}
	if got := codes(Default.ToolCall(run, b)); got != "uncertain:chamada_repetida" {
		t.Fatalf("got %q", got)
	}
}

func TestDecisionAmbigua(t *testing.T) {
	s := &event.Step{Confidence: conf(0.58), Alternatives: []event.Alternative{{Tool: "troubleshoot", Score: conf(0.54)}}}
	if got := codes(Default.Decision(s)); got != "uncertain:baixa_confianca,uncertain:escolha_ambigua" {
		t.Fatalf("got %q", got)
	}
}

func TestRunEnd(t *testing.T) {
	tool := func(result, err string) *event.Step {
		st := &event.Step{Kind: "tool", Tool: "scan_cluster", Args: json.RawMessage(`{"cluster_id":"eks-prd-01"}`), Status: "ok"}
		if result != "" {
			st.Result = json.RawMessage(result)
		}
		if err != "" {
			st.Error, st.Status = err, "error"
		}
		return st
	}
	cases := []struct {
		name   string
		input  string
		steps  []*event.Step
		output string
		want   string
	}{
		{"fundamentada", "scan no eks-prd-01", []*event.Step{tool(`{"pod":"catalog-web-6c9d8","limit":"256Mi"}`, "")},
			"No eks-prd-01 o pod catalog-web-6c9d8 está com limit 256Mi. Verifique-se a taxa de 120/s e msg/s; posso rodar scan_cluster de novo.", ""},
		{"inventou pod", "scan no eks-prd-01", []*event.Step{tool(`{"pod":"catalog-web-6c9d8"}`, "")},
			"O payments-worker-77f4b no nó ip-10-42-3-17 está sem memória.", "hallucination:resposta_sem_base"},
		{"resultado como string JSON", "scan", []*event.Step{tool(`"pod: api-7f9\nstatus: ok"`, "")}, "O api-7f9 está ok.", ""},
		{"sem tool cita id", "como reinicio um pod?", nil, "Rode kubectl delete pod api-123.", "uncertain:resposta_sem_base"},
		{"tudo falhou e resposta confiante", "logs", []*event.Step{tool("", "context deadline exceeded")}, "Os logs estão limpos.", "uncertain:resposta_apos_erro"},
		{"tudo falhou e admite", "logs", []*event.Step{tool("", "timeout")}, "Não consegui ler os logs (timeout).", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := &event.Run{Input: c.input, Steps: c.steps, Output: c.output, Tools: []event.ToolSpec{scan}}
			if got := codes(Default.RunEnd(run)); got != c.want {
				t.Fatalf("got %q, want %q (%v)", got, c.want, Default.RunEnd(run))
			}
		})
	}
}

// Casos tirados do teste ao vivo com qwen2.5:7b + knowledge-mcp.
func TestAcaoNaoExecutada(t *testing.T) {
	search := &event.Step{Kind: "tool", Tool: "search_knowledge", Status: "ok", Result: json.RawMessage(`"lição: aumentar limit para 512Mi"`)}
	record := &event.Step{Kind: "tool", Tool: "record_lesson", Status: "ok"}
	recordErr := &event.Step{Kind: "tool", Tool: "record_lesson", Status: "error", Error: "x"}
	cases := []struct {
		name   string
		steps  []*event.Step
		output string
		want   bool
	}{
		{"registrou sem chamar record", []*event.Step{search}, "Já vimos antes: aumentar para 512Mi resolveu.\n\nRegistrei essa informação para futuras referências.", true},
		{"registrou e chamou", []*event.Step{search, record}, "Registrei a lição.", false},
		{"registrou mas a tool falhou", []*event.Step{recordErr}, "Registrei a lição.", true},
		{"aumentou sem tool de alteração", []*event.Step{search, record}, "Aumentei o limite de memória para 512Mi.", true},
		{"inglês", []*event.Step{search}, "I restarted the pod.", true},
		{"só sugere", []*event.Step{search}, "Vamos tentar aumentar o limite para 512Mi de novo.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := &event.Run{Input: "OOMKilled no checkout-api", Steps: c.steps, Output: c.output}
			got := strings.Contains(codes(Default.RunEnd(run)), "acao_nao_executada")
			if got != c.want {
				t.Fatalf("acao_nao_executada=%v, quer %v: %v", got, c.want, Default.RunEnd(run))
			}
		})
	}
}

// O raciocínio de uma decisão intermediária também conta ("Aumentei o limite
// de memória para 512Mi no checkout-api" enquanto chamava record_lesson).
func TestAcaoNaoExecutadaNoRaciocinio(t *testing.T) {
	run := &event.Run{Input: "OOMKilled no checkout-api", Output: "Vamos monitorar.", Steps: []*event.Step{
		{Kind: "decision", Reasoning: "Aumentei o limite de memória para 512Mi no checkout-api."},
		{Kind: "tool", Tool: "record_lesson", Status: "ok"},
	}}
	if !strings.Contains(codes(Default.RunEnd(run)), "acao_nao_executada") {
		t.Fatal(Default.RunEnd(run))
	}
}

func TestAcaoNaoExecutadaFontesDeAcao(t *testing.T) {
	no, yes := false, true
	ts := &event.Step{Kind: "tool", Tool: "troubleshoot", Status: "ok"}
	out := "Já reiniciei o pod."
	flagged := func(c Config, tools []event.ToolSpec, steps ...*event.Step) bool {
		run := &event.Run{Input: "x", Output: out, Tools: tools, Steps: steps}
		return strings.Contains(codes(c.RunEnd(run)), "acao_nao_executada")
	}
	if !flagged(Default, nil, ts) {
		t.Error("sem nenhuma informação, troubleshoot não parece reiniciar nada")
	}
	if flagged(Config{ActionTools: []string{"troubleshoot"}}, nil, ts) {
		t.Error("--action-tools troubleshoot deveria contar como ação")
	}
	if flagged(Default, []event.ToolSpec{{Name: "troubleshoot", ReadOnly: &no}}, ts) {
		t.Error("anotação MCP readOnlyHint=false deveria contar como ação")
	}
	if !flagged(Default, []event.ToolSpec{{Name: "rollout_restart", ReadOnly: &yes}}, &event.Step{Kind: "tool", Tool: "rollout_restart", Status: "ok"}) {
		t.Error("readOnlyHint=true vence a palavra-chave do nome")
	}
	out = "Alterei a configuração."
	if !flagged(Default, nil, &event.Step{Kind: "tool", Tool: "get_settings", Status: "ok"}) {
		t.Error(`"get_settings" não pode contar como "set" (match por token, não substring)`)
	}
	if flagged(Default, nil, &event.Step{Kind: "tool", Tool: "update_config", Status: "ok"}) {
		t.Error("update_config deveria contar como alteração")
	}
}

// "bem-sucedida" disparou resposta_sem_base ao vivo: palavra composta do
// português não é identificador. Nome de recurso entre crases ainda conta.
func TestIdentificadorVersusPalavraComposta(t *testing.T) {
	scan := &event.Step{Kind: "tool", Tool: "search_knowledge", Status: "ok", Result: json.RawMessage(`"OOMKilled em checkout-api"`)}
	cases := []struct {
		output string
		want   string
	}{
		{"A solução foi bem-sucedida e o pós-incidente está ok; mande um e-mail.", ""},
		{"O `payments-worker` também caiu.", "payments-worker"},
		{"O pod payments-worker caiu.", ""},                            // um hífen, sem crase, sem dígito: não acusa
		{"Veja o eks-prd-payments.", "eks-prd-payments"},               // 2+ separadores: acusa
		{"O checkout-api caiu de novo.", ""},                           // está no retorno da tool
		{"O pod payments-worker-77f4b caiu.", "payments-worker-77f4b"}, // com dígito: acusa
	}
	for _, c := range cases {
		run := &event.Run{Input: "OOMKilled?", Steps: []*event.Step{scan}, Output: c.output}
		fs := Default.RunEnd(run)
		got := ""
		for _, f := range fs {
			if f.Code == "resposta_sem_base" {
				got = f.Reason
			}
		}
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%q → %q, quer %q", c.output, got, c.want)
		}
	}
}
