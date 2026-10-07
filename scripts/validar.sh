#!/usr/bin/env bash
# Valida uma implantação do mirante de ponta a ponta, sem precisar de agente.
#
#   ./scripts/validar.sh [URL do mirante]        (padrão: http://localhost:8080)
#   MIRANTE_INGEST_TOKEN=... ./scripts/validar.sh https://mirante.interno
#
# Depende só de bash, curl e grep.
set -u
BASE="${1:-http://localhost:8080}"
BASE="${BASE%/}"
AUTH=()
[ -n "${MIRANTE_INGEST_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${MIRANTE_INGEST_TOKEN}")
FAIL=0
ok()   { printf '  \033[32m✅ %s\033[0m\n' "$1"; }
bad()  { printf '  \033[31m❌ %s\033[0m\n' "$1"; FAIL=$((FAIL+1)); }
info() { printf '     %s\n' "$1"; }

echo "mirante em $BASE"

echo "1. saúde"
if [ "$(curl -s -m 5 "$BASE/healthz")" = "ok" ]; then ok "/healthz respondeu ok"; else bad "/healthz não respondeu (mirante no ar? URL certa?)"; echo; exit 1; fi

echo "2. upstreams do proxy (modo plug-and-play)"
UP=$(curl -s -m 5 "$BASE/api/upstreams")
info "$UP"
LLM=$(echo "$UP" | grep -o '"llm":\[[^]]*\]' | grep -o '"[^"]*"' | sed -n '2p' | tr -d '"')
MCP=$(echo "$UP" | grep -o '"mcp":\[[^]]*\]' | grep -o '"[^"]*"' | sed -n '2p' | tr -d '"')
if [ -z "$LLM$MCP" ]; then
  info "nenhum upstream configurado: só o modo SDK/HTTP está disponível (use --llm/--mcp para o proxy)"
fi

if [ -n "$LLM" ]; then
  code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$BASE/p/validacao/llm/$LLM/api/tags")
  [ "$code" = "404" ] && code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$BASE/p/validacao/llm/$LLM/v1/models")
  if [ "$code" = "200" ]; then ok "proxy alcança o LLM \"$LLM\" (HTTP $code)"; elif [ "$code" = "401" ] || [ "$code" = "403" ]; then ok "proxy alcança o LLM \"$LLM\" (HTTP $code: exige credencial, que o agente envia normalmente)"; else bad "proxy NÃO alcança o LLM \"$LLM\" (HTTP $code) — confira a URL em --llm e a rede do mirante até ele"; fi
fi
if [ -n "$MCP" ]; then
  INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"validar.sh","version":"0"}}}'
  code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "$BASE/p/validacao/mcp/$MCP" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d "$INIT")
  if [ "$code" = "200" ]; then ok "proxy alcança o MCP \"$MCP\" (initialize HTTP $code)"; elif [ "$code" = "401" ] || [ "$code" = "403" ]; then ok "proxy alcança o MCP \"$MCP\" (HTTP $code: exige token, que o agente envia normalmente)"; else bad "proxy NÃO alcança o MCP \"$MCP\" (HTTP $code) — confira a URL em --mcp (inclui o caminho, ex: /mcp?)"; fi
fi

echo "3. ingestão + detecção (run sintético com uma alucinação plantada)"
RUN="validacao-$(date +%s)"
EVENTS='[
 {"type":"run_start","run_id":"'$RUN'","agent":"validar.sh","input":"reinicia o pod api-1 no eks-a",
  "tools":[{"server":"k8s-ts-mcp","name":"scan_cluster","schema":{"type":"object","required":["cluster_id"],"properties":{"cluster_id":{"type":"string"}}}}]},
 {"type":"decision","run_id":"'$RUN'","span_id":"d1","model":"validacao","chosen":["restart_cluster"],"confidence":0.9,"duration_ms":10},
 {"type":"tool_call","run_id":"'$RUN'","span_id":"t1","server":"k8s-ts-mcp","tool":"restart_cluster","args":{"cluster_id":"eks-a"}},
 {"type":"tool_result","run_id":"'$RUN'","span_id":"t1","error":"tool não existe","duration_ms":5},
 {"type":"run_end","run_id":"'$RUN'","output":"Reiniciei o pod api-1."}
]'
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/events" -H 'Content-Type: application/json' "${AUTH[@]}" -d "$EVENTS")
case "$code" in
  202) ok "POST /v1/events aceito" ;;
  401) bad "POST /v1/events recusou o token (exporte MIRANTE_INGEST_TOKEN)";;
  *)   bad "POST /v1/events HTTP $code";;
esac
RUNJSON=$(curl -s -m 5 "$BASE/api/runs/$RUN")
echo "$RUNJSON" | grep -q '"status":"ok"' && ok "run montado e finalizado" || bad "run não encontrado/finalizado"
echo "$RUNJSON" | grep -q '"code":"tool_inexistente"' && ok "detectou tool inventada (restart_cluster)" || bad "não detectou tool_inexistente"
echo "$RUNJSON" | grep -q '"code":"acao_nao_executada"' && ok "detectou ação afirmada sem tool (\"Reiniciei\")" || bad "não detectou acao_nao_executada"

echo "4. stream do painel (SSE)"
if curl -s -N -m 3 "$BASE/api/stream" 2>/dev/null | grep -q '^event: snapshot'; then
  ok "/api/stream entrega snapshot (se o telão travar, o problema está entre o navegador e o mirante: Ingress/proxy bufferizando)"
else
  bad "/api/stream não entregou snapshot"
fi

echo
if [ "$FAIL" -eq 0 ]; then
  printf '\033[32mtudo certo.\033[0m Abra %s e procure o agente "validar.sh" no mapa.\n' "$BASE"
  echo "próximo passo: troque as URLs de um agente real (botão \"+ Conectar agente\" no painel gera as URLs)."
else
  printf '\033[31m%d verificação(ões) falharam.\033[0m Ver docs/IMPLANTACAO.md, seção "Problemas comuns".\n' "$FAIL"
  exit 1
fi
