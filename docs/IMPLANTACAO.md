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

**Importante:** `replicas: 1`. O store e o stream do painel ficam em memória, por processo. Para não virar ponto único de falha do agente, use o modo sidecar (seção 7).

### 3.3 Flags e variáveis

| flag | env | padrão | para quê |
|---|---|---|---|
| `--addr` | `MIRANTE_ADDR` | `:8080` | porta HTTP |
| `--llm nome=url` | `MIRANTE_LLM` (`a=url,b=url`) | — | upstreams de LLM do proxy (repetível) |
| `--mcp server=url` | `MIRANTE_MCP` | — | upstreams MCP do proxy (repetível) |
| `--inject-reasoning` | `MIRANTE_INJECT_REASONING=true` | desligado | pede ao modelo o **porquê** e a **confiança** de cada chamada (ver seção 8) |
| `--action-tools` | `MIRANTE_ACTION_TOOLS` | — | tools que executam ações mesmo sem o nome dizer (ver seção 9) |
| `--redact-keys` | `MIRANTE_REDACT_KEYS` | password, secret, token, api_key, authorization… | regex de chaves JSON mascaradas como `***` antes de chegar ao painel |
| `--ingest-token` | `MIRANTE_INGEST_TOKEN` | aberto | bearer exigido em `POST /v1/events` |
| `--events-url` | `MIRANTE_EVENTS_URL` | — | modo sidecar: manda eventos para o central em vez de servir o painel |
| `--public-url` | `MIRANTE_PUBLIC_URL` | deduzida da requisição | URL que os agentes usam para chegar ao mirante; vai nas instruções para IA em `/ia` |
| `--front server=url` | `MIRANTE_FRONT` | — | **modo front**: o mirante escuta no lugar do MCP (mesma URL e caminho), sem nada mudar nos clientes. A URL é só `esquema://host:porta` (seção 6.1) |
| `--front-addr` | `MIRANTE_FRONT_ADDR` | `:8081` | porta do modo front (o Service/Ingress do MCP aponta para ela) |
| — | `MIRANTE_MCP_TOKEN` | — | tokens **só para a descoberta** de MCPs que exigem `Authorization` (`server=token,outro=token`). Por env, não por flag, para não aparecer na lista de processos. Os clientes seguem mandando o próprio token |
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

Para ver também as decisões do Claude (incluindo o *thinking*), instrumente com o SDK (seção 10). Os dois modos convivem no mesmo painel.

### 4.4 Pedir para a IA fazer (Claude Code, Kiro…)

O próprio mirante serve uma instrução escrita **para assistentes de IA**, já preenchida com a URL dele e os upstreams configurados:

| URL | formato |
|---|---|
| `<mirante>/ia` | índice com os comandos abaixo prontos |
| `<mirante>/ia/integrar-mirante.md` | instrução em Markdown puro |
| `<mirante>/ia/claude/SKILL.md` | skill do Claude Code |
| `<mirante>/ia/kiro/mirante.md` | steering do Kiro (inclusão manual) |

**Uso avulso.** Dentro do repositório do agente, peça ao assistente:

> Leia a instrução com `curl -s http://<mirante>/ia/integrar-mirante.md` e integre o mirante neste agente seguindo-a.

**Deixar instalado no repositório.** Com isso, o assistente acha a instrução sozinho da próxima vez:

```bash
# Claude Code: skill (dispara com "integra o mirante")
mkdir -p .claude/skills/integrar-mirante && curl -s http://<mirante>/ia/claude/SKILL.md -o .claude/skills/integrar-mirante/SKILL.md
# Kiro: steering manual (no chat, use #mirante)
mkdir -p .kiro/steering && curl -s http://<mirante>/ia/kiro/mirante.md -o .kiro/steering/mirante.md
```

O botão **"+ Conectar agente"** no painel mostra esses comandos prontos para copiar. A instrução leva o assistente a:

1. descobrir como o agente fala com LLM e MCP;
2. escolher o modo;
3. mudar só a config (ou, se precisar, instrumentar com os clientes de referência Python/TypeScript, testados contra o mirante real);
4. **verificar no próprio mirante** que o run chegou;
5. entregar o diff com o valor antigo e o novo, para facilitar o rollback.

Sem acesso ao mirante rodando, há uma cópia estática em [`docs/ia/`](ia/). Configure `--public-url` (ou `MIRANTE_PUBLIC_URL`) quando a URL que os agentes usam for diferente da URL do telão.

### 4.5 Qualquer outro agente

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
| 7 | pare o mirante e repita a pergunta | no modo central, o agente perde LLM/MCP (por isso existe o sidecar, seção 7). No sidecar, o agente segue normal |

## 6. Mirante na frente de um MCP que já roda no Kubernetes

O cenário: um MCP server (ex: o hub-server do k8s-ts-mcp ou o ce-k8s-mcp) já roda 100% no cluster, e você quer os dados e as métricas dele no painel. Há dois casos:

- **Você não tem agente próprio**, e o MCP é usado por clientes que você não controla (Claude Code, Kiro, outros times): use o **modo front** (6.1). **Os clientes não mudam nada.**
- **Os clientes são seus** e podem trocar a URL: basta o proxy com o nome do agente na URL (6.2 em diante).

### 6.1 Sem agente próprio: modo front (clientes não mudam nada)

No modo front (`--front server=URL`), o mirante **se passa pelo MCP**: escuta na mesma URL e no mesmo caminho que os clientes já usam, repassa tudo sem alterar, e manda os dados para o painel central. O Ingress ou Service que hoje aponta para o MCP passa a apontar para o mirante.

O nome de cada cliente no mapa vem do próprio protocolo MCP: todo cliente se identifica no `initialize` (`clientInfo.name`: `claude-code`, `Kiro`…). O mirante lembra esse nome pela sessão. Quem quiser se identificar melhor manda o header `X-Mirante-Agent: <nome>`. Rotas que não são MCP (ex: o `/healthz` do próprio MCP) passam direto. A saúde do mirante fica em `/_mirante/healthz`.

Duas formas de implantar. Os dois manifests foram validados num cluster kind:

| | **A: Deployment na frente** ([`opcao-a-deployment.yaml`](../deploy/k8s/mcp-front/opcao-a-deployment.yaml)) | **B: sidecar no pod do MCP** ([`opcao-b-sidecar.yaml`](../deploy/k8s/mcp-front/opcao-b-sidecar.yaml)) |
|---|---|---|
| mexe no MCP? | **não**: só o backend do Ingress/Service muda | sim: um container a mais no Deployment e o `targetPort` do Service |
| salto de rede | +1 (cliente → mirante → pod do MCP) | nenhum (localhost) |
| ponto único de falha | sim, 1 réplica (o vínculo sessão→pod fica em memória) | não, escala junto com o MCP |
| corrige `session not found` com várias réplicas | **sim** (afinidade via headless, 6.2) | não: a sessão fica como é hoje |
| rollback | voltar o backend do Ingress | voltar o `targetPort` |

**Quanto o mirante acrescenta na latência.** Medido de dentro de um cluster kind (1 nó), com o cliente MCP oficial do go-sdk e uma sessão longa: 2.000 chamadas por caminho, 3 rodadas, contra uma tool que **responde na hora**. Com isso, todo o custo extra é do mirante:

| caminho | p50 | p99 | extra |
|---|---|---|---|
| direto no MCP | 0,36 ms | 0,84 ms | — |
| A: Deployment front | 0,79 ms | 1,32 ms | **+0,43 ms** p50 / +0,5 ms p99 |
| B: sidecar | 0,77 ms | 1,29 ms | **+0,41 ms** p50 / +0,5 ms p99 |

Como ler esses números:

- O custo é quase todo **o salto HTTP a mais** (mais uma ida e volta pelo kernel, mesmo em localhost), e não a observação em si. O profile de CPU mostra o mirante com cerca de 11% da CPU do processo. É a mesma ordem de grandeza de um sidecar de service mesh (Envoy).
- Para uma tool real que leva 50 ms (uma chamada à API do Kubernetes), +0,4 ms é menos de 1%. Para uma `troubleshoot` de segundos, é ruído.
- Num cluster com vários nós, o modo **A** soma a latência de rede entre nós/AZs, tipicamente 0,1 a 1 ms. O **B** não, porque fala por localhost.
- Nada do painel fica no caminho da requisição. O evento entra numa fila e é aplicado depois. Se o painel atrasar ou cair, eventos são descartados (com aviso no log) e o MCP não espera.
- Sob saturação artificial (32 sessões numa só máquina, contra uma tool que não faz nada), a vazão máxima do conjunto cai pela metade, porque o mirante divide a CPU com o MCP. É um cenário de pior caso, longe de tráfego real. Dê ao container do mirante sua própria reserva de CPU (`requests`).

**Como escolher:** se o MCP tem várias réplicas e sofre de `session not found` (diagnóstico no passo 1 da 6.3), vá de **A**, que resolve isso de brinde. Se o MCP já funciona bem e você não quer nem um salto a mais nem um ponto único, vá de **B**.

**O MCP aparece no mapa já ao subir.** O mirante se conecta ao MCP como cliente, lista as tools e mostra server e tools ("sem chamadas") antes de qualquer cliente passar, repetindo a cada 5 minutos para acompanhar tools novas. Essa sessão de descoberta não vira run no painel.

- **Caminho:** se a URL do `--front` (ou do `--mcp`) trouxer o caminho do MCP (ex: `http://host:9000/mcp`), ele é usado. Se não trouxer, o mirante tenta `/` e depois `/mcp`.
- **Token:** se o MCP exige `Authorization`, informe um token **só para a descoberta**, por variável de ambiente (não por flag, para não aparecer na lista de processos): `MIRANTE_MCP_TOKEN="ce-k8s-mcp=<token>"`. Os clientes continuam mandando o próprio token, que o mirante só repassa.
- **Se não aparecer**, o log do mirante diz por quê e o que fazer, ex: `descoberta do MCP falhou … dica="o MCP exige token: defina MIRANTE_MCP_TOKEN=…"`. O MCP ainda aparece assim que um cliente passar.

#### Testar na sua máquina, com port-forward do EKS

Três terminais. O mirante fica entre o cliente e o MCP:

```bash
kubectl -n ce port-forward svc/ce-k8s-mcp 9000:80
```

```bash
MIRANTE_MCP_TOKEN="ce-k8s-mcp=<token>" go run ./cmd/mirante --front ce-k8s-mcp=http://localhost:9000 --front-addr :8081
```

Abra http://localhost:8080. O `ce-k8s-mcp` e as tools dele aparecem em segundos. Depois, faça um cliente chamar `http://localhost:8081/mcp` em vez de `http://localhost:9000/mcp`. Só a porta muda. Por exemplo, a ferramenta de diagnóstico:

```bash
MCP_BEARER_TOKEN=<token> go run ./examples/mcp-sessoes http://localhost:8081/mcp 3
```

Ou o Claude Code (`claude mcp add --transport http ce-k8s-mcp-mirante http://localhost:8081/mcp --header "Authorization: Bearer <token>"`). O cliente aparece no mapa ligado ao MCP, e cada chamada mostra argumentos, retorno e tempo.

> O port-forward liga a **um pod só**, então o problema de sessão com várias réplicas (6.2) não aparece nesse teste.

### 6.2 Por que precisa de um Service headless

MCP Streamable HTTP feito com o go-sdk (opções padrão) guarda a sessão (`Mcp-Session-Id`) **na memória da réplica que fez o `initialize`**. Com várias réplicas atrás de um Service comum, o kube-proxy espalha as conexões (e o cliente MCP abre mais de uma), então parte das requisições cai na réplica errada e falha com `session not found`.

O Service **headless** (`clusterIP: None`) faz o DNS devolver o IP de **cada pod**. O mirante escolhe um pod no `initialize`, guarda **sessão → pod** e manda o resto daquela sessão para o mesmo pod.

Medido num cluster kind com um MCP stateful de 2 réplicas, usando o cliente oficial do go-sdk:

| caminho | sessões com falha |
|---|---|
| cliente → Service do MCP (como é hoje) | **13%** (100 sessões); **17%** matando um pod no meio (300 sessões) |
| cliente → mirante → Service headless | **0%**; matando um pod, **1** em 300 (a sessão que estava em andamento no pod morto) |

> **Cookie de sticky session no ALB não resolve para clientes Go:** o cliente MCP do go-sdk usa `http.DefaultClient`, que não guarda cookies. Então a afinidade por cookie no Ingress não vale para wb-ce-agent, ce CLI e outros clientes Go. A afinidade no mirante não depende do cliente.

### 6.3 Passo a passo (headless + afinidade)

**1. (Opcional) Diagnostique o MCP atual.** A ferramenta `examples/mcp-sessoes` abre N sessões e conta as falhas. Ela precisa rodar **dentro do cluster** para passar pelo kube-proxy:

```bash
docker build --build-arg CMD=./examples/mcp-sessoes -t <registry>/mcp-sessoes:1 . && docker push <registry>/mcp-sessoes:1
kubectl -n k8s-ts-mcp run diag --rm -it --restart=Never --image=<registry>/mcp-sessoes:1 -- http://hub-server-mcp.k8s-ts-mcp.svc:8443 30
# MCP com token: acrescente  --env=MCP_BEARER_TOKEN=...
```

Saída com `sessoes_falhas` > 0 e `session not found` = o problema existe hoje, com ou sem mirante. Se o erro for de **conexão** (timeout, `connection refused`), não é o problema de sessão: é a NetworkPolicy do MCP bloqueando a porta. É o caso do hub-server, cuja policy só libera a `:7443`. Nesse caso, pule para o passo 2.

**2. Crie o Service headless** (e, se o MCP já tiver NetworkPolicy, a regra que libera o mirante). Exemplos prontos para o hub-server e o ce-k8s-mcp:

```bash
kubectl apply -f deploy/k8s/mirante-na-frente-do-mcp.yaml
```

> **NetworkPolicy, cuidado nos dois sentidos:**
> - O hub-server **já** é isolado: a policy dele só libera a `:7443`, o que bloqueia a `:8443` para todo o resto. Sem a regra `hub-server-mcp-from-mirante`, o mirante não conecta. Validado em kind: sem a regra, bloqueado; com ela, conecta; outro pod sem o label `app: mirante`, continua bloqueado.
> - Num MCP **sem** NetworkPolicy (o ce-k8s-mcp), **não** crie uma "só para liberar o mirante". A existência dela isola os pods e bloqueia todo o resto, inclusive o ALB atual.

**3. Aponte o mirante para o headless** no ConfigMap (`MIRANTE_MCP`), atento ao caminho do MCP (hub-server na raiz; ce-k8s-mcp em `/mcp`):

```
k8s-ts-mcp=http://hub-server-mcp-headless.k8s-ts-mcp.svc.cluster.local:8443
ce-k8s-mcp=http://ce-k8s-mcp-headless.ce.svc.cluster.local:8080/mcp
```

Depois aplique a mudança com `kubectl -n observabilidade rollout restart deploy/mirante`.

**4. Troque a URL nos clientes** para `http://mirante.observabilidade:8080/p/<agente>/mcp/<server>`. Tokens (`Authorization`) passam intactos: o MCP continua autenticando e identificando o cliente como hoje.

**5. Verifique.** Rode o diagnóstico pelo mirante (esperado: `sessoes_falhas=0`):

```bash
kubectl -n k8s-ts-mcp run diag --rm -it --restart=Never --image=<registry>/mcp-sessoes:1 -- http://mirante.observabilidade:8080/p/diagnostico/mcp/k8s-ts-mcp 30
```

O diagnóstico conversa com o mirante, e quem conversa com o MCP é o pod do mirante (que tem o label liberado), então ele pode rodar de qualquer namespace. No painel, **cada sessão MCP vira um run** ("sessão MCP XXXXXXXX"), fechado quando o cliente encerra a sessão. Sessões longas, como de um poller, viram um run novo a cada 200 chamadas.

### 6.4 Como a afinidade se comporta

- **Sessão nova:** vai para um pod aleatório (como o kube-proxy). Daí em diante, todas as requisições dela (POST, stream GET, DELETE) vão para o mesmo pod.
- **Pod morre:** some do DNS headless em segundos (o mirante consulta de novo a cada 3 s). Se o pod recusar conexão antes disso, ele vai para uma **quarentena de 30 s**. Sessões que estavam nele recebem `session not found` e o cliente precisa reabrir a sessão, exatamente como acontece hoje quando um pod do MCP reinicia.
- **O header `Host` original é preservado**, então um upstream atrás de ALB/Ingress que roteia por nome continua funcionando. Um Service comum (1 IP) ou um host externo seguem funcionando como antes.
- **Upstream `https`:** a afinidade fica desligada (o certificado não valeria para o IP do pod). Dentro do cluster, use `http` para o headless.
- **O mapa sessão → pod fica em memória:** se o **mirante** reiniciar, as sessões abertas perdem o vínculo e os clientes precisam reabri-las. Por isso o mirante roda com `replicas: 1`.

> **Recomendação para os clientes:** reconectar a sessão MCP ao receber `session not found`. Isso já é necessário hoje em qualquer reinício de pod do MCP, com ou sem mirante.

**Alternativa sem mirante:** se as tools do MCP não dependem de estado por sessão, ele pode rodar em modo sem sessão (`&mcp.StreamableHTTPOptions{Stateless: true}` no go-sdk), e qualquer réplica atende qualquer requisição. No hub-server, a identidade do agente é resolvida a partir do `Authorization` da sessão (`newSessionServer`). Avalie se essa resolução pode ser feita por requisição antes de mudar.

## 7. Modo sidecar (sem ponto único de falha)

No modo central, se o mirante cair, o agente perde o caminho até o LLM e o MCP. No **sidecar**, um mirante leve roda **no mesmo pod do agente** e só faz proxy. Os eventos vão em lote, de forma assíncrona, para o mirante central. Se o central cair, o sidecar descarta eventos e o agente não percebe.

```
pod do agente: [ agente ] ──localhost──▶ [ mirante --events-url ] ──▶ LLM / MCP reais
                                                    │ (assíncrono, descartável)
                                                    ▼
                                           mirante central ──▶ telão
```

Exemplo em [`deploy/k8s/sidecar-exemplo.yaml`](../deploy/k8s/sidecar-exemplo.yaml). No agente, as URLs passam a apontar para `http://localhost:8081/p/<agente>/...`.

## 8. Injeção de raciocínio (`--inject-reasoning`)

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

## 9. Detecção de alucinação

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

## 10. Modo SDK (Go) e HTTP direto

Use quando quiser algo que o proxy não vê, como o *thinking* do Claude no Bedrock ou um sinal de LLM-judge. O SDK é assíncrono, envia em lote, descarta eventos se o painel cair e é **no-op com URL vazia**. Detalhes e exemplo no [README](../README.md#instrumentar-um-agente-go). Para outras linguagens, o contrato JSON de `POST /v1/events` também está no README.

## 11. Segurança

- **Segredos:** headers (`Authorization`, tokens) são repassados, mas **nunca gravados nem exibidos**. Chaves JSON que casam com `--redact-keys` viram `***` em argumentos e retornos. Mesmo assim, o payload aparece no telão: revise o regex para o seu domínio.
- **Rede:** o mirante não tem autenticação de leitura própria. Exponha o painel só na rede interna, ou atrás do Ingress com SSO. Use `--ingest-token` se `/v1/events` ficar acessível fora do cluster.
- **Upstreams fixos:** o proxy só encaminha para os nomes configurados em `--llm`/`--mcp`. Um nome desconhecido devolve 404, então o mirante não vira um proxy aberto.

## 12. Problemas comuns

| sintoma | causa provável | o que fazer |
|---|---|---|
| `404 upstream de LLM "x" não configurado` | o nome na URL não bate com `--llm` | confira `GET /api/upstreams` |
| `502 mirante proxy: …` | o mirante não alcança o upstream | teste o upstream a partir do pod do mirante. O run aparece com erro `LLM: …` |
| agente aparece, mas sem tools no mapa | o MCP não passa pelo mirante, ou o agente não chama `tools/list` | aponte também a URL do MCP |
| tool aparece em `local` em vez do server | o LLM passa pelo mirante, mas o MCP não | aponte também a URL do MCP |
| run fica "em andamento" | o agente morreu entre uma chamada e outra | fecha sozinho em 10 min como "abandonado" |
| muitos 🟡 `baixa_confianca` | modelo pequeno com `--inject-reasoning` | ajuste `--min-confidence` (ex: `0.4`) ou `0` para desligar |
| nada aparece no telão | navegador sem acesso ao `/api/stream` (proxy/ingress bufferizando SSE) | o mirante manda `X-Accel-Buffering: no`. Confira o timeout de leitura do Ingress (≥ 60 s) |

## 13. Limitações conhecidas (v1)

- Estado em memória, 1 réplica. Reiniciou, o histórico do painel zera (os agentes não são afetados).
- A detecção é heurística. Ela pega identificador inventado, tool/argumento inventado e ação afirmada sem tool, mas **não** pega afirmação falsa em texto corrido ("o cluster está saudável"). Para isso, use um LLM-judge e mande o sinal via SDK (`run.Flag`).
- Bedrock e outros LLMs com assinatura de requisição não passam pelo proxy (use proxy só MCP ou o SDK).
- `--inject-reasoning` só age em requisições sem streaming.
