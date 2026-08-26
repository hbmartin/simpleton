package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func WriteAll(output string, pack domain.EvidencePack) error {
	if err := os.MkdirAll(output, 0o700); err != nil {
		return err
	}
	evidence, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return err
	}
	scoping, err := json.MarshalIndent(pack.Scoping, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"evidence-pack.json": append(evidence, '\n'),
		"scoping-pack.json":  append(scoping, '\n'),
		"backlog.md":         []byte(markdown(pack)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(output, name), content, 0o600); err != nil {
			return err
		}
	}
	html, err := renderHTML(pack)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, "backlog.html"), html, 0o600)
}

func markdown(pack domain.EvidencePack) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Simpleton Evidence — `%s`\n\n", pack.RunID)
	fmt.Fprintf(&out, "- Repository: `%s`\n", pack.Provenance.Repository)
	fmt.Fprintf(&out, "- Base: `%s`\n", pack.Provenance.BaseRevision)
	fmt.Fprintf(&out, "- Head: `%s`\n", pack.Provenance.HeadRevision)
	fmt.Fprintf(&out, "- Semantic mode: `%s`\n", map[bool]string{true: "advisory", false: "approved contract"}[pack.SemanticAdvisory])
	fmt.Fprintf(&out, "- Blockers selected: `%d`\n\n", len(pack.Blockers))
	out.WriteString("## Method results\n\n")
	for _, method := range pack.Methods {
		fmt.Fprintf(&out, "- `%s`: **%s**", method.ID, method.Status)
		if method.Reason != "" {
			fmt.Fprintf(&out, " — %s", method.Reason)
		}
		out.WriteString("\n")
	}
	out.WriteString("\n## SENSE backlog\n\n")
	if len(pack.Scoping.Opportunities) == 0 {
		out.WriteString("No ranked opportunities were emitted.\n")
	} else {
		for index, opportunity := range pack.Scoping.Opportunities {
			fmt.Fprintf(&out, "%d. **%s** (`%s`, score %.3f)\n", index+1, opportunity.Category, opportunity.Region, opportunity.Rank)
			for _, evidence := range opportunity.Evidence {
				fmt.Fprintf(&out, "   - %s\n", evidence)
			}
		}
	}
	out.WriteString("\n## Semantic evidence\n\n")
	fmt.Fprintf(&out, "Observed divergences: `%d`; Behavioral Witnesses: `%d`. This report never represents a safety verdict.\n", len(pack.Divergences), len(pack.Witnesses))
	return out.String()
}

func renderHTML(pack domain.EvidencePack) ([]byte, error) {
	const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Simpleton {{.RunID}}</title><style>
body{font:16px/1.5 system-ui,sans-serif;max-width:960px;margin:40px auto;padding:0 20px;color:#17212b}code{background:#eef2f5;padding:.1rem .3rem;border-radius:4px}.card{border:1px solid #d8e0e7;border-radius:10px;padding:16px;margin:12px 0}.muted{color:#5c6b77}.score{font-variant-numeric:tabular-nums;font-weight:700}table{border-collapse:collapse;width:100%}td,th{padding:8px;border-bottom:1px solid #e5e9ed;text-align:left}</style></head>
<body><h1>Simpleton Evidence</h1><p><code>{{.RunID}}</code></p><p class="muted">No safety or equivalence verdict is produced.</p>
<h2>Methods</h2><table><thead><tr><th>Method</th><th>Status</th><th>Reason</th></tr></thead><tbody>{{range .Methods}}<tr><td><code>{{.ID}}</code></td><td>{{.Status}}</td><td>{{.Reason}}</td></tr>{{end}}</tbody></table>
<h2>SENSE backlog</h2>{{if .Scoping.Opportunities}}{{range .Scoping.Opportunities}}<div class="card"><div class="score">{{printf "%.3f" .Rank}}</div><strong>{{.Category}}</strong> · <code>{{.Region}}</code><ul>{{range .Evidence}}<li>{{.}}</li>{{end}}</ul></div>{{end}}{{else}}<p>No ranked opportunities were emitted.</p>{{end}}
<h2>Semantic evidence</h2><p>Observed divergences: {{len .Divergences}} · Behavioral Witnesses: {{len .Witnesses}} · Selected blockers: {{len .Blockers}}</p></body></html>`
	tmpl, err := template.New("report").Parse(page)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, pack); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
