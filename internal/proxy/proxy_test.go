package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/mirante/internal/detect"
	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/internal/hub"
	"github.com/chwiee/mirante/internal/server"
	"github.com/chwiee/mirante/internal/store"
)

type scanIn struct {
	ClusterID string `json:"cluster_id"`
}
type scanOut struct {
	Pods []string `json:"pods"`
}

// mcpServer sobe um MCP server REAL (go-sdk, Streamable HTTP) com scan_cluster.
func mcpServer(t *testing.T, delay time.Duration) (*httptest.Server, *[]scanIn) {
	t.Helper()
	var mu sync.Mutex
	got := &[]scanIn{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-k8s-ts-mcp", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "scan_cluster", Description: "scan"},
		func(_ context.Context, _ *mcp.CallToolRequest, in scanIn) (*mcp.CallToolResult, scanOut, error) {
			time.Sleep(delay)
			mu.Lock()
			*got = append(*got, in)
			mu.Unlock()
			return nil, scanOut{Pods: []string{"api-7f9 CrashLoopBackOff"}}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(ts.Close)
	return ts, got
}

// fakeOllama: 1ª rodada pede scan_cluster (com reason/confiança se foram
// injetados no schema); 2ª rodada, com o retorno da tool, responde em texto.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("caminho no upstream = %s", r.URL.Path)
		}
		var req struct {
			Messages []map[string]any `json:"messages"`
			Tools    []struct {
				Function struct {
					Parameters map[string]any `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		last := req.Messages[len(req.Messages)-1]
		w.Header().Set("Content-Type", "application/json")
		if last["role"] == "tool" {
			io.WriteString(w, `{"model":"qwen2.5:7b","message":{"role":"assistant","content":"No eks-a o pod api-7f9 está em CrashLoopBackOff."},"done":true,"prompt_eval_count":900,"eval_count":40}`)
			return
		}
		args := map[string]any{"cluster_id": "eks-a"}
		props, _ := req.Tools[0].Function.Parameters["properties"].(map[string]any)
		if _, injected := props["reason"]; injected {
			args["reason"] = "usuário pediu scan do eks-a"
			args["confiança"] = 0.42 // com cedilha, como o qwen2.5:7b fez ao vivo
		}
		b, _ := json.Marshal(map[string]any{"model": "qwen2.5:7b", "done": true, "prompt_eval_count": 800, "eval_count": 30,
			"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "scan_cluster", "arguments": args}}}}})
		w.Write(b)
	}))
	t.Cleanup(ts.Close)
	return ts
}

type rig struct {
	srv *server.Server
	px  *Proxy
	url string
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	srv := &server.Server{Store: store.New(100, detect.Default), Hub: hub.New(), UI: fstest.MapFS{}}
	cfg.Emit = func(e *event.Event) { srv.Ingest([]*event.Event{e}) }
	px := New(cfg)
	t.Cleanup(px.Close)
	srv.Mount = px.Register
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &rig{srv: srv, px: px, url: ts.URL}
}

func (r *rig) waitRun(t *testing.T, pred func(*event.Run) bool) *event.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := r.srv.Store.Runs()
		for _, light := range runs {
			if full, ok := r.srv.Store.Run(light.ID); ok && pred(full) {
				return full
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, _ := r.srv.Store.Runs()
	b, _ := json.MarshalIndent(runs, "", " ")
	t.Fatalf("run esperado não apareceu; runs:\n%s", b)
	return nil
}

func chat(t *testing.T, url string, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	return out
}

// O loop inteiro do wb-ce-agent, sem uma linha de SDK: só as URLs apontando
// para o mirante.
func TestAgenteSemCodigoLLMMaisMCP(t *testing.T) {
	mcpTS, received := mcpServer(t, 30*time.Millisecond)
	llmTS := fakeOllama(t)
	r := newRig(t, Config{LLM: map[string]string{"ollama": llmTS.URL}, MCP: map[string]string{"k8s-ts-mcp": mcpTS.URL},
		InjectReasoning: true, RedactKeys: DefaultRedactKeys})

	ctx := context.Background()
	// 1) agente conecta no MCP pelo mirante e lista as tools (como no startup do wb-ce-agent)
	client := mcp.NewClient(&mcp.Implementation{Name: "wb-ce-agent", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: r.url + "/p/wb-ce-agent/mcp/k8s-ts-mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil || len(lt.Tools) != 1 {
		t.Fatalf("ListTools via proxy: %v %v", lt, err)
	}
	schema := lt.Tools[0].InputSchema

	// 2) primeira rodada com o LLM
	llmURL := r.url + "/p/wb-ce-agent/llm/ollama/api/chat"
	msgs := []any{
		map[string]any{"role": "system", "content": "você é o wb-ce-agent"},
		map[string]any{"role": "user", "content": "faz um scan no eks-a"},
	}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "scan_cluster", "description": "scan", "parameters": schema}}}
	resp := chat(t, llmURL, map[string]any{"model": "qwen2.5:7b", "stream": false, "messages": msgs, "tools": tools})
	msg := resp["message"].(map[string]any)
	call := msg["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	args := call["arguments"].(map[string]any)
	if len(args) != 1 {
		t.Fatalf("reason/confiança vazou para o agente: %v", args)
	}

	// 3) agente executa a tool pelo MCP (através do mirante)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "scan_cluster", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("CallTool via proxy: %v %+v", err, res)
	}
	toolText := res.Content[0].(*mcp.TextContent).Text

	// 4) segunda rodada, com o retorno
	msgs = append(msgs, msg, map[string]any{"role": "tool", "name": "scan_cluster", "content": toolText})
	final := chat(t, llmURL, map[string]any{"model": "qwen2.5:7b", "stream": false, "messages": msgs, "tools": tools})
	if !strings.Contains(final["message"].(map[string]any)["content"].(string), "api-7f9") {
		t.Fatalf("resposta final: %v", final)
	}

	run := r.waitRun(t, func(r *event.Run) bool { return r.Status == "ok" && r.Input == "faz um scan no eks-a" })

	if len(*received) != 1 || (*received)[0].ClusterID != "eks-a" {
		t.Fatalf("MCP server recebeu %+v", *received)
	}
	if run.Agent != "wb-ce-agent" || len(run.Steps) != 3 {
		t.Fatalf("esperava decisão, tool, decisão — veio %d steps: %+v", len(run.Steps), run.Steps)
	}
	d, tool := run.Steps[0], run.Steps[1]
	if d.Kind != "decision" || d.Model != "qwen2.5:7b" || d.TokensIn != 800 || d.Confidence == nil || *d.Confidence != 0.42 {
		t.Fatalf("decisão: %+v", d)
	}
	if tool.Server != "k8s-ts-mcp" || tool.Tool != "scan_cluster" || tool.Rationale != "usuário pediu scan do eks-a" ||
		string(tool.Args) != `{"cluster_id":"eks-a"}` || tool.Status != "ok" || tool.DurationMs < 30 {
		t.Fatalf("tool: %+v args=%s", tool, tool.Args)
	}
	if !strings.Contains(string(tool.Result), "api-7f9") {
		t.Fatalf("retorno da tool não veio do MCP: %s", tool.Result)
	}
	if run.Output != "No eks-a o pod api-7f9 está em CrashLoopBackOff." {
		t.Fatalf("output: %q", run.Output)
	}
	if run.Worst() != event.LevelUncertain { // confiança 0.42 < 0.6, e nada inventado
		t.Fatalf("flags: run=%+v steps=%+v %+v", run.Flags, d.Flags, tool.Flags)
	}
}

// Agente cujo LLM não passa pelo mirante (Bedrock, Claude Code…): só o MCP.
func TestSoMCPViraRunSintetico(t *testing.T) {
	mcpTS, _ := mcpServer(t, 0)
	r := newRig(t, Config{MCP: map[string]string{"k8s-ts-mcp": mcpTS.URL}})
	r.px.corr.idleMCP = 50 * time.Millisecond

	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "alfred", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: r.url + "/p/alfred/mcp/k8s-ts-mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	cs.ListTools(ctx, nil)
	for _, c := range []string{"eks-a", "eks-b"} {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "scan_cluster", Arguments: map[string]any{"cluster_id": c}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(80 * time.Millisecond)
	r.px.corr.sweep()

	run := r.waitRun(t, func(r *event.Run) bool { return r.Agent == "alfred" && r.Status == "ok" })
	if len(run.Steps) != 2 || run.Steps[0].Server != "k8s-ts-mcp" || run.Steps[1].Status != "ok" {
		t.Fatalf("run sintético: %+v", run.Steps)
	}
	if len(run.Tools) != 1 || run.Tools[0].Name != "scan_cluster" {
		t.Fatalf("tools aprendidas do tools/list: %+v", run.Tools)
	}
}

func TestLLMForaDoArFechaRunComErro(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	r := newRig(t, Config{LLM: map[string]string{"openai": dead.URL}})
	b := `{"model":"m","messages":[{"role":"user","content":"oi"}]}`
	resp, err := http.Post(r.url+"/p/ce-aut/llm/openai/v1/chat/completions", "application/json", strings.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	run := r.waitRun(t, func(r *event.Run) bool { return r.Status == "error" })
	if !strings.HasPrefix(run.Error, "LLM:") {
		t.Fatalf("erro: %q", run.Error)
	}
}

func TestUpstreamDesconhecido404(t *testing.T) {
	r := newRig(t, Config{})
	resp, _ := http.Post(r.url+"/p/x/mcp/nao-existe", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestOpenAIStreamComToolCallFragmentada(t *testing.T) {
	body := strings.Join([]string{
		`data: {"model":"qwen","choices":[{"delta":{"content":"vou checar "}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"logs","arguments":"{\"clus"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ter\":\"a\",\"motivo\":\"pediu log\",\"confianca\":0.9}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	r, ok := parseChatResp(dialectOpenAI, []byte(body), true)
	if !ok || len(r.Calls) != 1 {
		t.Fatalf("parse: %+v", r)
	}
	c := r.Calls[0]
	if c.ID != "call_1" || c.Name != "logs" || string(c.Args) != `{"cluster":"a"}` || c.Motivo != "pediu log" || *c.Confianca != 0.9 {
		t.Fatalf("call: %+v args=%s", c, c.Args)
	}
	if r.Content != "vou checar " || r.TokensIn != 10 || r.Model != "qwen" {
		t.Fatalf("resp: %+v", r)
	}
}

func TestOllamaStream(t *testing.T) {
	body := `{"model":"q","message":{"role":"assistant","content":"No "}}
{"model":"q","message":{"role":"assistant","content":"eks-a tudo ok."}}
{"model":"q","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":7,"eval_count":3}`
	r, ok := parseChatResp(dialectOllama, []byte(body), true)
	if !ok || r.Content != "No eks-a tudo ok." || r.TokensOut != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestStripReasoningOpenAI(t *testing.T) {
	in := `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"x","arguments":"{\"a\":1,\"motivo\":\"m\",\"confianca\":0.5}"}}]}}]}`
	out := string(stripReasoning(dialectOpenAI, []byte(in)))
	if strings.Contains(out, "motivo") || !strings.Contains(out, `{\"a\":1}`) {
		t.Fatalf("%s", out)
	}
}

func TestRedacao(t *testing.T) {
	red := redactor(DefaultRedactKeys)
	out := string(red(json.RawMessage(`{"cluster":"a","password":"s3nh4","nested":[{"api_key":"k","ok":1}],"token":""}`)))
	if strings.Contains(out, "s3nh4") || strings.Contains(out, `"k"`) || !strings.Contains(out, `"cluster":"a"`) {
		t.Fatalf("%s", out)
	}
}

// Rede de segurança: mesmo que um campo de raciocínio chegue no tools/call
// (agente que monta args por conta própria, variação não prevista…), o MCP
// server real — que rejeita propriedade desconhecida — recebe a chamada limpa.
func TestMCPRemoveCampoDeRaciocinioQueEscapou(t *testing.T) {
	mcpTS, received := mcpServer(t, 0)
	r := newRig(t, Config{MCP: map[string]string{"k8s-ts-mcp": mcpTS.URL}, InjectReasoning: true})
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "x", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: r.url + "/p/x/mcp/k8s-ts-mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	cs.ListTools(ctx, nil)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "scan_cluster",
		Arguments: map[string]any{"cluster_id": "eks-a", "confiança": 0.5, "Motivo": "x"}})
	if err != nil || res.IsError {
		t.Fatalf("chamada deveria passar limpa: err=%v res=%+v", err, res)
	}
	if len(*received) != 1 || (*received)[0].ClusterID != "eks-a" {
		t.Fatalf("server recebeu %+v", *received)
	}
}

// "Conectou, apareceu": só de conectar e listar as tools, o agente já entra
// no mapa — é o primeiro sinal de que a implantação funcionou.
func TestConectarJaApareceNoMapa(t *testing.T) {
	mcpTS, _ := mcpServer(t, 0)
	r := newRig(t, Config{MCP: map[string]string{"k8s-ts-mcp": mcpTS.URL}})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "novo", Version: "0"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: r.url + "/p/agente-novo/mcp/k8s-ts-mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range r.srv.Store.Topology() {
			if e.Agent == "agente-novo" && e.Server == "k8s-ts-mcp" && e.Tool == "scan_cluster" && e.Declared {
				if runs, _ := r.srv.Store.Runs(); len(runs) != 0 {
					t.Fatalf("listar tools não deveria criar run: %d", len(runs))
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("topologia: %+v", r.srv.Store.Topology())
}
