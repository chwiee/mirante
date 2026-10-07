# mirante

Painel em tempo real dos agentes de IA, feito para o telão. **Qualquer agente se conecta só trocando URLs, sem alterar código.**

- **Mapa mental animado:** agentes no centro, MCP servers no meio, tools na borda. Cada chamada vira um pacote que sai do agente, passa pelo server, bate na tool e volta verde (ok) ou laranja (erro), com timer enquanto a tool roda.
- **Bolinha vermelha `!` (alucinação)** e **amarela `?` (incerteza)** no nó em que aconteceu. Uma tool inventada pelo modelo vira um nó *fantasma* tracejado.
- **Clique num nó:** chamadas, p50/p95/máx, % de erro, confiança média, histograma de latência e motivo de cada alerta.
- **Clique numa requisição:** linha do tempo, cada decisão do LLM (raciocínio, confiança, tokens, alternativas descartadas) e cada tool (**por quê**, **payload enviado**, **retorno**, duração), com botão de copiar.
- KPIs no topo e ticker de alertas embaixo. Binário único com a UI embutida e **sem CDN**.

## Começar em 1 minuto

```bash
go run ./cmd/mirante --demo        # tráfego sintético com as tools reais do ecossistema
```

Abra http://localhost:8080. No telão: `F` liga a tela cheia, `Esc` fecha o painel lateral e `?janela=30` muda a janela das métricas.

## Conectar um agente (plug and play)

Suba o mirante apontando para os destinos reais:

```bash
mirante --llm ollama=http://ollama:11434 --mcp k8s-ts-mcp=http://k8s-ts-mcp:8443 --inject-reasoning
```

Troque as URLs no agente. O nome do agente vai na própria URL:

```diff
- OLLAMA_URL=http://ollama:11434
+ OLLAMA_URL=http://mirante:8080/p/wb-ce-agent/llm/ollama
- MCP_SERVERS=k8s-ts-mcp=http://k8s-ts-mcp:8443
+ MCP_SERVERS=k8s-ts-mcp=http://mirante:8080/p/wb-ce-agent/mcp/k8s-ts-mcp
```

O botão **"+ Conectar agente"** no painel gera essas URLs. O mirante repassa tudo de forma transparente (Ollama `/api/chat`, OpenAI-compatible `/chat/completions`, MCP Streamable HTTP) e observa no caminho.

➡️ **Guia completo de implantação** (modos central e sidecar, Kubernetes, checklist de validação, segurança, problemas comuns): [`docs/IMPLANTACAO.md`](docs/IMPLANTACAO.md)

➡️ **Validar uma implantação:** `./scripts/validar.sh http://<mirante>`

➡️ **MCP que já roda no Kubernetes, com várias réplicas** (sessão MCP por pod): o mirante entra como outro Deployment, aponta para o Service headless e prende cada sessão ao pod certo. Ver [guia, seção 6](docs/IMPLANTACAO.md#6-mirante-na-frente-de-um-mcp-que-já-roda-no-kubernetes) e o diagnóstico `examples/mcp-sessoes`.

➡️ **Pedir para a IA integrar** (Claude Code, Kiro): o mirante serve a instrução pronta em `/ia`. Ver [guia, seção 4.4](docs/IMPLANTACAO.md#44-pedir-para-a-ia-fazer-claude-code-kiro).

➡️ **Agente de exemplo que não importa nada do mirante:** [`examples/agente-minimo`](examples/agente-minimo/main.go)

## Detecção de alucinação

| código | | quando |
|---|---|---|
| `tool_inexistente` | 🔴 | o modelo chamou uma tool que não foi oferecida |
| `args_invalidos` | 🔴 | argumento obrigatório faltando, inventado, com tipo errado ou fora do enum |
| `resposta_sem_base` | 🔴 | a resposta cita identificador (pod, IP, versão, `` `nome` ``) que não está na pergunta nem nos retornos |
| `acao_nao_executada` | 🔴 | o modelo afirma que fez algo ("registrei", "reiniciei") sem nenhuma tool capaz disso ter rodado |
| `baixa_confianca` · `escolha_ambigua` · `chamada_repetida` · `resposta_apos_erro` | 🟡 | sinais de escolha duvidosa |

Os detalhes e o ajuste fino (`--action-tools`, anotações MCP) estão na [seção 9 do guia](docs/IMPLANTACAO.md#9-detecção-de-alucinação).

## Modo SDK (Go)

Use quando quiser algo que o proxy não vê, como o *thinking* do Claude no Bedrock ou o veredito de um LLM-judge.

```go
import "github.com/chwiee/mirante/pkg/mirante"

mc := mirante.New(mirante.Config{URL: os.Getenv("MIRANTE_URL"), Agent: "alfred"}) // URL vazia = no-op
defer mc.Close()

run := mc.StartRun(pergunta, specs) // specs = []mirante.ToolSpec{Server, Name, Schema, ReadOnly}
run.Decision(mirante.Decision{Model: "claude-sonnet-5", Reasoning: thinking, Chosen: []string{"troubleshoot"}, Duration: d})
call := run.ToolCall(mirante.Call{Server: "k8s-ts-mcp", Tool: "troubleshoot", Args: args, Rationale: reason, Confidence: conf})
call.End(resultado, err)
run.Flag("hallucination", "judge", "o juiz discordou da resposta") // opcional
run.End(resposta, nil)
```

Comportamento: assíncrono e em lote, **nunca trava nem derruba o agente** (descarta eventos se o painel cair), e um `*Client` nil é no-op. Para pedir o "porquê" a modelos que não explicam, use `mirante.WithReasoningParams(schema)` nas tools e `mirante.SplitReasoning(args)` antes de executar. É o mesmo mecanismo que o proxy aplica com `--inject-reasoning`.

### Outras linguagens: HTTP direto

`POST /v1/events` aceita um objeto ou um array:

```json
[
  {"type":"run_start","run_id":"r1","agent":"alfred","input":"…","tools":[{"server":"k8s-ts-mcp","name":"scan_cluster","schema":{…},"read_only":true}]},
  {"type":"decision","run_id":"r1","span_id":"d1","model":"…","reasoning":"…","chosen":["scan_cluster"],"confidence":0.9,"duration_ms":812},
  {"type":"tool_call","run_id":"r1","span_id":"t1","server":"k8s-ts-mcp","tool":"scan_cluster","args":{…},"rationale":"…"},
  {"type":"tool_result","run_id":"r1","span_id":"t1","result":{…},"error":"","duration_ms":1530},
  {"type":"flag","run_id":"r1","flag":{"level":"uncertain","code":"judge","reason":"…"}},
  {"type":"run_end","run_id":"r1","output":"…"}
]
```

Leitura: `GET /api/runs`, `GET /api/runs/{id}` (com payloads), `GET /api/stream` (SSE) e `GET /api/upstreams`.

## Desenvolvimento

```bash
make test     # go vet + go test (inclui proxy de ponta a ponta com MCP server/cliente reais do go-sdk)
make demo
make docker
```

Validado ao vivo com `qwen2.5:7b` (Ollama) + `knowledge-mcp` real. Esse teste revelou e corrigiu: o modelo acentuar o nome do campo injetado (`confiança`), palavra composta do português virar falso "identificador inventado" e o modelo afirmar ações que não executou.

## Limites da v1

Estado em memória com 1 réplica. Heurística não pega afirmação falsa em texto corrido (use LLM-judge + `Flag`). LLM com requisição assinada (Bedrock) não passa pelo proxy: use proxy só de MCP ou o SDK. A lista completa está no [guia](docs/IMPLANTACAO.md#13-limitações-conhecidas-v1).
