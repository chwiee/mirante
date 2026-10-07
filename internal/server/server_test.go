package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/chwiee/mirante/internal/detect"
	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/internal/hub"
	"github.com/chwiee/mirante/internal/store"
	"github.com/chwiee/mirante/pkg/mirante"
)

func newServer(token string) (*Server, *httptest.Server) {
	s := &Server{Store: store.New(100, detect.Default), Hub: hub.New(), IngestToken: token, UI: fstest.MapFS{"index.html": {Data: []byte("ui")}}}
	return s, httptest.NewServer(s.Handler())
}

// Caminho real: SDK → POST /v1/events → store → SSE e GET /api/runs/{id}.
func TestSDKPontaAPonta(t *testing.T) {
	_, ts := newServer("segredo")
	defer ts.Close()

	// assina o SSE antes de gerar eventos
	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "event: ") {
				events <- strings.TrimPrefix(l, "event: ")
			}
		}
	}()
	if got := <-events; got != "snapshot" {
		t.Fatalf("primeiro evento SSE = %q, quer snapshot", got)
	}

	mc := mirante.New(mirante.Config{URL: ts.URL, Agent: "wb-ce-agent", Token: "segredo", FlushInterval: 10 * time.Millisecond})
	run := mc.StartRun("scan no eks-a", []mirante.ToolSpec{{Server: "k8s-ts-mcp", Name: "scan_cluster",
		Schema: map[string]any{"type": "object", "required": []any{"cluster_id"}, "properties": map[string]any{"cluster_id": map[string]any{"type": "string"}}}}})
	run.Decision(mirante.Decision{Model: "qwen2.5:7b", Chosen: []string{"restart_cluster"}, Confidence: mirante.Conf(0.9)})
	call := run.ToolCall(mirante.Call{Server: "k8s-ts-mcp", Tool: "restart_cluster", Args: map[string]any{"cluster_id": "eks-a"}, Rationale: "reiniciar"})
	call.End(nil, errors.New(`ferramenta "restart_cluster" não existe`))
	run.End("Reiniciei o eks-a e o pod api-99x subiu.", nil)
	mc.Close()

	for i := 0; i < 5; i++ {
		select {
		case got := <-events:
			if got != "update" {
				t.Fatalf("evento SSE %d = %q", i, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("só chegaram %d updates", i)
		}
	}

	r, err := http.Get(ts.URL + "/api/runs/" + run.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var got event.Run
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || len(got.Steps) != 2 || got.Steps[1].Status != "error" {
		t.Fatalf("run montado errado: %+v", got)
	}
	if string(got.Steps[1].Args) != `{"cluster_id":"eks-a"}` {
		t.Fatalf("payload perdido: %s", got.Steps[1].Args)
	}
	if got.Steps[1].Flags[0].Code != "tool_inexistente" {
		t.Fatalf("flags do step: %+v", got.Steps[1].Flags)
	}
	if got.Worst() != event.LevelHallucination || got.Flags[0].Code != "resposta_sem_base" || !strings.Contains(got.Flags[0].Reason, "api-99x") {
		t.Fatalf("flags do run: %+v", got.Flags)
	}
}

func TestIngestExigeToken(t *testing.T) {
	_, ts := newServer("segredo")
	defer ts.Close()
	r, _ := http.Post(ts.URL+"/v1/events", "application/json", strings.NewReader(`{"type":"run_start","run_id":"x"}`))
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestSDKNilEhNoop(t *testing.T) {
	mc := mirante.New(mirante.Config{}) // sem URL
	run := mc.StartRun("x", nil)
	run.Decision(mirante.Decision{})
	run.ToolCall(mirante.Call{Tool: "t"}).End("r", nil)
	run.Flag("hallucination", "x", "y")
	run.End("ok", nil)
	mc.Close()
}

func TestPayloadGrandeEhTruncado(t *testing.T) {
	s, ts := newServer("")
	defer ts.Close()
	big := `"` + strings.Repeat("a", store.MaxPayload+100) + `"`
	body := `[{"type":"tool_call","run_id":"r","span_id":"s","tool":"logs"},{"type":"tool_result","run_id":"r","span_id":"s","result":` + big + `}]`
	if r, _ := http.Post(ts.URL+"/v1/events", "application/json", strings.NewReader(body)); r.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", r.StatusCode)
	}
	run, _ := s.Store.Run("r")
	if len(run.Steps[0].Result) > store.MaxPayload+200 || !strings.Contains(string(run.Steps[0].Result), "_mirante_truncado") {
		t.Fatalf("resultado não truncado (%d bytes)", len(run.Steps[0].Result))
	}
}

// Regressão: POSTs concorrentes (um por agente) não podem publicar seq fora de
// ordem — o cliente descarta seq "velho" e perdia tool_result (timer travado).
func TestPublishEmOrdemComIngestConcorrente(t *testing.T) {
	s, ts := newServer("")
	defer ts.Close()
	ch := s.Hub.Subscribe()

	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			s.Ingest([]*event.Event{{Type: event.ToolCall, RunID: "r" + strconv.Itoa(i), Tool: "x"}})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	var last uint64
	for i := 0; i < n; i++ {
		msg := <-ch
		data := msg[strings.Index(string(msg), "data: ")+6:]
		var u store.Update
		if err := json.Unmarshal(data, &u); err != nil {
			t.Fatal(err)
		}
		if u.Seq <= last {
			t.Fatalf("seq fora de ordem: %d depois de %d", u.Seq, last)
		}
		last = u.Seq
	}
}

func TestInstrucaoIAServidaComURLReal(t *testing.T) {
	s, ts := newServer("")
	defer ts.Close()
	s.Upstreams = func() map[string]any {
		return map[string]any{"llm_urls": map[string]string{"ollama": "http://ollama:11434"}, "mcp_urls": map[string]string{}, "inject_reasoning": true}
	}
	get := func(path string, hdr map[string]string) string {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	md := get("/ia/integrar-mirante.md", nil)
	if !strings.Contains(md, "Gerado pelo mirante em **"+ts.URL+"**") || !strings.Contains(md, "`ollama` → `http://ollama:11434`") {
		t.Fatalf("doc viva sem URL/upstreams reais:\n%.400s", md)
	}
	// atrás de Ingress TLS, a URL tem que ser a pública, não a do pod
	md = get("/ia/claude/SKILL.md", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "mirante.empresa.com"})
	if !strings.HasPrefix(md, "---\nname: integrar-mirante") || !strings.Contains(md, "https://mirante.empresa.com/p/<agente>/llm/ollama") {
		t.Fatalf("skill atrás de ingress:\n%.300s", md)
	}
	if k := get("/ia/kiro/mirante.md", nil); !strings.HasPrefix(k, "---\ninclusion: manual") {
		t.Fatal("steering do Kiro sem frontmatter")
	}
	s.PublicURL = "http://mirante.observabilidade:8080/"
	if idx := get("/ia", nil); !strings.Contains(idx, "curl -s http://mirante.observabilidade:8080/ia/claude/SKILL.md -o .claude/skills/integrar-mirante/SKILL.md") {
		t.Fatalf("índice:\n%s", idx)
	}
}
