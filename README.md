# mirante

[![CI](https://github.com/chwiee/mirante/actions/workflows/ci.yml/badge.svg)](https://github.com/chwiee/mirante/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/chwiee/mirante.svg)](https://pkg.go.dev/github.com/chwiee/mirante/pkg/mirante)
[![Licença: Apache-2.0](https://img.shields.io/badge/licen%C3%A7a-Apache--2.0-blue.svg)](LICENSE)

Painel em tempo real dos agentes de IA, feito para o telão. **Qualquer agente se conecta só trocando URLs, sem alterar código.** Funciona com Ollama, APIs OpenAI-compatible (vLLM, LM Studio, LiteLLM…) e MCP servers (Streamable HTTP).

- **Mapa mental animado:** agentes no centro, MCP servers no meio, tools na borda. Cada chamada vira um pacote que sai do agente, passa pelo server, bate na tool e volta verde (ok) ou laranja (erro), com timer enquanto a tool roda.
- **Bolinha vermelha `!` (alucinação)** e **amarela `?` (incerteza)** no nó em que aconteceu. Uma tool inventada pelo modelo vira um nó *fantasma* tracejado.
- **Clique num nó:** chamadas, p50/p95/máx, % de erro, confiança média, histograma de latência e motivo de cada alerta.
- **Clique numa requisição:** linha do tempo, cada decisão do LLM (raciocínio, confiança, tokens, alternativas descartadas) e cada tool (**por quê**, **payload enviado**, **retorno**, duração), com botão de copiar.
- KPIs no topo e ticker de alertas embaixo. Binário único com a UI embutida e **sem CDN**.

## Começar em 1 minuto

Escolha um jeito de rodar (todos sobem com tráfego sintético para você ver o painel antes de conectar qualquer agente):

```bash
docker run --rm -p 8080:8080 ghcr.io/chwiee/mirante:latest --demo
```

```bash
go install github.com/chwiee/mirante/cmd/mirante@latest && mirante --demo
```

Ou baixe o binário do seu sistema (Linux, macOS, Windows; amd64 e arm64) em [Releases](https://github.com/chwiee/mirante/releases). É um binário único, sem dependências, com a UI embutida.

Abra http://localhost:8080. No telão: `F` liga a tela cheia, `Esc` fecha o painel lateral e `?janela=30` muda a janela das métricas.

## Conectar um agente (plug and play)

Suba o mirante apontando para os destinos reais:

```bash
mirante --llm ollama=http://ollama:11434 --mcp k8s-mcp=http://k8s-mcp:8443 --inject-reasoning
```

Troque as URLs no agente. O nome do agente vai na própria URL:

```diff
- OLLAMA_URL=http://ollama:11434
+ OLLAMA_URL=http://mirante:8080/p/suporte-bot/llm/ollama
- MCP_SERVERS=k8s-mcp=http://k8s-mcp:8443
+ MCP_SERVERS=k8s-mcp=http://mirante:8080/p/suporte-bot/mcp/k8s-mcp
```

O botão **"+ Conectar agente"** no painel gera essas URLs. O mirante repassa tudo de forma transparente (Ollama `/api/chat`, OpenAI-compatible `/chat/completions`, MCP Streamable HTTP) e observa no caminho.

➡️ **Guia completo de implantação** (modos central e sidecar, Kubernetes, checklist de validação, segurança, problemas comuns): [`docs/IMPLANTACAO.md`](docs/IMPLANTACAO.md)

➡️ **Validar uma implantação:** `./scripts/validar.sh http://<mirante>`

➡️ **Só tem o MCP, sem agente próprio** (clientes que você não controla, como Claude Code ou Kiro): **modo front**, em que o mirante escuta no lugar do MCP e **os clientes não mudam nada**. Custo medido: +0,4 ms por chamada. Ver [guia, seção 6.1](docs/IMPLANTACAO.md#61-sem-agente-próprio-modo-front-clientes-não-mudam-nada).

➡️ **MCP que já roda no Kubernetes, com várias réplicas** (sessão MCP por pod): o mirante entra como outro Deployment, aponta para o Service headless e prende cada sessão ao pod certo. Ver [guia, seção 6](docs/IMPLANTACAO.md#6-mirante-na-frente-de-um-mcp-que-já-roda-no-kubernetes) e o diagnóstico `examples/mcp-sessoes`.

➡️ **Pedir para a IA integrar** (Claude Code, Kiro): o mirante serve a instrução pronta em `/ia`. Ver [guia, seção 4.4](docs/IMPLANTACAO.md#44-pedir-para-a-ia-fazer-claude-code-kiro).

➡️ **Agente no mesmo cluster ou LLM assinado (Bedrock):** mande eventos por HTTP direto em `/v1/events`, de qualquer linguagem. Contrato, cliente Go de ~60 linhas e SDK: [`docs/INTEGRACAO-AGENTES.md`](docs/INTEGRACAO-AGENTES.md)

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

mc := mirante.New(mirante.Config{URL: os.Getenv("MIRANTE_URL"), Agent: "ops-bot"}) // URL vazia = no-op
defer mc.Close()

run := mc.StartRun(pergunta, specs) // specs = []mirante.ToolSpec{Server, Name, Schema, ReadOnly}
run.Decision(mirante.Decision{Model: "claude-sonnet-5", Reasoning: thinking, Chosen: []string{"troubleshoot"}, Duration: d})
call := run.ToolCall(mirante.Call{Server: "k8s-mcp", Tool: "troubleshoot", Args: args, Rationale: reason, Confidence: conf})
call.End(resultado, err)
run.Flag("hallucination", "judge", "o juiz discordou da resposta") // opcional
run.End(resposta, nil)
```

Comportamento: assíncrono e em lote, **nunca trava nem derruba o agente** (descarta eventos se o painel cair), e um `*Client` nil é no-op. Para pedir o "porquê" a modelos que não explicam, use `mirante.WithReasoningParams(schema)` nas tools e `mirante.SplitReasoning(args)` antes de executar. É o mesmo mecanismo que o proxy aplica com `--inject-reasoning`.

### Outras linguagens: HTTP direto

`POST /v1/events` aceita um objeto ou um array:

```json
[
  {"type":"run_start","run_id":"r1","agent":"ops-bot","input":"…","tools":[{"server":"k8s-mcp","name":"scan_cluster","schema":{…},"read_only":true}]},
  {"type":"decision","run_id":"r1","span_id":"d1","model":"…","reasoning":"…","chosen":["scan_cluster"],"confidence":0.9,"duration_ms":812},
  {"type":"tool_call","run_id":"r1","span_id":"t1","server":"k8s-mcp","tool":"scan_cluster","args":{…},"rationale":"…"},
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

## Segurança: o painel não tem login

O mirante **não tem autenticação de leitura**, e o painel mostra argumentos e retornos das tools (com chaves sensíveis mascaradas por `--redact-keys`). Rode-o **só em rede interna**, ou atrás de um proxy com SSO (oauth2-proxy, Ingress com auth, Cloudflare Access…). Se o `/v1/events` ficar acessível fora do cluster, use `--ingest-token`. Detalhes em [SECURITY.md](SECURITY.md) e na [seção 11 do guia](docs/IMPLANTACAO.md#11-segurança).

## Limites da v1

Estado em memória com 1 réplica. Heurística não pega afirmação falsa em texto corrido (use LLM-judge + `Flag`). LLM com requisição assinada (Bedrock) não passa pelo proxy: use proxy só de MCP ou o SDK. MCP via stdio não é suportado (só Streamable HTTP). A lista completa está no [guia](docs/IMPLANTACAO.md#13-limitações-conhecidas-v1).

## Contribuir

Issues e pull requests são bem-vindos. Veja [CONTRIBUTING.md](CONTRIBUTING.md) para rodar os testes e o que esperamos de um PR.

## Licença

[Apache-2.0](LICENSE).
