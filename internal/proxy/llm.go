package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/pkg/mirante"
)

// Dialetos de chat suportados. Detectados pelo caminho da requisição.
const (
	dialectOllama = "ollama" // POST /api/chat
	dialectOpenAI = "openai" // POST /v1/chat/completions (vLLM, LiteLLM, Ollama /v1, OpenAI…)
)

func detectDialect(path string) string {
	p := strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(p, "api/chat"):
		return dialectOllama
	case strings.HasSuffix(p, "chat/completions"):
		return dialectOpenAI
	}
	return ""
}

// parseChatReq extrai o que importa do corpo, nos dois dialetos (o formato
// de messages/tools é o mesmo o suficiente).
func parseChatReq(dialect string, body []byte) (chatReq, map[string]any, bool) {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return chatReq{}, nil, false
	}
	var req chatReq
	req.Model, _ = raw["model"].(string)
	req.Stream, _ = raw["stream"].(bool)
	if _, set := raw["stream"]; !set && dialect == dialectOllama {
		req.Stream = true // no Ollama, stream omitido = stream ligado
	}
	msgs, _ := raw["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		cm := chatMsg{Content: contentText(mm["content"])}
		cm.Role, _ = mm["role"].(string)
		cm.ToolCallID, _ = mm["tool_call_id"].(string)
		cm.Name, _ = mm["name"].(string)
		if cm.Name == "" {
			cm.Name, _ = mm["tool_name"].(string) // Ollama recente
		}
		req.Messages = append(req.Messages, cm)
	}
	tools, _ := raw["tools"].([]any)
	for _, t := range tools {
		tm, _ := t.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		spec := event.ToolSpec{}
		spec.Name, _ = fn["name"].(string)
		spec.Description, _ = fn["description"].(string)
		spec.Schema, _ = fn["parameters"].(map[string]any)
		req.Tools = append(req.Tools, spec)
	}
	return req, raw, true
}

// contentText aceita content string ou array de partes [{type:text,text}].
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// injectReasoning acrescenta reason/confidence obrigatórios ao schema de toda
// tool oferecida. Devolve o corpo novo.
func injectReasoning(raw map[string]any) []byte {
	tools, _ := raw["tools"].([]any)
	for _, t := range tools {
		tm, _ := t.(map[string]any)
		if fn, ok := tm["function"].(map[string]any); ok {
			params, _ := fn["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{"type": "object"}
			}
			fn["parameters"] = mirante.WithReasoningParams(params)
		}
	}
	b, _ := json.Marshal(raw)
	return b
}

// ---------- respostas ----------

// parseChatResp entende os 4 formatos: Ollama/OpenAI × com/sem stream.
func parseChatResp(dialect string, body []byte, stream bool) (chatResp, bool) {
	if dialect == dialectOllama {
		if stream || bytes.Count(bytes.TrimSpace(body), []byte("\n")) > 0 {
			return parseOllamaStream(body)
		}
		return parseOllama(body)
	}
	if stream || bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:")) {
		return parseOpenAIStream(body)
	}
	return parseOpenAI(body)
}

type ollamaMsg struct {
	Content   string `json:"content"`
	Thinking  string `json:"thinking"`
	ToolCalls []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type ollamaResp struct {
	Model           string    `json:"model"`
	Message         ollamaMsg `json:"message"`
	Done            bool      `json:"done"`
	PromptEvalCount int       `json:"prompt_eval_count"`
	EvalCount       int       `json:"eval_count"`
}

func parseOllama(body []byte) (chatResp, bool) {
	var r ollamaResp
	if json.Unmarshal(body, &r) != nil {
		return chatResp{}, false
	}
	out := chatResp{Model: r.Model, Content: r.Message.Content, Reasoning: r.Message.Thinking, TokensIn: r.PromptEvalCount, TokensOut: r.EvalCount}
	for _, tc := range r.Message.ToolCalls {
		out.Calls = append(out.Calls, splitCall("", tc.Function.Name, tc.Function.Arguments))
	}
	return out, true
}

func parseOllamaStream(body []byte) (chatResp, bool) {
	var out chatResp
	ok := false
	var content, thinking strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	for sc.Scan() {
		var r ollamaResp
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		ok = true
		out.Model = r.Model
		content.WriteString(r.Message.Content)
		thinking.WriteString(r.Message.Thinking)
		for _, tc := range r.Message.ToolCalls {
			out.Calls = append(out.Calls, splitCall("", tc.Function.Name, tc.Function.Arguments))
		}
		if r.Done {
			out.TokensIn, out.TokensOut = r.PromptEvalCount, r.EvalCount
		}
	}
	out.Content, out.Reasoning = content.String(), thinking.String()
	return out, ok
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIResp struct {
	Model   string `json:"model"`
	Choices []struct {
		Message *struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta *struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func parseOpenAI(body []byte) (chatResp, bool) {
	var r openAIResp
	if json.Unmarshal(body, &r) != nil || len(r.Choices) == 0 || r.Choices[0].Message == nil {
		return chatResp{}, false
	}
	m := r.Choices[0].Message
	out := chatResp{Model: r.Model, Content: m.Content, Reasoning: m.ReasoningContent}
	if r.Usage != nil {
		out.TokensIn, out.TokensOut = r.Usage.PromptTokens, r.Usage.CompletionTokens
	}
	for _, tc := range m.ToolCalls {
		out.Calls = append(out.Calls, splitCall(tc.ID, tc.Function.Name, json.RawMessage(argsOrEmpty(tc.Function.Arguments))))
	}
	return out, true
}

func parseOpenAIStream(body []byte) (chatResp, bool) {
	var out chatResp
	ok := false
	var content, reasoning strings.Builder
	type acc struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*acc{}
	var order []int
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var r openAIResp
		if json.Unmarshal([]byte(data), &r) != nil {
			continue
		}
		ok = true
		if r.Model != "" {
			out.Model = r.Model
		}
		if r.Usage != nil {
			out.TokensIn, out.TokensOut = r.Usage.PromptTokens, r.Usage.CompletionTokens
		}
		if len(r.Choices) == 0 || r.Choices[0].Delta == nil {
			continue
		}
		d := r.Choices[0].Delta
		content.WriteString(d.Content)
		reasoning.WriteString(d.ReasoningContent)
		for _, tc := range d.ToolCalls {
			a := calls[tc.Index]
			if a == nil {
				a = &acc{}
				calls[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
	}
	out.Content, out.Reasoning = content.String(), reasoning.String()
	for _, i := range order {
		a := calls[i]
		out.Calls = append(out.Calls, splitCall(a.id, a.name, json.RawMessage(argsOrEmpty(a.args.String()))))
	}
	return out, ok
}

func argsOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// splitCall tira reason/confidence dos args (quando presentes).
func splitCall(id, name string, args json.RawMessage) toolCall {
	tc := toolCall{ID: id, Name: name, Args: args}
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return tc
	}
	clean, motivo, conf := mirante.SplitReasoning(m)
	if motivo == "" && conf == nil {
		return tc
	}
	tc.Args, _ = json.Marshal(clean)
	tc.Motivo, tc.Confianca = motivo, conf
	return tc
}

// stripReasoning reescreve uma resposta NÃO-stream tirando reason/confidence
// dos argumentos, pra que o agente receba exatamente o que esperava.
func stripReasoning(dialect string, body []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	clean := func(args map[string]any) map[string]any {
		c, _, _ := mirante.SplitReasoning(args)
		return c
	}
	var calls []any
	if dialect == dialectOllama {
		msg, _ := raw["message"].(map[string]any)
		calls, _ = msg["tool_calls"].([]any)
	} else if ch, _ := raw["choices"].([]any); len(ch) > 0 {
		c0, _ := ch[0].(map[string]any)
		msg, _ := c0["message"].(map[string]any)
		calls, _ = msg["tool_calls"].([]any)
	}
	if len(calls) == 0 {
		return body
	}
	for _, c := range calls {
		cm, _ := c.(map[string]any)
		fn, _ := cm["function"].(map[string]any)
		switch a := fn["arguments"].(type) {
		case map[string]any:
			fn["arguments"] = clean(a)
		case string:
			var m map[string]any
			if json.Unmarshal([]byte(a), &m) == nil {
				b, _ := json.Marshal(clean(m))
				fn["arguments"] = string(b)
			}
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return b
}
