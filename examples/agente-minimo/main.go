// agente-minimo é um agente de tool-calling comum (Ollama + MCP), no mesmo
// formato do wb-ce-agent — e que NÃO importa nada do mirante.
//
// Ele existe para provar o modo plug-and-play: para aparecer no painel basta
// apontar as URLs para o mirante. Compare:
//
//	# direto (sem painel)
//	LLM_URL=http://localhost:11434 MCP_URL=http://localhost:8444 go run ./examples/agente-minimo "pergunta"
//
//	# pelo mirante (com painel) — mesmo binário, só as URLs mudam
//	LLM_URL=http://localhost:8080/p/agente-minimo/llm/ollama \
//	MCP_URL=http://localhost:8080/p/agente-minimo/mcp/knowledge-mcp \
//	go run ./examples/agente-minimo "pergunta"
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Name      string     `json:"name,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

func main() {
	llmURL := strings.TrimRight(env("LLM_URL", "http://localhost:11434"), "/")
	mcpURL := env("MCP_URL", "http://localhost:8444")
	model := env("MODEL", "qwen2.5:7b")
	question := strings.Join(os.Args[1:], " ")
	if question == "" {
		question = "Já registramos alguma lição sobre OOMKilled? Procure na base."
	}
	ctx := context.Background()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agente-minimo", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpURL}, nil)
	if err != nil {
		log.Fatalf("conectando no MCP %s: %v", mcpURL, err)
	}
	defer cs.Close()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	var tools []any
	for _, t := range lt.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
	}

	msgs := []message{
		{Role: "system", Content: "Você é um agente de troubleshooting. Use as ferramentas quando precisar de dados; responda em português, curto."},
		{Role: "user", Content: question},
	}
	for round := 0; round < 4; round++ {
		reply := chat(llmURL, model, msgs, tools)
		if len(reply.ToolCalls) == 0 {
			fmt.Println(reply.Content)
			return
		}
		msgs = append(msgs, reply)
		for _, c := range reply.ToolCalls {
			fmt.Fprintf(os.Stderr, "→ %s %v\n", c.Function.Name, c.Function.Arguments)
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.Function.Name, Arguments: c.Function.Arguments})
			out := ""
			switch {
			case err != nil:
				out = "erro: " + err.Error()
			default:
				for _, ct := range res.Content {
					if tc, ok := ct.(*mcp.TextContent); ok {
						out += tc.Text
					}
				}
			}
			msgs = append(msgs, message{Role: "tool", Name: c.Function.Name, Content: out})
		}
	}
	log.Fatal("o modelo continuou pedindo ferramentas")
}

func chat(base, model string, msgs []message, tools []any) message {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": false, "messages": msgs, "tools": tools})
	resp, err := http.Post(base+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("chamando LLM: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Message message `json:"message"`
		Error   string  `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		log.Fatalf("LLM HTTP %d: %s", resp.StatusCode, out.Error)
	}
	return out.Message
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
