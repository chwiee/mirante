package mirante

import (
	"reflect"
	"testing"
)

func TestReasoningParamsIdaEVolta(t *testing.T) {
	orig := map[string]any{"type": "object", "required": []any{"cluster_id"},
		"properties": map[string]any{"cluster_id": map[string]any{"type": "string"}}}
	aug := WithReasoningParams(orig)

	if req := aug["required"].([]any); !reflect.DeepEqual(req, []any{"cluster_id", ArgReason, ArgConfidence}) {
		t.Fatalf("required = %v", req)
	}
	if _, ok := orig["properties"].(map[string]any)[ArgReason]; ok {
		t.Fatal("schema original foi alterado")
	}

	clean, reason, conf := SplitReasoning(map[string]any{"cluster_id": "a", ArgReason: "porque sim", ArgConfidence: 0.7})
	if !reflect.DeepEqual(clean, map[string]any{"cluster_id": "a"}) || reason != "porque sim" || conf == nil || *conf != 0.7 {
		t.Fatalf("clean=%v reason=%q conf=%v", clean, reason, conf)
	}
}

// Visto ao vivo com qwen2.5:7b: o modelo devolveu "confiança" com cedilha.
func TestSplitReasoningToleraVariacoes(t *testing.T) {
	cases := []struct {
		args map[string]any
		conf float64
	}{
		{map[string]any{"q": 1, "motivo": "m", "confiança": 0.42}, 0.42},
		{map[string]any{"q": 1, "Motivo": "m", "Confianca": "80%"}, 0.8},
		{map[string]any{"q": 1, "rationale": "m", "confidence": 75}, 0.75},
		{map[string]any{"q": 1, "reason": "m", "CONFIDENCE": "0,9"}, 0.9},
	}
	for _, c := range cases {
		clean, reason, conf := SplitReasoning(c.args)
		if !reflect.DeepEqual(clean, map[string]any{"q": 1}) || reason != "m" || conf == nil || *conf != c.conf {
			t.Errorf("%v → clean=%v reason=%q conf=%v", c.args, clean, reason, conf)
		}
	}
	if _, _, conf := SplitReasoning(map[string]any{"confidence": "alta"}); conf != nil {
		t.Errorf("confiança não numérica deveria virar nil, veio %v", *conf)
	}
}
