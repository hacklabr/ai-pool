package admin

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// =============================================================================
// Renderer de templates HTML (F8).
//
// Carrega templates de um fs.FS (tipicamente embed.FS do pacote web) com
// composição via {{template "name" .}}. html/template escapa output por padrão
// (defesa XSS automática). FuncMap adiciona helpers usados nos templates.
//
// Estrutura esperada do fs.FS (subtree "templates"):
//
//	templates/
//	  layouts/base.html          # shell com topbar/sidebar + {{block "page-content"}}
//	  partials/*.html            # componentes reusáveis (head, topbar, sidebar)
//	  pages/*.html               # uma por tela (cada uma define "page-content")
//
// === Estratégia para layouts com html/template ===
//
// html/template NÃO suporta herança clássica — todos os {{define}} são
// globais em um *Template, então múltiplas páginas definindo "page-content"
// colidiriam (apenas o último parseado venceria). Solução adotada: criar
// uma INSTÂNCIA separada de *template.Template por página, cada uma com o
// layout + partials + AQUELA página específica. Cacheada em map no startup.
//
// Isto é thread-safe (template.Template é safe após Parse) e performático
// (parse só no startup; renderização é só Execute).
// =============================================================================

// templateRenderer carrega e executa templates HTML.
//
// pages é um map pageName → *template.Template pré-parseado. Cada valor
// contém layout + partials + a página específica (com seu block "page-content"
// único naquele namespace).
type templateRenderer struct {
	pages map[string]*template.Template
}

// newTemplateRenderer carrega todos os .html do fs.FS fornecido e constrói
// uma instância de template por página.
//
// Retorna erro se qualquer arquivo tiver sintaxe inválida. Falha cedo no
// startup (não em runtime) — preferível a panics intermitentes.
//
// NewTemplateRenderer é o alias exportado para uso no main.go (DI).
func newTemplateRenderer(tplFS fs.FS) (*templateRenderer, error) {
	// Lê layout e partials como strings cruas (parse único por página).
	var layoutHTML string
	partialsHTML := map[string]string{}
	pagesHTML := map[string]string{}

	err := fs.WalkDir(tplFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		data, rerr := fs.ReadFile(tplFS, path)
		if rerr != nil {
			return fmt.Errorf("read template %s: %w", path, rerr)
		}
		switch {
		case strings.HasPrefix(path, "layouts/"):
			if layoutHTML != "" {
				return fmt.Errorf("multiple layouts not supported; already have one, found %s", path)
			}
			layoutHTML = string(data)
		case strings.HasPrefix(path, "partials/"):
			partialsHTML[path] = string(data)
		case strings.HasPrefix(path, "pages/"):
			pagesHTML[path] = string(data)
		default:
			// Outros arquivos .html no root — ignoramos (pode ser README etc).
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if layoutHTML == "" {
		return nil, fmt.Errorf("layout not found (expected templates/layouts/base.html)")
	}

	// Para cada página, monta *template independente com layout + partials + página.
	pages := make(map[string]*template.Template, len(pagesHTML))
	for pageName, pageHTML := range pagesHTML {
		t := template.New("").Funcs(templateFuncMap())

		// Partials primeiro (register nomes).
		for name, html := range partialsHTML {
			if _, err := t.New(name).Parse(html); err != nil {
				return nil, fmt.Errorf("parse partial %s (for page %s): %w", name, pageName, err)
			}
		}
		// Layout.
		if _, err := t.New("layouts/base.html").Parse(layoutHTML); err != nil {
			return nil, fmt.Errorf("parse layout (for page %s): %w", pageName, err)
		}
		// Página específica — define block "page-content" único neste namespace.
		if _, err := t.New(pageName).Parse(pageHTML); err != nil {
			return nil, fmt.Errorf("parse page %s: %w", pageName, err)
		}
		pages[pageName] = t
	}

	return &templateRenderer{pages: pages}, nil
}

// NewTemplateRenderer é o export wrapper para uso em DI (main.go).
// Mantém a lógica em newTemplateRenderer (não-exportado) para clareza de
// escopo interno vs API pública.
func NewTemplateRenderer(tplFS fs.FS) (*templateRenderer, error) {
	return newTemplateRenderer(tplFS)
}

// pageData é o contexto padrão passado a toda página renderizada.
//
// Campos são populados pelo handler antes de chamar renderPage. Templates
// acessam via {{.Title}}, {{.User}}, {{.CSRFToken}}, etc. Content é o payload
// específico de cada página (struct dedicada).
type pageData struct {
	Title string
	// User é o usuário autenticado (nil em páginas públicas como login).
	User      *sessionContext
	CSRFToken string
	ActiveNav string // marca item ativo no sidebar (ex.: "dashboard")
	Flash     string // mensagem one-shot (opcional)
	// Content é o payload específico de cada página (struct dedicada).
	Content any
	// DevMode influencia UI (mostra badges de debug, etc.).
	DevMode bool
}

// renderPage escreve o layout base + página indicada no ResponseWriter.
//
// pageName é o caminho relativo dentro do FS (ex.: "pages/dashboard.html").
// Internamente: Lookup no cache de páginas (instância pré-parseada), e executa
// o template "layouts/base.html" que por sua vez renderiza {{template
// "page-content" .}} — block definido pela página.
func (r *templateRenderer) renderPage(w http.ResponseWriter, pageName string, data pageData) error {
	t, ok := r.pages[pageName]
	if !ok {
		return fmt.Errorf("page template not found: %s", pageName)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// ExecuteTemplate pelo nome do layout (não pelo da página) — o layout
	// referencia {{template "page-content" .}} que está definido no mesmo
	// namespace desta instância.
	if err := t.ExecuteTemplate(w, "layouts/base.html", data); err != nil {
		return fmt.Errorf("execute template %s: %w", pageName, err)
	}
	return nil
}

// renderPartial escreve apenas um partial (sem layout). Para respostas HTMX
// que trocam um fragmento da página (ex.: linha de tabela atualizada).
//
// Como partials não têm block "page-content", criamos uma instância fresh
// apenas com partials. Custo baixo (partials são pequenos).
func (r *templateRenderer) renderPartial(w http.ResponseWriter, pageName string, data any) error {
	// Reaproveita a primeira instância cacheada (todas têm os mesmos partials).
	for _, t := range r.pages {
		return t.ExecuteTemplate(w, pageName, data)
	}
	return fmt.Errorf("no cached templates; cannot resolve partial %s", pageName)
}

// templateFuncMap define helpers disponíveis em todos os templates via
// {{funcname args}}. Mantemos o set enxuto — preferimos lógica no handler.
func templateFuncMap() template.FuncMap {
	// toTime normaliza qualquer valor (time.Time, *time.Time, nil) para
	// (time.Time, bool). Usado por todas as funcs de formatação temporal.
	// Permite templates passarem ponteiros nil-safe sem pré-check.
	toTime := func(v any) (time.Time, bool) {
		switch x := v.(type) {
		case time.Time:
			return x, !x.IsZero()
		case *time.Time:
			if x == nil || x.IsZero() {
				return time.Time{}, false
			}
			return *x, true
		default:
			return time.Time{}, false
		}
	}
	return template.FuncMap{
		// fmtTime formata timestamps no padrão legível pt-BR.
		"fmtTime": func(v any) string {
			t, ok := toTime(v)
			if !ok {
				return "—"
			}
			return t.Local().Format("02/01/2006 15:04")
		},
		// fmtTimeRelativo mostra "há X minutos" / "em X horas" para datas próximas.
		"fmtTimeRel": func(v any) string {
			t, ok := toTime(v)
			if !ok {
				return "—"
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "agora"
			case d < time.Hour:
				return fmt.Sprintf("há %d min", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("há %d h", int(d.Hours()))
			default:
				return fmt.Sprintf("há %d d", int(d.Hours()/24))
			}
		},
		// fmtTimeUntil mostra "em X horas/minutos" para datas futuras (reativação).
		"fmtTimeUntil": func(v any) string {
			t, ok := toTime(v)
			if !ok {
				return "—"
			}
			d := time.Until(t)
			if d < 0 {
				return "vencido"
			}
			switch {
			case d < time.Minute:
				return "agora"
			case d < time.Hour:
				return fmt.Sprintf("em %d min", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("em %d h", int(d.Hours()))
			default:
				return fmt.Sprintf("em %d d", int(d.Hours()/24))
			}
		},
		// fmtNumber formata inteiros com separador de milhar (pt-BR usa ponto).
		"fmtNumber": func(n int64) string {
			if n == 0 {
				return "0"
			}
			s := fmt.Sprintf("%d", n)
			negative := strings.HasPrefix(s, "-")
			if negative {
				s = s[1:]
			}
			if len(s) <= 3 {
				if negative {
					return "-" + s
				}
				return s
			}
			var b strings.Builder
			pre := len(s) % 3
			if pre > 0 {
				b.WriteString(s[:pre])
				if len(s) > pre {
					b.WriteString(".")
				}
			}
			for i := pre; i < len(s); i += 3 {
				b.WriteString(s[i : i+3])
				if i+3 < len(s) {
					b.WriteString(".")
				}
			}
			if negative {
				return "-" + b.String()
			}
			return b.String()
		},
		// keyStatusClass retorna a classe CSS (BEM modifier) para o status da chave.
		// Mapeia os 4 estados visuais (Frontend §2.6) para cores.
		"keyStatusClass": func(status string) string {
			switch status {
			case "enabled":
				return "c-status-badge--success"
			case "manual_disabled":
				return "c-status-badge--neutral"
			case "auto_disabled":
				return "c-status-badge--warning"
			default:
				return "c-status-badge--neutral"
			}
		},
		"keyStatusLabel": func(status string) string {
			switch status {
			case "enabled":
				return "Ativa"
			case "manual_disabled":
				return "Desativada (manual)"
			case "auto_disabled":
				return "Auto-desativada"
			default:
				return status
			}
		},
		"tokenStatusClass": func(status string) string {
			switch status {
			case "active":
				return "c-status-badge--success"
			case "revoked":
				return "c-status-badge--danger"
			case "expired":
				return "c-status-badge--neutral"
			default:
				return "c-status-badge--neutral"
			}
		},
		"tokenStatusLabel": func(status string) string {
			switch status {
			case "active":
				return "Ativo"
			case "revoked":
				return "Revogado"
			case "expired":
				return "Expirado"
			default:
				return status
			}
		},
		"userStatusClass": func(status string) string {
			switch status {
			case "active":
				return "c-status-badge--success"
			case "disabled":
				return "c-status-badge--danger"
			default:
				return "c-status-badge--neutral"
			}
		},
		"userStatusLabel": func(status string) string {
			switch status {
			case "active":
				return "Ativo"
			case "disabled":
				return "Desativado"
			default:
				return status
			}
		},
		// jsonIndent retorna o JSON cru como string para exibição de metadata.
		// html/template escapa automaticamente (XSS-safe).
		"jsonIndent": func(b []byte) string {
			if len(b) == 0 {
				return "{}"
			}
			return string(b)
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			if n <= 3 {
				return s[:n]
			}
			return s[:n-3] + "..."
		},
		// providerSlug resolve um provider_id (int64) para o slug legível,
		// consultando o mapa id→slug fornecido no pageData.Content (campo
		// Providers de keysContent). Retorna "?" se não encontrado.
		"providerSlug": func(providerID int64, providers any) string {
			m, ok := providers.(map[int64]string)
			if !ok {
				return "?"
			}
			if slug, ok := m[providerID]; ok {
				return slug
			}
			return "?"
		},
		// lenSlice retorna length de qualquer slice via reflection-lite.
		// html/template já suporta {{len .X}} nativamente para slices — este
		// helper é fallback defensivo para casos onde o valor pode ser nil.
		"lenSlice": func(v any) int {
			if v == nil {
				return 0
			}
			// html/template já fornece {{len .X}}. Mantemos compat.
			type lener interface{ Len() int }
			if l, ok := v.(lener); ok {
				return l.Len()
			}
			return 0
		},
	}
}
