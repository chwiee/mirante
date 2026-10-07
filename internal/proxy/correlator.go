package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/chwiee/mirante/internal/event"
)

// correlator transforma o que os proxies observam em eventos de run.
//
// Run = um TURNO de conversa: começa na última mensagem do usuário e termina
// quando o LLM responde sem pedir tool. Num loop de tool-calling, toda
// requisição ao LLM dentro do mesmo turno reenvia o histórico até aquela
// mensagem do usuário — esse prefixo é a chave que liga as requisições.
//
// Quando o proxy MCP também está no caminho, a chamada MCP é casada com a
// tool_call pendente do LLM (mesmo agente, mesmo nome, args iguais de
// preferência) e é ela quem fecha o step, com duração exata. Sem LLM
// observado (ex: Bedrock, Claude Code), as chamadas MCP viram um run
// sintético por sessão de atividade.
type correlator struct {
	mu          sync.Mutex
	emit        func(*event.Event)
	idleMCP     time.Duration // run sintético sem sessão: fecha após este silêncio
	idleSession time.Duration // sessão MCP sem DELETE: fecha após este silêncio
	idleLLM     time.Duration
	agents      map[string]*agentState
	redact      func(json.RawMessage) json.RawMessage
}

type agentState struct {
	runs       map[string]*llmRun // chave do turno → run aberto
	toolServer map[string]string  // tool → MCP server (aprendido no tools/list)
	tools      map[string]event.ToolSpec
	mcpRuns    map[string]*mcpRun // Mcp-Session-Id → run sintético ("" = sem sessão)
}

type llmRun struct {
	id       string
	seen     int // mensagens já processadas do histórico
	pending  []*pendingCall
	lastSeen time.Time
}

type pendingCall struct {
	span     string
	callID   string // id da tool_call (OpenAI); vazio no Ollama
	name     string
	args     json.RawMessage
	start    time.Time
	resolved bool // retorno já emitido
	claimed  bool // casada com uma chamada MCP observada
}

type mcpRun struct {
	id       string
	lastSeen time.Time
	inflight int
	calls    int
}

func newCorrelator(emit func(*event.Event), redact func(json.RawMessage) json.RawMessage) *correlator {
	return &correlator{emit: emit, redact: redact, idleMCP: 20 * time.Second, idleSession: 2 * time.Minute, idleLLM: 10 * time.Minute, agents: map[string]*agentState{}}
}

func (c *correlator) agent(name string) *agentState {
	a := c.agents[name]
	if a == nil {
		a = &agentState{runs: map[string]*llmRun{}, toolServer: map[string]string{}, tools: map[string]event.ToolSpec{}, mcpRuns: map[string]*mcpRun{}}
		c.agents[name] = a
	}
	return a
}

func (c *correlator) send(agent string, e *event.Event) {
	e.Agent = agent
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Args, e.Result = c.redact(e.Args), c.redact(e.Result)
	c.emit(e)
}

// ---------- lado LLM ----------

type chatMsg struct {
	Role       string
	Content    string
	ToolCallID string
	Name       string
}

type chatReq struct {
	Model    string
	Messages []chatMsg
	Tools    []event.ToolSpec
	Stream   bool
}

type toolCall struct {
	ID        string
	Name      string
	Args      json.RawMessage // sem reason/confidence
	Motivo    string
	Confianca *float64
}

type chatResp struct {
	Model     string
	Content   string
	Reasoning string
	Calls     []toolCall
	TokensIn  int
	TokensOut int
}

// turn liga a resposta do LLM ao run aberto na requisição.
type turn struct {
	agent string
	run   *llmRun
	start time.Time
}

func (c *correlator) llmRequest(agent string, req chatReq) *turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(agent)
	now := time.Now()

	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	key := turnKey(agent, req.Messages, lastUser)

	run := a.runs[key]
	if run == nil {
		run = &llmRun{id: newID(), seen: lastUser + 1}
		a.runs[key] = run
		input := ""
		if lastUser >= 0 {
			input = req.Messages[lastUser].Content
		}
		tools := make([]event.ToolSpec, len(req.Tools))
		for i, t := range req.Tools {
			t.Server = a.toolServer[t.Name]
			tools[i] = t
		}
		c.send(agent, &event.Event{Type: event.RunStart, RunID: run.id, Input: input, Tools: tools})
	}

	// mensagens novas desde a última requisição: retornos de tool
	for _, m := range req.Messages[min(run.seen, len(req.Messages)):] {
		if m.Role != "tool" {
			continue
		}
		p := run.match(m)
		if p == nil || p.resolved {
			continue
		}
		p.resolved = true
		e := &event.Event{Type: event.ToolResult, RunID: run.id, SpanID: p.span, Server: a.toolServer[p.name], Tool: p.name,
			Result: jsonString(m.Content), DurationMs: ms(now.Sub(p.start))}
		if looksLikeError(m.Content) {
			e.Error = m.Content
		}
		c.send(agent, e)
	}
	run.seen = len(req.Messages)
	run.lastSeen = now
	return &turn{agent: agent, run: run, start: now}
}

func (r *llmRun) match(m chatMsg) *pendingCall {
	if m.ToolCallID != "" {
		for _, p := range r.pending {
			if p.callID == m.ToolCallID {
				return p
			}
		}
	}
	for _, p := range r.pending {
		if !p.resolved && m.Name != "" && p.name == m.Name {
			return p
		}
	}
	for _, p := range r.pending {
		if !p.resolved {
			return p
		}
	}
	return nil
}

func (c *correlator) llmResponse(t *turn, resp chatResp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(t.agent)
	now := time.Now()

	chosen := make([]string, 0, len(resp.Calls))
	var conf *float64
	for _, tc := range resp.Calls {
		chosen = append(chosen, tc.Name)
		if tc.Confianca != nil && (conf == nil || *tc.Confianca < *conf) {
			conf = tc.Confianca // a decisão é tão confiável quanto a chamada menos confiável
		}
	}
	reasoning := strings.TrimSpace(strings.TrimSpace(resp.Reasoning + "\n" + resp.Content))
	if len(resp.Calls) == 0 {
		reasoning = strings.TrimSpace(resp.Reasoning)
	}
	c.send(t.agent, &event.Event{Type: event.Decision, RunID: t.run.id, SpanID: newID(), Model: resp.Model,
		Reasoning: reasoning, Chosen: chosen, Confidence: conf, TokensIn: resp.TokensIn, TokensOut: resp.TokensOut,
		DurationMs: ms(now.Sub(t.start))})

	if len(resp.Calls) == 0 {
		c.send(t.agent, &event.Event{Type: event.RunEnd, RunID: t.run.id, Output: resp.Content})
		c.dropRun(a, t.run)
		return
	}
	for _, tc := range resp.Calls {
		p := &pendingCall{span: newID(), callID: tc.ID, name: tc.Name, args: tc.Args, start: now}
		t.run.pending = append(t.run.pending, p)
		c.send(t.agent, &event.Event{Type: event.ToolCall, RunID: t.run.id, SpanID: p.span, Server: a.toolServer[tc.Name],
			Tool: tc.Name, Args: tc.Args, Rationale: tc.Motivo, Confidence: tc.Confianca})
	}
	t.run.lastSeen = now
}

func (c *correlator) llmError(t *turn, err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.send(t.agent, &event.Event{Type: event.RunEnd, RunID: t.run.id, Error: "LLM: " + err})
	c.dropRun(c.agent(t.agent), t.run)
}

func (c *correlator) dropRun(a *agentState, r *llmRun) {
	for k, v := range a.runs {
		if v == r {
			delete(a.runs, k)
		}
	}
}

// ---------- lado MCP ----------

func (c *correlator) mcpTools(agent, server string, specs []event.ToolSpec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(agent)
	out := make([]event.ToolSpec, 0, len(specs))
	for _, s := range specs {
		s.Server = server
		a.toolServer[s.Name] = server
		a.tools[s.Name] = s
		out = append(out, s)
	}
	// avisa o painel já na conexão: o agente aparece ligado às tools antes
	// da primeira pergunta — é o "conectou, apareceu" da validação.
	c.send(agent, &event.Event{Type: event.ToolsEvt, Tools: out})
}

// declares diz se a tool (aprendida no tools/list) declara a propriedade k.
func (c *correlator) declares(agent, tool, k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	spec, ok := c.agent(agent).tools[tool]
	if !ok {
		return false
	}
	props, _ := spec.Schema["properties"].(map[string]any)
	_, declared := props[k]
	return declared
}

// mcpCall é uma chamada tools/call em andamento.
type mcpCall struct {
	agent, server, tool string
	runID, span         string
	start               time.Time
	synthetic           bool
	sessKey             string
}

func (c *correlator) mcpCallStart(agent, server, tool, sessionKey string, args json.RawMessage) *mcpCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(agent)
	a.toolServer[tool] = server
	call := &mcpCall{agent: agent, server: server, tool: tool, start: time.Now()}

	// 1) casa com tool_call pendente do LLM (args iguais primeiro, depois só nome)
	var best *pendingCall
	var bestRun *llmRun
	exact := false
	for _, r := range a.runs {
		for _, p := range r.pending {
			if p.claimed || p.resolved || p.name != tool {
				continue
			}
			eq := jsonEqual(p.args, args)
			switch {
			case eq && !exact:
				best, bestRun, exact = p, r, true
			case eq == exact && (best == nil || r.lastSeen.After(bestRun.lastSeen)):
				best, bestRun = p, r
			}
		}
	}
	if best != nil {
		best.claimed = true
		call.runID, call.span = bestRun.id, best.span
		return call
	}

	// 2) sem LLM observado: run sintético por SESSÃO MCP (Mcp-Session-Id);
	// sem sessão (server stateless), por janela de atividade.
	key := sessionKey
	r := a.mcpRuns[key]
	if r != nil && (r.calls >= maxCallsPerRun || (key == "" && r.inflight == 0 && time.Since(r.lastSeen) > c.idleMCP)) {
		c.send(agent, &event.Event{Type: event.RunEnd, RunID: r.id}) // vira: run novo
		r = nil
	}
	if r == nil {
		r = &mcpRun{id: newID()}
		a.mcpRuns[key] = r
		tools := make([]event.ToolSpec, 0, len(a.tools))
		for _, t := range a.tools {
			tools = append(tools, t)
		}
		input := "(chamadas MCP diretas — o LLM não passa pelo mirante)"
		if key != "" {
			input = "sessão MCP " + shortID(key) + " (o LLM não passa pelo mirante)"
		}
		c.send(agent, &event.Event{Type: event.RunStart, RunID: r.id, Input: input, Tools: tools})
	}
	r.inflight++
	r.calls++
	r.lastSeen = call.start
	call.runID, call.span, call.synthetic, call.sessKey = r.id, newID(), true, key
	c.send(agent, &event.Event{Type: event.ToolCall, RunID: call.runID, SpanID: call.span, Server: server, Tool: tool, Args: args})
	return call
}

// mcpSessionEnd fecha o run da sessão quando o cliente encerra (DELETE).
func (c *correlator) mcpSessionEnd(agent, sessionKey string) {
	if sessionKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(agent)
	if r := a.mcpRuns[sessionKey]; r != nil {
		c.send(agent, &event.Event{Type: event.RunEnd, RunID: r.id})
		delete(a.mcpRuns, sessionKey)
	}
}

func (c *correlator) mcpCallDone(call *mcpCall, result json.RawMessage, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agent(call.agent)
	if r := a.mcpRuns[call.sessKey]; call.synthetic && r != nil && r.id == call.runID {
		r.inflight--
		r.lastSeen = time.Now()
	} else if !call.synthetic {
		// marca resolvido para o lado LLM não emitir um segundo retorno
		for _, r := range a.runs {
			for _, p := range r.pending {
				if p.span == call.span {
					p.resolved = true
				}
			}
		}
	}
	c.send(call.agent, &event.Event{Type: event.ToolResult, RunID: call.runID, SpanID: call.span, Server: call.server,
		Tool: call.tool, Result: result, Error: errMsg, DurationMs: ms(time.Since(call.start))})
}

// sweep fecha runs esquecidos. Chamado periodicamente.
func (c *correlator) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for name, a := range c.agents {
		for k, r := range a.runs {
			if now.Sub(r.lastSeen) > c.idleLLM {
				c.send(name, &event.Event{Type: event.RunEnd, RunID: r.id, Error: "abandonado: o LLM não devolveu resposta final"})
				delete(a.runs, k)
			}
		}
		for k, r := range a.mcpRuns {
			idle := c.idleMCP
			if k != "" {
				idle = c.idleSession // sessão sem DELETE (cliente morreu ou nunca fecha)
			}
			if r.inflight == 0 && now.Sub(r.lastSeen) > idle {
				c.send(name, &event.Event{Type: event.RunEnd, RunID: r.id})
				delete(a.mcpRuns, k)
			}
		}
	}
}

// ---------- util ----------

func turnKey(agent string, msgs []chatMsg, lastUser int) string {
	h := sha256.New()
	h.Write([]byte(agent))
	if lastUser < 0 {
		h.Write([]byte(newID())) // sem mensagem de usuário: cada requisição é um run
	}
	for _, m := range msgs[:lastUser+1] {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// jsonString devolve s como JSON: o próprio JSON se s já for objeto/array,
// senão uma string JSON.
func jsonString(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(s)
	return b
}

func looksLikeError(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "erro") || strings.HasPrefix(l, "error") || strings.HasPrefix(l, "falha")
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// maxCallsPerRun: sessão MCP longa (ex: poller) vira run novo a cada N
// chamadas, para nenhum run crescer sem limite no painel.
const maxCallsPerRun = 200

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
