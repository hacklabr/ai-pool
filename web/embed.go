// Package web empacota os assets estáticos e templates da UI admin via embed.FS.
//
// Estrutura:
//
//	web/
//	  embed.go           # este arquivo: //go:embed all:templates all:static
//	  templates/         # templates HTML (html/template)
//	    layouts/         # shells reusáveis (base.html)
//	    partials/        # componentes (topbar, sidebar, badges, etc.)
//	    pages/           # uma entrada por tela (8 telas)
//	  static/
//	    css/             # CSS próprio (ITCSS+BEM) — nosso design system
//
// Decisão F8 (Frontend Architect): CSS próprio é embed (essencial, pequeno,
// self-contained). JS libs (HTMX, Alpine.js, ECharts) são servidas via CDN
// com SRI hashes — documentado em templates/partials/head.html. Se a rede
// interna exigir offline, baixar para static/vendor/ e ajustar os <script src>.
//
// Compilação: o `go build` inclui o embed.FS no binário único. K8s deployment
// fica em 1 container, 1 binário — alinhado à spec §3.6 (readOnlyRootFilesystem
// compatível: assets embedded não escrevem no disco).
package web

import (
	"embed"
	"io/fs"
)

// Assets é o embed.FS raiz contendo templates/ e static/.
//
// Diretivas `all:` garantem inclusão de arquivos começando com `.` ou `_`
// (útil para partials nomeados como `_row.html`).
//
//go:embed all:templates all:static
var Assets embed.FS

// TemplatesFS retorna o subtree "templates" como fs.FS isolado.
// Usado por admin.newTemplateRenderer para carregar templates via fs.WalkDir.
func TemplatesFS() fs.FS {
	sub, err := fs.Sub(Assets, "templates")
	if err != nil {
		// Erro só ocorre se "templates" não existe no embed — não deve acontecer
		// em build normal (fallback no caller).
		panic("web: templates subtree missing from embed.FS: " + err.Error())
	}
	return sub
}

// StaticFS retorna o subtree "static" como fs.FS isolado.
// Servido pelo admin handler em GET /static/* via http.FileServer.
func StaticFS() fs.FS {
	sub, err := fs.Sub(Assets, "static")
	if err != nil {
		panic("web: static subtree missing from embed.FS: " + err.Error())
	}
	return sub
}
