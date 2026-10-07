// Package demo gera tráfego sintético com as tools reais do ecossistema
// (k8s-ts-mcp, knowledge-mcp, ce-k8s-mcp, rabbitmq-mcp) passando pelo SDK de
// verdade — serve pra ver o painel no telão antes de instrumentar os agentes,
// e como teste de ponta a ponta do caminho SDK → HTTP → store → SSE.
package demo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/chwiee/mirante/pkg/mirante"
)

func obj(required []string, props map[string]any) map[string]any {
	r := make([]any, len(required))
	for i, s := range required {
		r[i] = s
	}
	return map[string]any{"type": "object", "properties": props, "required": r, "additionalProperties": false}
}

var str = map[string]any{"type": "string"}

// acts marca tools que executam ações (troubleshoot deleta pod, approve_action
// aplica a mudança) — como o k8s-ts-mcp anotaria com readOnlyHint=false.
var acts = false

var (
	k8s = []mirante.ToolSpec{
		{Server: "k8s-ts-mcp", Name: "list_clusters", Schema: obj(nil, map[string]any{})},
		{Server: "k8s-ts-mcp", Name: "scan_cluster", Schema: obj([]string{"cluster_id"}, map[string]any{"cluster_id": str})},
		{Server: "k8s-ts-mcp", Name: "troubleshoot", ReadOnly: &acts, Schema: obj([]string{"cluster_id", "signal"}, map[string]any{"cluster_id": str, "signal": str, "namespace": str})},
		{Server: "k8s-ts-mcp", Name: "approve_action", ReadOnly: &acts, Schema: obj([]string{"incident_id", "action_name"}, map[string]any{"incident_id": str, "action_name": str})},
		{Server: "k8s-ts-mcp", Name: "get_postmortem", Schema: obj([]string{"incident_id"}, map[string]any{"incident_id": str})},
	}
	know = []mirante.ToolSpec{
		{Server: "knowledge-mcp", Name: "search_knowledge", Schema: obj([]string{"query"}, map[string]any{"query": str, "source_filter": str})},
		{Server: "knowledge-mcp", Name: "record_lesson", Schema: obj([]string{"signal", "resolution"}, map[string]any{"signal": str, "context": str, "resolution": str, "outcome": str})},
	}
	ce = []mirante.ToolSpec{
		{Server: "ce-k8s-mcp", Name: "logs", Schema: obj([]string{"cluster", "namespace"}, map[string]any{"cluster": str, "namespace": str, "pod": str, "tail": map[string]any{"type": "integer"}})},
		{Server: "ce-k8s-mcp", Name: "status", Schema: obj([]string{"cluster", "namespace"}, map[string]any{"cluster": str, "namespace": str})},
		{Server: "ce-k8s-mcp", Name: "events", Schema: obj([]string{"cluster", "namespace"}, map[string]any{"cluster": str, "namespace": str})},
	}
	rabbit = []mirante.ToolSpec{
		{Server: "rabbitmq-mcp", Name: "queue_depth", Schema: obj([]string{"vhost", "queue"}, map[string]any{"vhost": str, "queue": str})},
		{Server: "rabbitmq-mcp", Name: "consumers", Schema: obj([]string{"vhost", "queue"}, map[string]any{"vhost": str, "queue": str})},
	}
)

func cat(lists ...[]mirante.ToolSpec) []mirante.ToolSpec {
	var out []mirante.ToolSpec
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

type agent struct {
	name, model string
	tools       []mirante.ToolSpec
	llm         [2]int // latência do LLM em ms (min, max)
	c           *mirante.Client
}

// Run gera tráfego até ctx acabar.
func Run(ctx context.Context, url string) {
	agents := []*agent{
		{name: "alfred", model: "claude-sonnet-5 (bedrock)", tools: cat(k8s, know, rabbit), llm: [2]int{700, 1800}},
		{name: "wb-ce-agent", model: "qwen2.5:7b", tools: cat(k8s, know), llm: [2]int{900, 2600}},
		{name: "ce-aut", model: "qwen2.5:7b", tools: ce, llm: [2]int{600, 1500}},
	}
	for _, a := range agents {
		a.c = mirante.New(mirante.Config{URL: url, Agent: a.name})
		defer a.c.Close()
	}

	type scenario struct {
		w  int
		fn func(*agent)
		ok func(*agent) bool
	}
	has := func(server string) func(*agent) bool {
		return func(a *agent) bool {
			for _, t := range a.tools {
				if t.Server == server {
					return true
				}
			}
			return false
		}
	}
	scenarios := []scenario{
		{30, troubleshootOK, has("k8s-ts-mcp")},
		{22, logsOK, has("ce-k8s-mcp")},
		{10, rabbitOK, has("rabbitmq-mcp")},
		{6, hallucinatedTool, has("k8s-ts-mcp")},
		{6, invalidArgs, has("k8s-ts-mcp")},
		{7, lowConfidence, has("ce-k8s-mcp")},
		{6, ungroundedAnswer, has("k8s-ts-mcp")},
		{5, errorThenConfident, has("ce-k8s-mcp")},
		{4, loop, has("knowledge-mcp")},
		{6, ambiguous, has("k8s-ts-mcp")},
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(900+rand.IntN(2600)) * time.Millisecond):
		}
		a := agents[rand.IntN(len(agents))]
		var pool []scenario
		total := 0
		for _, s := range scenarios {
			if s.ok(a) {
				pool = append(pool, s)
				total += s.w
			}
		}
		n := rand.IntN(total)
		for _, s := range pool {
			if n -= s.w; n < 0 {
				go s.fn(a)
				break
			}
		}
	}
}

// ---------- helpers ----------

var clusters = []string{"eks-prd-pagamentos-01", "eks-prd-checkout-02", "eks-hml-core-01", "eks-prd-catalogo-03", "eks-prd-auth-01"}
var namespaces = []string{"payments", "checkout", "catalog", "auth", "keda-system"}

func pick[T any](xs []T) T { return xs[rand.IntN(len(xs))] }

func (a *agent) think(run *mirante.Run, reasoning string, chosen []string, conf float64, alts ...mirante.Alternative) {
	d := time.Duration(a.llm[0]+rand.IntN(a.llm[1]-a.llm[0])) * time.Millisecond
	time.Sleep(d)
	run.Decision(mirante.Decision{
		Model: a.model, Reasoning: reasoning, Chosen: chosen, Alternatives: alts,
		Confidence: mirante.Conf(conf), TokensIn: 800 + rand.IntN(3000), TokensOut: 40 + rand.IntN(300), Duration: d,
	})
}

func (a *agent) call(run *mirante.Run, server, tool string, args map[string]any, why string, conf float64, lat [2]int, result any, err error) {
	sp := run.ToolCall(mirante.Call{Server: server, Tool: tool, Args: args, Rationale: why, Confidence: mirante.Conf(conf)})
	time.Sleep(time.Duration(lat[0]+rand.IntN(lat[1]-lat[0])) * time.Millisecond)
	sp.End(result, err)
}

func hi() float64 { return 0.78 + rand.Float64()*0.2 }

// ---------- cenários saudáveis ----------

func troubleshootOK(a *agent) {
	cl, ns := pick(clusters), pick(namespaces)
	pod := fmt.Sprintf("%s-api-%x", ns, rand.IntN(0xfffff))
	inc := fmt.Sprintf("INC-%d", 4000+rand.IntN(999))
	input := fmt.Sprintf("o %s no %s tá reiniciando sem parar, consegue ver?", ns, cl)
	run := a.c.StartRunAs("joana.silva", input, a.tools)

	a.think(run, "Usuário relata restart em loop. Antes de diagnosticar do zero, consulto a base de lições pra ver se já vimos isso.",
		[]string{"search_knowledge"}, hi(), mirante.Alternative{Tool: "troubleshoot", Score: mirante.Conf(0.55), Why: "diagnóstico direto, mas pode repetir trabalho já feito"})
	a.call(run, "knowledge-mcp", "search_knowledge", map[string]any{"query": "CrashLoopBackOff OOMKilled " + ns},
		"Base de lições primeiro: se já houver solução registrada, economiza o diagnóstico.", hi(), [2]int{60, 250},
		map[string]any{"hits": 1, "top": map[string]any{"signal": "OOMKilled", "resolution": "aumentar limit de memória do deployment", "score": 0.71}}, nil)

	a.think(run, "Há lição parecida (OOMKilled). Confirmo no cluster com troubleshoot antes de sugerir.", []string{"troubleshoot"}, hi())
	a.call(run, "k8s-ts-mcp", "troubleshoot", map[string]any{"cluster_id": cl, "signal": "CrashLoopBackOff", "namespace": ns},
		"Confirmar a causa real no cluster informado pelo usuário.", hi(), [2]int{1200, 4200},
		map[string]any{"incident_id": inc, "cluster_id": cl, "pod": pod, "cause": "OOMKilled", "memory_limit": "256Mi",
			"proposed": []string{"delete pod (safe, executado)", "raise memory limit 256Mi→384Mi (high, requer approve_action)"}}, nil)

	a.think(run, "Diagnóstico confirmado. Registro a lição e respondo com a ação que precisa de aprovação.", []string{"record_lesson"}, hi())
	a.call(run, "knowledge-mcp", "record_lesson", map[string]any{"signal": "OOMKilled", "context": cl + "/" + ns, "resolution": "raise memory limit", "outcome": "pending_approval"},
		"Fechar o loop de aprendizado.", hi(), [2]int{40, 160}, map[string]any{"ok": true}, nil)

	a.think(run, "Respondo.", nil, hi())
	run.End(fmt.Sprintf("O pod %s no %s foi OOMKilled (limit 256Mi). Já reiniciei o pod; pra subir o limit pra 384Mi preciso de aprovação: approve_action %s.", pod, cl, inc), nil)
}

func logsOK(a *agent) {
	cl, ns := pick(clusters), pick(namespaces)
	run := a.c.StartRunAs("rafael.costa", fmt.Sprintf("me mostra o status do %s no %s", ns, cl), a.tools)
	a.think(run, "Pedido explícito de status; tool status do ce-k8s-mcp responde direto.", []string{"status"}, hi(),
		mirante.Alternative{Tool: "events", Score: mirante.Conf(0.3), Why: "eventos não dão o estado atual dos pods"})
	ready := 2 + rand.IntN(4)
	a.call(run, "ce-k8s-mcp", "status", map[string]any{"cluster": cl, "namespace": ns}, "Status atual dos pods no namespace pedido.", hi(), [2]int{150, 700},
		map[string]any{"pods": ready, "ready": ready, "restarts_24h": 0}, nil)
	a.think(run, "Tudo pronto, respondo.", nil, hi())
	run.End(fmt.Sprintf("%s no %s: %d/%d pods Ready, nenhum restart nas últimas 24h.", ns, cl, ready, ready), nil)
}

func rabbitOK(a *agent) {
	q := pick([]string{"pagamentos.aprovados", "checkout.pedidos", "notificacoes.email"})
	run := a.c.StartRunAs("time-sre", "a fila "+q+" tá acumulando?", a.tools)
	a.think(run, "Pergunta sobre profundidade de fila: queue_depth.", []string{"queue_depth"}, hi())
	depth := rand.IntN(40000)
	a.call(run, "rabbitmq-mcp", "queue_depth", map[string]any{"vhost": "/prd", "queue": q}, "Medir backlog atual da fila.", hi(), [2]int{80, 400},
		map[string]any{"messages": depth, "rate_in": 120, "rate_out": 118}, nil)
	a.think(run, "Verifico consumidores pra explicar o backlog.", []string{"consumers"}, hi())
	a.call(run, "rabbitmq-mcp", "consumers", map[string]any{"vhost": "/prd", "queue": q}, "Ver se faltam consumidores.", hi(), [2]int{80, 300},
		map[string]any{"consumers": 4}, nil)
	a.think(run, "Respondo.", nil, hi())
	run.End(fmt.Sprintf("A fila %s tem %d mensagens, entrada 120/s e saída 118/s com 4 consumidores — está estável, acumulando devagar.", q, depth), nil)
}

// ---------- cenários com problema ----------

func hallucinatedTool(a *agent) {
	cl := pick(clusters)
	run := a.c.StartRunAs("joana.silva", "reinicia o "+cl+" inteiro pra mim", a.tools)
	a.think(run, "Usuário quer reiniciar o cluster. Vou usar restart_cluster.", []string{"restart_cluster"}, 0.82)
	a.call(run, "k8s-ts-mcp", "restart_cluster", map[string]any{"cluster_id": cl}, "Reiniciar o cluster pedido.", 0.82, [2]int{20, 60},
		nil, errors.New(`ferramenta "restart_cluster" não existe`))
	a.think(run, "Ferramenta não existe; explico ao usuário.", nil, 0.7)
	run.End("Não tenho uma ferramenta para reiniciar o cluster inteiro — posso rodar scan_cluster ou troubleshoot num sinal específico. Deu erro ao tentar.", nil)
}

func invalidArgs(a *agent) {
	cl := pick(clusters)
	run := a.c.StartRunAs("rafael.costa", "faz um scan no "+cl, a.tools)
	a.think(run, "Scan pedido explicitamente.", []string{"scan_cluster"}, 0.9)
	a.call(run, "k8s-ts-mcp", "scan_cluster", map[string]any{"cluster": cl, "deep": true}, "Scan completo do cluster.", 0.9, [2]int{30, 90},
		nil, errors.New("cluster_id é obrigatório"))
	a.think(run, "Corrijo o nome do argumento e tento de novo.", []string{"scan_cluster"}, 0.88)
	a.call(run, "k8s-ts-mcp", "scan_cluster", map[string]any{"cluster_id": cl}, "Scan com o argumento correto.", 0.88, [2]int{900, 3000},
		map[string]any{"cluster_id": cl, "findings": 2, "signals": []string{"KEDAScaledObjectStuck", "ImagePullBackOff"}}, nil)
	a.think(run, "Respondo.", nil, 0.9)
	run.End(fmt.Sprintf("Scan do %s: 2 achados — KEDAScaledObjectStuck e ImagePullBackOff. Quer que eu rode troubleshoot em algum?", cl), nil)
}

func lowConfidence(a *agent) {
	cl, ns := pick(clusters), pick(namespaces)
	run := a.c.StartRunAs("time-sre", "tá lento o "+ns+", o que pode ser?", a.tools)
	a.think(run, "\"Lento\" é vago. Talvez eventos mostrem algo, mas não tenho certeza de que é a ferramenta certa.",
		[]string{"events"}, 0.38, mirante.Alternative{Tool: "logs", Score: mirante.Conf(0.35), Why: "logs podem ter latência, mas não sei qual pod"})
	a.call(run, "ce-k8s-mcp", "events", map[string]any{"cluster": cl, "namespace": ns}, "Talvez haja eventos de throttling ou HPA.", 0.38, [2]int{200, 800},
		map[string]any{"events": []string{"Normal Scheduled", "Normal Pulled"}}, nil)
	a.think(run, "Nada conclusivo.", nil, 0.4)
	run.End("Não encontrei eventos anormais no "+ns+". Lentidão pode estar fora do Kubernetes — vale olhar APM/banco.", nil)
}

func ungroundedAnswer(a *agent) {
	cl := pick(clusters)
	run := a.c.StartRunAs("joana.silva", "quais problemas tem no "+cl+"?", a.tools)
	a.think(run, "Scan do cluster responde isso.", []string{"scan_cluster"}, 0.92)
	a.call(run, "k8s-ts-mcp", "scan_cluster", map[string]any{"cluster_id": cl}, "Listar sinais ativos.", 0.92, [2]int{900, 3000},
		map[string]any{"cluster_id": cl, "findings": 1, "signals": []string{"ImagePullBackOff"}, "pod": "catalog-web-6c9d8"}, nil)
	a.think(run, "Respondo.", nil, 0.9)
	// "payments-worker-77f4b" e "ip-10-42-3-17" não vieram de lugar nenhum.
	run.End(fmt.Sprintf("No %s o pod catalog-web-6c9d8 está em ImagePullBackOff, e o payments-worker-77f4b está sem memória no nó ip-10-42-3-17.", cl), nil)
}

func errorThenConfident(a *agent) {
	cl, ns := pick(clusters), pick(namespaces)
	run := a.c.StartRunAs("rafael.costa", "tem erro nos logs do "+ns+"?", a.tools)
	a.think(run, "Pedido de logs.", []string{"logs"}, 0.9)
	a.call(run, "ce-k8s-mcp", "logs", map[string]any{"cluster": cl, "namespace": ns, "tail": 200}, "Ler logs recentes.", 0.9, [2]int{3000, 5000},
		nil, errors.New("context deadline exceeded"))
	a.think(run, "Respondo.", nil, 0.85)
	run.End("Os logs do "+ns+" estão limpos, sem nada fora do normal.", nil)
}

func loop(a *agent) {
	run := a.c.StartRunAs("time-sre", "já vimos esse ExecFormatError antes?", a.tools)
	for i := 0; i < 3; i++ {
		a.think(run, "Busco na base.", []string{"search_knowledge"}, 0.7)
		a.call(run, "knowledge-mcp", "search_knowledge", map[string]any{"query": "exec format error"}, "Procurar lição anterior.", 0.7, [2]int{60, 200},
			map[string]any{"hits": 0}, nil)
	}
	a.think(run, "Respondo.", nil, 0.6)
	run.End("Não encontrei lições sobre exec format error na base.", nil)
}

func ambiguous(a *agent) {
	cl := pick(clusters)
	inc := fmt.Sprintf("INC-%d", 4000+rand.IntN(999))
	run := a.c.StartRunAs("joana.silva", "o que aconteceu no "+inc+"?", a.tools)
	a.think(run, "Pode ser pedido de postmortem ou de novo diagnóstico — ambos fazem sentido.", []string{"get_postmortem"}, 0.58,
		mirante.Alternative{Tool: "troubleshoot", Score: mirante.Conf(0.54), Why: "talvez o usuário queira re-diagnosticar"})
	a.call(run, "k8s-ts-mcp", "get_postmortem", map[string]any{"incident_id": inc}, "Incidente citado pelo ID → postmortem.", 0.58, [2]int{100, 500},
		map[string]any{"incident_id": inc, "cluster_id": cl, "summary": "OOMKilled em checkout-api, limit ajustado 256Mi→384Mi"}, nil)
	a.think(run, "Respondo.", nil, 0.7)
	run.End(fmt.Sprintf("%s (%s): OOMKilled em checkout-api, limit ajustado 256Mi→384Mi.", inc, cl), nil)
}
