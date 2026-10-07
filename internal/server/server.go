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
	"time"

	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/internal/hub"
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
