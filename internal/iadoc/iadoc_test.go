package iadoc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenera docs/ia a partir do template")

// A cópia versionada em docs/ia não pode divergir do template: quem lê o
// repositório (ou copia o arquivo à mão) tem que ver a mesma instrução que o
// mirante serve. Rode `go test ./internal/iadoc -update` após mudar o template.
func TestDocsIAEmDia(t *testing.T) {
	gen := map[string]func(Data) (string, error){
		"integrar-mirante.md":                     Markdown,
		"claude/skills/integrar-mirante/SKILL.md": Skill,
		"kiro/steering/mirante.md":                Steering,
	}
	for rel, fn := range gen {
		want, err := fn(StaticData)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "docs", "ia", filepath.FromSlash(rel))
		if *update {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != want {
			t.Errorf("docs/ia/%s desatualizado em relação ao template — rode: go test ./internal/iadoc -update", rel)
		}
	}
}

func TestVersaoVivaTrazDadosReais(t *testing.T) {
	d := FromUpstreams("https://mirante.interno/", map[string]any{
		"llm_urls":         map[string]string{"ollama": "http://ollama:11434"},
		"mcp_urls":         map[string]string{"k8s-ts-mcp": "http://hub:8443/mcp", "knowledge-mcp": "http://k:8444"},
		"inject_reasoning": true,
	})
	md, err := Markdown(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Gerado pelo mirante em **https://mirante.interno**",
		"`ollama` → `http://ollama:11434`",
		"`k8s-ts-mcp` → `http://hub:8443/mcp`",
		"https://mirante.interno/p/<agente>/llm/ollama/v1",
		"**ligada**",
		"class Mirante:",         // cliente Python embutido
		"export class Mirante {", // cliente TS embutido
	} {
		if !strings.Contains(md, want) {
			t.Errorf("faltou %q", want)
		}
	}
	if strings.Contains(md, "[[") || strings.Contains(md, "<no value>") {
		t.Error("template mal renderizado")
	}
	// knowledge-mcp antes de k8s-ts-mcp? ordenado por nome: k8s < knowledge
	if strings.Index(md, "`k8s-ts-mcp` →") > strings.Index(md, "`knowledge-mcp` →") {
		t.Error("upstreams deveriam vir ordenados")
	}
}

func TestFormatosDeFerramenta(t *testing.T) {
	s, _ := Skill(StaticData)
	if !strings.HasPrefix(s, "---\nname: integrar-mirante\ndescription: ") {
		t.Errorf("frontmatter da skill do Claude Code: %q", s[:80])
	}
	k, _ := Steering(StaticData)
	if !strings.HasPrefix(k, "---\ninclusion: manual\n---\n") {
		t.Errorf("frontmatter do steering do Kiro: %q", k[:40])
	}
}
