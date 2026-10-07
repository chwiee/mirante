# Implantação do mirante

Guia para colocar o painel de agentes no ar e conectar **qualquer agente** a ele. O caminho principal não exige alterar código do agente: só trocar URLs de configuração.

> Tempo esperado: **~10 min** para subir o mirante e ver o primeiro agente no painel.
> Para desfazer, volte as URLs ao valor original. Nada fica instalado no agente.

---

## 1. Como funciona, em uma figura

```
                         ┌───────────────────────── mirante ─────────────────────────┐
  agente ── LLM_URL ───▶ │ /p/{agente}/llm/{nome} ──▶ encaminha ──▶ Ollama / vLLM / …  │
         ── MCP_URL ───▶ │ /p/{agente}/mcp/{server} ─▶ encaminha ──▶ MCP server real   │
                         │            │ observa no caminho                            │
                         │            ▼                                               │
                         │   correlaciona ▸ detecta alucinação ▸ SSE ──────────────────┼──▶ telão
                         └────────────────────────────────────────────────────────────┘
```

O mirante funciona como **proxy transparente**. Ele repassa cada requisição ao destino original e devolve a resposta original. Pelo caminho, ele registra:

| visto no proxy de **LLM** | visto no proxy de **MCP** |
|---|---|
| pergunta do usuário | lista de tools de cada server (`tools/list`) |
| tools oferecidas ao modelo | cada `tools/call`: argumentos, retorno, erro |
| cada decisão: tool escolhida, texto/raciocínio, tokens, latência | duração **exata** de cada tool |
| resposta final | anotações MCP (`readOnlyHint`/`destructiveHint`) |

Com os dois proxies juntos, o mirante liga tudo no mesmo run. Com só o MCP, cada rajada de chamadas vira um run, mas sem a visão do LLM. Isso serve para agentes cujo LLM não dá para interceptar (Bedrock, Claude Code etc.).

## 2. Qual modo usar

| situação | modo | o que muda no agente |
|---|---|---|
| LLM via **Ollama** (`/api/chat`) ou **OpenAI-compatible** (`/chat/completions`) **e** tools via MCP Streamable HTTP | **Proxy LLM + MCP** (recomendado) | 2 URLs |
| LLM que assina requisição (**AWS Bedrock**/SigV4), mas tools via MCP | **Proxy só MCP** | 1 URL |
| Agente em Go e você quer o raciocínio completo (ex: thinking do Claude) | **SDK** (`pkg/mirante`) | ~10 linhas |
| Outra linguagem, sem MCP nem LLM HTTP | **HTTP direto** (`POST /v1/events`) | emitir JSON |

> **Por que o Bedrock não passa pelo proxy:** a requisição é assinada com SigV4 para o host da AWS, e um proxy no meio invalida a assinatura. Use o proxy só de MCP ou o SDK.

## 3. Subir o mirante (central)

### 3.1 Local (para validar)

```bash
go build -o mirante ./cmd/mirante
./mirante --addr :8080 \
  --llm ollama=http://localhost:11434 \
  --mcp k8s-ts-mcp=http://localhost:8443 \
  --mcp knowledge-mcp=http://localhost:8444 \
  --inject-reasoning \
  --action-tools troubleshoot,approve_action
```

Abra http://localhost:8080 e clique em **"+ Conectar agente"**. A tela gera as URLs exatas para o agente, a partir do que está configurado.

### 3.2 Kubernetes

Manifesto pronto em [`deploy/k8s/mirante.yaml`](../deploy/k8s/mirante.yaml) (Deployment + Service + ConfigMap):

```bash
kubectl apply -f deploy/k8s/mirante.yaml
kubectl -n observabilidade port-forward svc/mirante 8080:8080   # ou exponha via Ingress para o telão
```

Os upstreams ficam no ConfigMap, em `MIRANTE_LLM` e `MIRANTE_MCP`. Para adicionar um MCP server novo, edite o ConfigMap e reinicie o pod. Os agentes não mudam.

**Importante:** `replicas: 1`. O store e o stream do painel ficam em memória, por processo. Para não virar ponto único de falha do agente, use o modo sidecar (seção 6).

### 3.3 Flags e variáveis

| flag | env | padrão | para quê |
|---|---|---|---|
| `--addr` | `MIRANTE_ADDR` | `:8080` | porta HTTP |
| `--llm nome=url` | `MIRANTE_LLM` (`a=url,b=url`) | — | upstreams de LLM do proxy (repetível) |
| `--mcp server=url` | `MIRANTE_MCP` | — | upstreams MCP do proxy (repetível) |
| `--inject-reasoning` | `MIRANTE_INJECT_REASONING=true` | desligado | pede ao modelo o **porquê** e a **confiança** de cada chamada (ver seção 7) |
| `--action-tools` | `MIRANTE_ACTION_TOOLS` | — | tools que executam ações mesmo sem o nome dizer (ver seção 8) |
| `--redact-keys` | `MIRANTE_REDACT_KEYS` | password, secret, token, api_key, authorization… | regex de chaves JSON mascaradas como `***` antes de chegar ao painel |
| `--ingest-token` | `MIRANTE_INGEST_TOKEN` | aberto | bearer exigido em `POST /v1/events` |
| `--events-url` | `MIRANTE_EVENTS_URL` | — | modo sidecar: manda eventos para o central em vez de servir o painel |
| `--min-confidence` | — | `0.6` | abaixo disso, a chamada fica marcada como incerteza |
| `--max-runs` | — | `500` | quantos runs ficam em memória |

## 4. Conectar um agente (modo proxy)

O padrão das URLs é este, com o nome do agente **na própria URL**. É assim que ele aparece no mapa:

```
LLM:  http://<mirante>/p/<nome-do-agente>/llm/<nome-do-upstream>
MCP:  http://<mirante>/p/<nome-do-agente>/mcp/<nome-do-server>
```

Tudo o que vem depois é repassado igual. O caminho (`/api/chat`, `/v1/chat/completions`, `/api/tags`…), a query string e os headers (inclusive `Authorization` e `Mcp-Session-Id`) chegam intactos ao upstream. A única exceção é o `Accept-Encoding`, removido nas requisições observadas para a resposta vir sem compressão e poder ser lida.

### 4.1 wb-ce-agent (Ollama + MCP)

```diff
- OLLAMA_URL=http://ollama:11434
+ OLLAMA_URL=http://mirante:8080/p/wb-ce-agent/llm/ollama
- MCP_SERVERS=k8s-ts-mcp=http://k8s-ts-mcp:8443
+ MCP_SERVERS=k8s-ts-mcp=http://mirante:8080/p/wb-ce-agent/mcp/k8s-ts-mcp
```

### 4.2 ce aut (OpenAI-compatible)

```diff
- CE_LLM_URL=http://ollama.ce.svc:11434/v1
+ CE_LLM_URL=http://mirante:8080/p/ce-aut/llm/ollama/v1
- CE_K8S_MCP_URL=http://ce-k8s-mcp:8080/mcp
+ CE_K8S_MCP_URL=http://mirante:8080/p/ce-aut/mcp/ce-k8s-mcp
```

O `/v1` continua no fim da URL do LLM: o mirante só troca o host. O `CE_LLM_API_KEY` e o `CE_K8S_MCP_TOKEN` seguem no header até o destino. O mirante não os guarda nem os exibe.

### 4.3 Alfred (Bedrock + MCP)

Só a URL dos MCP servers:

```diff
- k8s-ts-mcp: http://k8s-ts-mcp:8443
+ k8s-ts-mcp: http://mirante:8080/p/alfred/mcp/k8s-ts-mcp
```

Para ver também as decisões do Claude (incluindo o *thinking*), instrumente com o SDK (seção 9). Os dois modos convivem no mesmo painel.

### 4.4 Qualquer outro agente

Se ele fala Ollama ou OpenAI-compatible e/ou MCP Streamable HTTP, o procedimento é o mesmo: troque as URLs. Exemplo mínimo e completo, que **não importa nada do mirante**: [`examples/agente-minimo`](../examples/agente-minimo/main.go).

## 5. Validar a implantação (checklist)

Script automático, que não depende de agente real:

```bash
./scripts/validar.sh http://localhost:8080
```

Ele confere a saúde do serviço, os upstreams configurados, a ingestão, a detecção de alucinação e o stream do painel, e imprime ✅/❌ em cada passo.

Validação manual com um agente real:

| # | faça | esperado |
|---|---|---|
| 1 | `curl http://<mirante>/healthz` | `ok` |
| 2 | `curl http://<mirante>/api/upstreams` | seus `llm` e `mcp` listados |
| 3 | troque as URLs do agente e reinicie-o | o agente sobe normal. Se usa MCP, o painel mostra o agente ligado ao server e às tools logo na conexão (`tools/list`) |
| 4 | faça uma pergunta que use tool | no telão, o pacote anda **agente → server → tool** e volta verde. No feed, aparece um card com a pergunta |
| 5 | clique no card | linha do tempo com decisões e tools, **payload enviado** e **retorno** |
| 6 | compare a resposta do agente com e sem o mirante | **idêntica**: o proxy é transparente |
| 7 | pare o mirante e repita a pergunta | no modo central, o agente perde LLM/MCP (por isso existe o sidecar, seção 6). No sidecar, o agente segue normal |

## 6. Modo sidecar (sem ponto único de falha)

No modo central, se o mirante cair, o agente perde o caminho até o LLM e o MCP. No **sidecar**, um mirante leve roda **no mesmo pod do agente** e só faz proxy. Os eventos vão em lote, de forma assíncrona, para o mirante central. Se o central cair, o sidecar descarta eventos e o agente não percebe.

```
pod do agente: [ agente ] ──localhost──▶ [ mirante --events-url ] ──▶ LLM / MCP reais
                                                    │ (assíncrono, descartável)
                                                    ▼
                                           mirante central ──▶ telão
```

Exemplo em [`deploy/k8s/sidecar-exemplo.yaml`](../deploy/k8s/sidecar-exemplo.yaml). No agente, as URLs passam a apontar para `http://localhost:8081/p/<agente>/...`.

## 7. Injeção de raciocínio (`--inject-reasoning`)

Modelos menores (ex: `qwen2.5:7b`) chamam tool sem explicar o motivo. Com esta opção, o mirante:

1. acrescenta `reason` (string) e `confidence` (0–1) como **argumentos obrigatórios** no schema de cada tool enviada ao modelo;
2. **remove** esses campos da resposta antes de devolvê-la ao agente;
3. registra os valores no painel ("por quê" e "confiança" de cada chamada).

Garantias, validadas com `qwen2.5:7b` real:

- O agente recebe exatamente os argumentos que receberia sem o mirante.
- A remoção tolera as variações que o modelo produz: `confiança` com cedilha (visto ao vivo), maiúsculas, `motivo`, `80`, `"80%"`, `"0,8"`.
- **Rede de segurança no proxy MCP:** se algum desses campos escapar e chegar num `tools/call`, ele é removido antes de chegar ao MCP server, a menos que a tool o declare de verdade. Sem isso, um server com `additionalProperties: false` rejeitaria a chamada.
- Só vale para requisições **sem streaming**. Com `stream: true`, o proxy só observa.

Limite: a confiança é **auto-reportada** pelo modelo e mal calibrada. O painel trata isso como um sinal entre vários, nunca como prova.

## 8. Detecção de alucinação

Roda no mirante, sobre o que ele observa. Funciona igual para qualquer modelo.

| código | cor | quando |
|---|---|---|
| `tool_inexistente` | 🔴 | o modelo chamou uma tool que não foi oferecida a ele. Vira um **nó fantasma** tracejado no mapa |
| `args_invalidos` | 🔴 | argumento obrigatório faltando, argumento inventado, tipo errado ou fora do `enum` |
| `resposta_sem_base` | 🔴 | a resposta cita identificadores (pod, IP, versão, `512Mi`, nome entre crases) que não aparecem na pergunta nem nos retornos das tools |
| `acao_nao_executada` | 🔴 | a resposta ou o raciocínio afirma uma ação ("registrei", "reiniciei", "aumentei o limit") e nenhuma tool capaz disso rodou com sucesso no run. **Visto ao vivo** com qwen2.5:7b |
| `baixa_confianca` | 🟡 | confiança abaixo de `--min-confidence` |
| `escolha_ambigua` | 🟡 | a tool descartada ficou quase empatada com a escolhida |
| `chamada_repetida` | 🟡 | mesma tool com os mesmos argumentos de novo (loop) |
| `resposta_apos_erro` | 🟡 | todas as tools falharam, mas a resposta não menciona erro |

**Para `acao_nao_executada` não gerar falso positivo**, o mirante precisa saber quais tools executam ações. Ele usa três fontes, nesta ordem:

1. `--action-tools troubleshoot,approve_action` (para tools cujo nome não diz, como o `troubleshoot` do k8s-ts-mcp, que deleta pod);
2. anotações MCP no `tools/list`: `readOnlyHint: false` ou `destructiveHint: true`. **Recomendado: anote suas tools no MCP server**;
3. palavras no nome da tool (`scale_deployment`, `record_lesson`, `delete_pod`…), comparadas por token: `get_settings` não conta como "set".

## 9. Modo SDK (Go) e HTTP direto

Use quando quiser algo que o proxy não vê, como o *thinking* do Claude no Bedrock ou um sinal de LLM-judge. O SDK é assíncrono, envia em lote, descarta eventos se o painel cair e é **no-op com URL vazia**. Detalhes e exemplo no [README](../README.md#instrumentar-um-agente-go). Para outras linguagens, o contrato JSON de `POST /v1/events` também está no README.

## 10. Segurança

- **Segredos:** headers (`Authorization`, tokens) são repassados, mas **nunca gravados nem exibidos**. Chaves JSON que casam com `--redact-keys` viram `***` em argumentos e retornos. Mesmo assim, o payload aparece no telão: revise o regex para o seu domínio.
- **Rede:** o mirante não tem autenticação de leitura própria. Exponha o painel só na rede interna, ou atrás do Ingress com SSO. Use `--ingest-token` se `/v1/events` ficar acessível fora do cluster.
- **Upstreams fixos:** o proxy só encaminha para os nomes configurados em `--llm`/`--mcp`. Um nome desconhecido devolve 404, então o mirante não vira um proxy aberto.

## 11. Problemas comuns

| sintoma | causa provável | o que fazer |
|---|---|---|
| `404 upstream de LLM "x" não configurado` | o nome na URL não bate com `--llm` | confira `GET /api/upstreams` |
| `502 mirante proxy: …` | o mirante não alcança o upstream | teste o upstream a partir do pod do mirante. O run aparece com erro `LLM: …` |
| agente aparece, mas sem tools no mapa | o MCP não passa pelo mirante, ou o agente não chama `tools/list` | aponte também a URL do MCP |
| tool aparece em `local` em vez do server | o LLM passa pelo mirante, mas o MCP não | aponte também a URL do MCP |
| run fica "em andamento" | o agente morreu entre uma chamada e outra | fecha sozinho em 10 min como "abandonado" |
| muitos 🟡 `baixa_confianca` | modelo pequeno com `--inject-reasoning` | ajuste `--min-confidence` (ex: `0.4`) ou `0` para desligar |
| nada aparece no telão | navegador sem acesso ao `/api/stream` (proxy/ingress bufferizando SSE) | o mirante manda `X-Accel-Buffering: no`. Confira o timeout de leitura do Ingress (≥ 60 s) |

## 12. Limitações conhecidas (v1)

- Estado em memória, 1 réplica. Reiniciou, o histórico do painel zera (os agentes não são afetados).
- A detecção é heurística. Ela pega identificador inventado, tool/argumento inventado e ação afirmada sem tool, mas **não** pega afirmação falsa em texto corrido ("o cluster está saudável"). Para isso, use um LLM-judge e mande o sinal via SDK (`run.Flag`).
- Bedrock e outros LLMs com assinatura de requisição não passam pelo proxy (use proxy só MCP ou o SDK).
- `--inject-reasoning` só age em requisições sem streaming.
