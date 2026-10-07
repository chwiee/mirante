// Package server expõe a ingestão (/v1/events), a API de leitura (/api/*),
// o stream SSE (/api/stream) e a UI embutida.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/internal/hub"
	"github.com/chwiee/mirante/internal/iadoc"
	"github.com/chwiee/mirante/internal/store"
)

type Server struct {
	Store       *store.Store
	Hub         *hub.Hub
	IngestToken string // vazio = ingestão aberta (rede interna)
	UI          fs.FS
	Log         *slog.Logger

	// Mount registra rotas extras (ex: proxy plug-and-play) no mesmo mux.
	Mount func(*http.ServeMux)
	// Upstreams descreve os upstreams do proxy para a tela "conectar agente".
	Upstreams func() map[string]any
	// PublicURL é a URL pela qual os agentes alcançam este mirante (vai nas
	// instruções para IA em /ia). Vazio = deduz da requisição.
	PublicURL string

	// ingestMu torna apply+publish atômico: sem ele, dois POSTs concorrentes
	// podem publicar seq N+1 antes de N e o cliente descarta N como "velho".
	ingestMu sync.Mutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.ingest)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("GET /api/upstreams", func(w http.ResponseWriter, _ *http.Request) {
		if s.Upstreams == nil {
			writeJSON(w, map[string]any{"llm": []string{}, "mcp": []string{}})
			return
		}
		writeJSON(w, s.Upstreams())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	// instrução para assistentes de IA, já com URL e upstreams reais
	mux.HandleFunc("GET /ia", s.iaIndex)
	mux.HandleFunc("GET /ia/integrar-mirante.md", s.iaDoc(iadoc.Markdown))
	mux.HandleFunc("GET /ia/claude/SKILL.md", s.iaDoc(iadoc.Skill))
	mux.HandleFunc("GET /ia/kiro/mirante.md", s.iaDoc(iadoc.Steering))
	static := http.FileServerFS(s.UI)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // telão pega a UI nova a cada deploy
		static.ServeHTTP(w, r)
	})
	if s.Mount != nil {
		s.Mount(mux)
	}
	return mux
}

// Ingest aplica eventos direto (usado pelo modo demo e pelos testes).
func (s *Server) Ingest(evs []*event.Event) {
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	for _, e := range evs {
		if e.Type == "" || (e.RunID == "" && e.Type != event.ToolsEvt) {
			continue
		}
		u := s.Store.Apply(e)
		b, _ := json.Marshal(u)
		s.Hub.Publish(sse("update", b))
	}
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if s.IngestToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.IngestToken)) != 1 {
			http.Error(w, "token inválido", http.StatusUnauthorized)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var evs []*event.Event
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "[") {
		err = json.Unmarshal(body, &evs)
	} else {
		var e event.Event
		err = json.Unmarshal(body, &e)
		evs = []*event.Event{&e}
	}
	if err != nil {
		http.Error(w, "json inválido: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.Ingest(evs)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) listRuns(w http.ResponseWriter, _ *http.Request) {
	runs, _ := s.Store.Runs()
	writeJSON(w, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.Store.Run(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, run)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming não suportado", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx/ingress não bufferizar

	ch := s.Hub.Subscribe()
	defer s.Hub.Unsubscribe(ch)

	runs, seq := s.Store.Runs()
	snap, _ := json.Marshal(map[string]any{"seq": seq, "runs": runs, "topology": s.Store.Topology()})
	w.Write(sse("snapshot", snap))
	fl.Flush()

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		case <-tick.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func sse(name string, data []byte) []byte {
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// baseURL é a URL pública do mirante: --public-url, ou o que o cliente usou
// para chegar aqui (respeitando X-Forwarded-* atrás de Ingress).
func (s *Server) baseURL(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimRight(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

func (s *Server) iaData(r *http.Request) iadoc.Data {
	up := map[string]any{}
	if s.Upstreams != nil {
		up = s.Upstreams()
	}
	return iadoc.FromUpstreams(s.baseURL(r), up)
}

func (s *Server) iaDoc(render func(iadoc.Data) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		md, err := render(s.iaData(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, md)
	}
}

// iaIndex explica, para humanos e IAs, como usar a instrução.
func (s *Server) iaIndex(w http.ResponseWriter, r *http.Request) {
	b := s.baseURL(r)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	fmt.Fprintf(w, `# mirante — integração por assistente de IA

Peça ao seu assistente, dentro do repositório do agente:

> Leia a instrução com `+"`curl -s %[1]s/ia/integrar-mirante.md`"+` e integre o mirante neste agente seguindo-a.

Para deixar a instrução instalada no repositório (o assistente acha sozinho da próxima vez):

Claude Code (skill — dispara com "integra o mirante"):

    mkdir -p .claude/skills/integrar-mirante && curl -s %[1]s/ia/claude/SKILL.md -o .claude/skills/integrar-mirante/SKILL.md

Kiro (steering manual — use #mirante no chat):

    mkdir -p .kiro/steering && curl -s %[1]s/ia/kiro/mirante.md -o .kiro/steering/mirante.md

Arquivos:
- %[1]s/ia/integrar-mirante.md — instrução (Markdown puro)
- %[1]s/ia/claude/SKILL.md — formato skill do Claude Code
- %[1]s/ia/kiro/mirante.md — formato steering do Kiro
`, b)
}

// AsyncIngest devolve um emissor que NÃO roda no caminho da requisição: o
// evento entra numa fila e uma goroutine aplica (store + SSE) em ordem. Fila
// cheia = evento descartado e contado — o MCP/LLM nunca espera o painel.
// Medido: aplicar o evento dentro da requisição custava ~0,4 ms por chamada
// e serializava as requisições concorrentes num lock global.
func (s *Server) AsyncIngest(buffer int) (emit func(*event.Event), dropped func() uint64) {
	ch := make(chan *event.Event, buffer)
	var drops atomic.Uint64
	go func() {
		for e := range ch {
			s.Ingest([]*event.Event{e})
		}
	}()
	emit = func(e *event.Event) {
		select {
		case ch <- e:
		default:
			if drops.Add(1)%1000 == 1 && s.Log != nil {
				s.Log.Warn("painel atrasado: eventos descartados (o tráfego do agente/MCP não foi afetado)", "total", drops.Load())
			}
		}
	}
	return emit, drops.Load
}
