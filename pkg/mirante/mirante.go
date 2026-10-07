// Package mirante é o SDK que um agente Go usa para mandar eventos ao painel.
//
// Regras de projeto:
//   - Nunca bloqueia nem derruba o agente: eventos vão para um buffer e são
//     enviados em lote por uma goroutine; buffer cheio ou painel fora do ar
//     = evento descartado em silêncio.
//   - Nil-safe: um *Client nil (painel desligado por config) vira no-op, então
//     a instrumentação pode ficar no código sem if em volta.
//
// Uso típico num loop de tool-calling:
//
//	mc := mirante.New(mirante.Config{URL: "http://mirante:8080", Agent: "wb-ce-agent"})
//	defer mc.Close()
//
//	run := mc.StartRun(msg, toolSpecs)
//	run.Decision(mirante.Decision{Model: "qwen2.5:7b", Chosen: []string{"troubleshoot"}, Duration: d})
//	call := run.ToolCall(mirante.Call{Server: "k8s-ts-mcp", Tool: "troubleshoot", Args: args, Rationale: motivo, Confidence: &conf})
//	call.End(result, err)
//	run.End(reply, nil)
package mirante

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type Config struct {
	URL   string // ex: http://mirante.observability:8080
	Agent string // nome do agente no mapa (ex: "alfred", "wb-ce-agent")
	Token string // opcional, se o servidor roda com --ingest-token

	BufferSize    int           // default 4096
	FlushInterval time.Duration // default 200ms
	HTTPClient    *http.Client  // default timeout 3s
}

type Client struct {
	cfg  Config
	ch   chan any
	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// New devolve nil se URL estiver vazia — e nil é um Client válido (no-op).
func New(cfg Config) *Client {
	if cfg.URL == "" {
		return nil
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 4096
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 200 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	c := &Client{cfg: cfg, ch: make(chan any, cfg.BufferSize), done: make(chan struct{})}
	c.wg.Add(1)
	go c.loop()
	return c
}

// Close envia o que estiver no buffer e encerra.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.once.Do(func() { close(c.done) })
	c.wg.Wait()
}

func (c *Client) emit(ev map[string]any) {
	if c == nil {
		return
	}
	ev["agent"] = c.cfg.Agent
	if _, ok := ev["time"]; !ok {
		ev["time"] = time.Now().UTC()
	}
	select {
	case c.ch <- ev:
	default: // buffer cheio: descarta, o agente é mais importante que o painel
	}
}

// Send enfileira um evento já montado (qualquer valor serializável no formato
// de POST /v1/events), sem tocar no campo agent. Usado pelo proxy em modo
// sidecar, que fala por vários agentes.
func (c *Client) Send(ev any) {
	if c == nil {
		return
	}
	select {
	case c.ch <- ev:
	default:
	}
}

func (c *Client) loop() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.FlushInterval)
	defer t.Stop()
	var batch []any
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b, _ := json.Marshal(batch)
		batch = batch[:0]
		req, err := http.NewRequest(http.MethodPost, c.cfg.URL+"/v1/events", bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		}
		if resp, err := c.cfg.HTTPClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	for {
		select {
		case ev := <-c.ch:
			batch = append(batch, ev)
			if len(batch) >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		case <-c.done:
			for {
				select {
				case ev := <-c.ch:
					batch = append(batch, ev)
				default:
					flush()
					return
				}
			}
		}
	}
}

// ToolSpec descreve uma tool disponível ao modelo. Mandar o Schema permite ao
// painel detectar argumentos inventados.
type ToolSpec struct {
	Server      string         `json:"server,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	// ReadOnly: false se a tool altera algo (reinicia, aplica, deleta…), mesmo
	// que o nome não diga — ex: troubleshoot do k8s-ts-mcp. Evita falso
	// positivo de "acao_nao_executada".
	ReadOnly *bool `json:"read_only,omitempty"`
}

// Run é uma requisição sendo atendida.
type Run struct {
	c     *Client
	id    string
	start time.Time
}

// StartRun abre um run. tools = o que foi oferecido ao modelo nesse turno.
func (c *Client) StartRun(input string, tools []ToolSpec) *Run {
	return c.StartRunAs("", input, tools)
}

// StartRunAs é StartRun com identificação de quem pediu (ex: usuário do Teams).
func (c *Client) StartRunAs(user, input string, tools []ToolSpec) *Run {
	if c == nil {
		return nil
	}
	r := &Run{c: c, id: newID(), start: time.Now()}
	c.emit(map[string]any{"type": "run_start", "run_id": r.id, "input": input, "user": user, "tools": tools})
	return r
}

// ID do run (útil pra logar junto e cruzar com o painel).
func (r *Run) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// Alternative é uma tool considerada e descartada.
type Alternative struct {
	Tool  string   `json:"tool"`
	Score *float64 `json:"score,omitempty"`
	Why   string   `json:"why,omitempty"`
}

// Decision é uma rodada do LLM: o que ele escolheu e por quê.
type Decision struct {
	Model        string
	Reasoning    string   // texto livre / thinking do modelo
	Chosen       []string // tools pedidas; vazio = respondeu direto
	Alternatives []Alternative
	Confidence   *float64
	TokensIn     int
	TokensOut    int
	Duration     time.Duration // quanto a chamada ao LLM levou
}

func (r *Run) Decision(d Decision) {
	if r == nil {
		return
	}
	r.c.emit(map[string]any{
		"type": "decision", "run_id": r.id, "span_id": newID(),
		"model": d.Model, "reasoning": d.Reasoning, "chosen": d.Chosen, "alternatives": d.Alternatives,
		"confidence": d.Confidence, "tokens_in": d.TokensIn, "tokens_out": d.TokensOut,
		"duration_ms": msf(d.Duration),
	})
}

// Call descreve uma chamada de tool.
type Call struct {
	Server     string // MCP server dono da tool (agrupa no mapa)
	Tool       string
	Args       any // map/struct/json.RawMessage
	Rationale  string
	Confidence *float64
}

// ToolSpan é uma chamada de tool em andamento.
type ToolSpan struct {
	r     *Run
	id    string
	call  Call
	start time.Time
}

func (r *Run) ToolCall(c Call) *ToolSpan {
	if r == nil {
		return nil
	}
	s := &ToolSpan{r: r, id: newID(), call: c, start: time.Now()}
	r.c.emit(map[string]any{
		"type": "tool_call", "run_id": r.id, "span_id": s.id,
		"server": c.Server, "tool": c.Tool, "args": c.Args,
		"rationale": c.Rationale, "confidence": c.Confidence,
	})
	return s
}

// End fecha a chamada. result pode ser string, map, struct…
func (s *ToolSpan) End(result any, err error) {
	if s == nil {
		return
	}
	ev := map[string]any{
		"type": "tool_result", "run_id": s.r.id, "span_id": s.id,
		"server": s.call.Server, "tool": s.call.Tool,
		"result": result, "duration_ms": msf(time.Since(s.start)),
	}
	if err != nil {
		ev["error"] = err.Error()
	}
	s.r.c.emit(ev)
}

// Flag marca o run como alucinação/incerteza por critério do próprio agente
// (ex: um LLM-judge rodando depois). level: "hallucination" | "uncertain".
func (r *Run) Flag(level, code, reason string) {
	if r == nil {
		return
	}
	r.c.emit(map[string]any{"type": "flag", "run_id": r.id,
		"flag": map[string]any{"level": level, "code": code, "reason": reason, "source": "agent"}})
}

// Flag numa chamada específica.
func (s *ToolSpan) Flag(level, code, reason string) {
	if s == nil {
		return
	}
	s.r.c.emit(map[string]any{"type": "flag", "run_id": s.r.id, "span_id": s.id,
		"flag": map[string]any{"level": level, "code": code, "reason": reason, "source": "agent"}})
}

func (r *Run) End(output string, err error) {
	if r == nil {
		return
	}
	ev := map[string]any{"type": "run_end", "run_id": r.id, "output": output, "duration_ms": msf(time.Since(r.start))}
	if err != nil {
		ev["error"] = err.Error()
	}
	r.c.emit(ev)
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Conf é um atalho pra passar confiança literal: Confidence: mirante.Conf(0.8).
func Conf(v float64) *float64 { return &v }
