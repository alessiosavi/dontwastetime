package digest

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/*.tmpl
var templates embed.FS

// The Markdown copy is plain text; the HTML copy uses html/template so mail
// subjects and senders (attacker-controlled) are always escaped.
var (
	mdTmpl   = texttemplate.Must(texttemplate.ParseFS(templates, "templates/digest.md.tmpl"))
	htmlTmpl = htmltemplate.Must(htmltemplate.ParseFS(templates, "templates/digest.html.tmpl"))
)

func render(v view) (md, html string, err error) {
	var a, b bytes.Buffer
	if err := mdTmpl.Execute(&a, v); err != nil {
		return "", "", err
	}
	if err := htmlTmpl.Execute(&b, v); err != nil {
		return "", "", err
	}
	return a.String(), b.String(), nil
}
