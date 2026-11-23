# Segurança

## Reportar uma vulnerabilidade

**Não abra issue pública.** Use o reporte privado do GitHub: aba **Security → Report a vulnerability** neste repositório. Respondemos assim que possível e combinamos a divulgação depois da correção.

## Modelo de segurança (leia antes de expor o mirante)

O mirante é uma ferramenta de **rede interna**. Pontos importantes:

- **O painel não tem autenticação.** Qualquer um que alcance a porta HTTP vê os runs, incluindo **argumentos e retornos das tools**. Exponha só na rede interna ou atrás de um proxy com SSO (oauth2-proxy, Ingress com autenticação, Cloudflare Access…). Ao subir fora de `127.0.0.1`, o mirante avisa isso no log.
- **Ingestão:** `POST /v1/events` fica aberto por padrão. Use `--ingest-token` (`MIRANTE_INGEST_TOKEN`) se ele for alcançável fora do cluster.
- **Segredos em trânsito:** headers como `Authorization` são repassados ao upstream, mas **nunca gravados nem exibidos**. Chaves JSON que casam com `--redact-keys` viram `***` em argumentos e retornos antes de chegar ao painel. Revise o regex para o seu domínio: valores sensíveis em chaves com outros nomes **aparecem** no telão.
- **Não é proxy aberto:** o proxy só encaminha para os upstreams configurados em `--llm`/`--mcp`/`--front`; nome desconhecido devolve 404.
- **Estado em memória:** nada é gravado em disco. Reiniciar o processo apaga o histórico.
- **Tokens de descoberta** (`MIRANTE_MCP_TOKEN`) vão por variável de ambiente, não por flag, para não aparecerem na lista de processos.
