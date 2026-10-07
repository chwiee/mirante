// Package store mantém em memória os últimos N runs, montados a partir dos
// eventos, e roda as heurísticas de detect conforme cada evento chega.
//
// Em memória de propósito (v1): o telão mostra o agora. Histórico longo é
// trabalho pra um backend de verdade (ver README, "Próximos passos").
package store

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/chwiee/mirante/internal/detect"
	"github.com/chwiee/mirante/internal/event"
)

// MaxPayload limita args/result guardados por step, pra um resultado gigante
// (ex: log inteiro de pod) não estourar memória nem o navegador.
const MaxPayload = 32 << 10

// Update é o que o store devolve a cada evento aplicado — vai direto pro SSE.
type Update struct {
	Seq   uint64       `json:"seq"` // monotônico: o cliente descarta update fora de ordem
	Event *event.Event `json:"event"`
	Run   *event.Run   `json:"run,omitempty"` // versão "leve" (sem payloads); nil em "tools"
}

type Store struct {
	mu     sync.Mutex
	max    int
	det    detect.Config
	runs   map[string]*event.Run
	order  []string // mais antigo primeiro
	spanIx map[string]*event.Step
	topo   map[topoKey]bool // (agente, server, tool) → declarada no run_start?
	seq    uint64
}

func New(max int, det detect.Config) *Store {
	return &Store{max: max, det: det, runs: map[string]*event.Run{}, spanIx: map[string]*event.Step{}, topo: map[topoKey]bool{}}
}

// Apply incorpora um evento e devolve a visão atualizada do run.
func (s *Store) Apply(e *event.Event) Update {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Args = clip(e.Args)
	e.Result = clip(e.Result)

	if e.Type == event.ToolsEvt {
		// só topologia: o agente aparece no mapa ligado às tools assim que
		// conecta, antes da primeira pergunta.
		for _, t := range e.Tools {
			if k := (topoKey{e.Agent, t.Server, t.Name}); !s.topo[k] {
				s.topo[k] = true
			}
		}
		s.seq++
		return Update{Seq: s.seq, Event: lightEvent(e)}
	}

	run := s.runs[e.RunID]
	if run == nil {
		// run_start perdido (ou agente que só manda tool_call) — cria implícito.
		run = &event.Run{ID: e.RunID, Agent: e.Agent, Start: e.Time, Status: "running", Steps: []*event.Step{}}
		s.add(run)
	}

	switch e.Type {
	case event.RunStart:
		for _, t := range e.Tools {
			s.topo[topoKey{e.Agent, t.Server, t.Name}] = true
		}
		run.Input, run.User, run.Tools, run.Start = e.Input, e.User, e.Tools, e.Time
		if e.Agent != "" {
			run.Agent = e.Agent
		}

	case event.Decision:
		st := &event.Step{
			SpanID: s.spanID(e, run), ParentID: e.ParentID, Kind: "decision",
			Model: e.Model, Reasoning: e.Reasoning, Chosen: e.Chosen, Alternatives: e.Alternatives,
			Confidence: e.Confidence, TokensIn: e.TokensIn, TokensOut: e.TokensOut,
			Start: e.Time.Add(-ms(e.DurationMs)), DurationMs: e.DurationMs, Status: "ok",
		}
		st.Flags = s.det.Decision(st)
		s.addStep(run, st)

	case event.ToolCall:
		st := &event.Step{
			SpanID: s.spanID(e, run), ParentID: e.ParentID, Kind: "tool",
			Server: e.Server, Tool: e.Tool, Args: e.Args, Rationale: e.Rationale, Confidence: e.Confidence,
			Start: e.Time, Status: "running",
		}
		s.addStep(run, st)
		st.Flags = s.det.ToolCall(run, st)
		if k := (topoKey{run.Agent, e.Server, e.Tool}); !s.topo[k] {
			// "fantasma" só quando havia uma lista de tools oferecidas e esta não
			// estava nela. Sem lista (cliente MCP que não chamou tools/list, SDK
			// sem tools no run_start) não dá para afirmar que é inventada.
			s.topo[k] = !offered(run.Tools) || declaredIn(run.Tools, e.Server, e.Tool)
		}

	case event.ToolResult:
		st := s.spanIx[e.SpanID]
		if st == nil {
			// tool_result sem tool_call: monta o step a partir do que veio.
			st = &event.Step{SpanID: s.spanID(e, run), Kind: "tool", Server: e.Server, Tool: e.Tool,
				Start: e.Time.Add(-ms(e.DurationMs))}
			s.addStep(run, st)
		}
		st.Result, st.Error = e.Result, e.Error
		st.DurationMs = e.DurationMs
		if st.DurationMs == 0 {
			st.DurationMs = float64(e.Time.Sub(st.Start).Microseconds()) / 1000
		}
		st.Status = "ok"
		if e.Error != "" {
			st.Status = "error"
		}

	case event.FlagEvt:
		if e.Flag != nil {
			f := *e.Flag
			if f.Source == "" {
				f.Source = "agent"
			}
			if st := s.spanIx[e.SpanID]; st != nil {
				st.Flags = append(st.Flags, f)
			} else {
				run.Flags = append(run.Flags, f)
			}
		}

	case event.RunEnd:
		run.Output, run.Error = e.Output, e.Error
		run.DurationMs = float64(e.Time.Sub(run.Start).Microseconds()) / 1000
		if e.DurationMs > 0 {
			run.DurationMs = e.DurationMs
		}
		run.Status = "ok"
		if e.Error != "" {
			run.Status = "error"
		}
		run.Flags = append(run.Flags, s.det.RunEnd(run)...)
	}

	s.seq++
	return Update{Seq: s.seq, Event: lightEvent(e), Run: Light(run)}
}

// Runs devolve a versão leve de todos os runs guardados (mais novo primeiro)
// e o seq atual, pra o cliente ignorar updates anteriores ao snapshot.
func (s *Store) Runs() ([]*event.Run, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*event.Run, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		out = append(out, Light(s.runs[s.order[i]]))
	}
	return out, s.seq
}

// Run devolve uma cópia completa (com payloads) de um run.
func (s *Store) Run(id string) (*event.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	b, _ := json.Marshal(r)
	var cp event.Run
	_ = json.Unmarshal(b, &cp)
	return &cp, true
}

func (s *Store) add(r *event.Run) {
	s.runs[r.ID] = r
	s.order = append(s.order, r.ID)
	for len(s.order) > s.max {
		old := s.runs[s.order[0]]
		for _, st := range old.Steps {
			delete(s.spanIx, st.SpanID)
		}
		delete(s.runs, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *Store) addStep(r *event.Run, st *event.Step) {
	r.Steps = append(r.Steps, st)
	s.spanIx[st.SpanID] = st
}

func (s *Store) spanID(e *event.Event, r *event.Run) string {
	if e.SpanID != "" {
		return e.SpanID
	}
	return r.ID + "/" + strconv.Itoa(len(r.Steps))
}

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func clip(raw json.RawMessage) json.RawMessage {
	if len(raw) <= MaxPayload {
		return raw
	}
	b, _ := json.Marshal(map[string]any{
		"_mirante_truncado": true,
		"bytes_originais":   len(raw),
		"inicio":            string(raw[:MaxPayload]),
	})
	return b
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Light copia o run sem payloads e com textos encurtados — é o que trafega no
// SSE e no snapshot. Detalhe completo vem de GET /api/runs/{id}.
func Light(r *event.Run) *event.Run {
	cp := *r
	cp.Input, cp.Output = trunc(r.Input, 280), trunc(r.Output, 280)
	cp.Tools = nil
	cp.Flags = append([]event.Flag(nil), r.Flags...)
	cp.Steps = make([]*event.Step, len(r.Steps))
	for i, st := range r.Steps {
		c := *st
		c.Args, c.Result, c.Alternatives = nil, nil, nil
		c.Reasoning, c.Rationale = trunc(st.Reasoning, 160), trunc(st.Rationale, 160)
		c.Flags = append([]event.Flag(nil), st.Flags...)
		cp.Steps[i] = &c
	}
	return &cp
}

func lightEvent(e *event.Event) *event.Event {
	c := *e
	c.Args, c.Result, c.Alternatives = nil, nil, nil
	c.Tools = nil
	for _, t := range e.Tools {
		c.Tools = append(c.Tools, event.ToolSpec{Server: t.Server, Name: t.Name}) // sem schema
	}
	c.Input, c.Output, c.Reasoning, c.Rationale = trunc(e.Input, 280), trunc(e.Output, 280), "", ""
	return &c
}

type topoKey struct{ Agent, Server, Tool string }

// TopoEdge é uma aresta agente → server → tool do mapa mental.
type TopoEdge struct {
	Agent    string `json:"agent"`
	Server   string `json:"server"`
	Tool     string `json:"tool"`
	Declared bool   `json:"declared"` // false = modelo chamou algo que não foi oferecido
}

// Topology devolve todas as arestas já vistas (não expira com o ring de runs:
// o mapa não deve "perder" uma tool só porque ela ficou um tempo sem uso).
func (s *Store) Topology() []TopoEdge {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TopoEdge, 0, len(s.topo))
	for k, d := range s.topo {
		out = append(out, TopoEdge{k.Agent, k.Server, k.Tool, d})
	}
	return out
}

func offered(tools []event.ToolSpec) bool { return len(tools) > 0 }

func declaredIn(tools []event.ToolSpec, server, name string) bool {
	for _, t := range tools {
		if t.Name == name && (server == "" || t.Server == "" || t.Server == server) {
			return true
		}
	}
	return false
}
