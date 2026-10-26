package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

const siteURL = "https://docktail.org"

type heading struct {
	Level int
	Text  string
	ID    string
}

// page is one rendered docs page: a docs/*.md file, or a docs/guides/*.md
// file. Each file becomes its own URL so search engines can rank it on its
// own; docs.md and llms-full.txt still carry everything in one file.
type page struct {
	// Source is the path relative to the docs directory, e.g. "04-labels.md"
	// or "guides/01-jellyfin.md". Cross-file links resolve against it.
	Source string
	// Slug is the URL path below /docs/: "" for the overview, "labels",
	// "guides" for the guides index, "guides/jellyfin-over-tailscale".
	Slug  string
	Guide bool
	// Section is set when the file opens with ## because it is a section of
	// the one-page docs.md; rendered on its own, every heading moves up a
	// level so that ## becomes the page's H1.
	Section  bool
	Title    string // the page's H1 text
	NavTitle string // shorter sidebar label, from the nav: front matter key
	SEOTitle string // the <title>, from the title: front matter key
	// Description is the meta description: the description: front matter
	// key, else the first paragraph.
	Description string
	Markdown    []byte // source with the front matter comment removed
	Headings    []heading
}

func (p *page) URL() string {
	if p.Slug == "" {
		return "/docs/"
	}
	return "/docs/" + p.Slug + "/"
}

// MarkdownURL is the page's Markdown alternate. The overview points at the
// full docs.md, since that is what a reader of the index wants.
func (p *page) MarkdownURL() string {
	if p.Slug == "" {
		return "/docs.md"
	}
	return "/docs/" + p.Slug + ".md"
}

type sidebarItem struct {
	Text string
	ID   string
}

type navItem struct {
	Text     string
	URL      string
	Current  bool
	Children []sidebarItem
}

type navGroup struct {
	Label string
	Items []navItem
}

type navLink struct {
	Text string
	URL  string
}

type pageData struct {
	Title       string
	SEOTitle    string
	Description string
	URL         string
	MarkdownURL string
	Stylesheet  string
	Body        template.HTML
	Groups      []navGroup
	Prev        *navLink
	Next        *navLink
	JSONLD      template.JS
	Redirects   template.JS
	GeneratedAt string
	GitHubStars string
}

// githubStars is the repository's star count for the header, formatted like the
// website nav ("1.3k"), fetched once per run. It is only as fresh as the last
// render; when GitHub is unreachable or rate limits the build, the header falls
// back to the bare GitHub mark rather than failing the build.
func githubStars() string {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/marvinvr/docktail", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "docktail-docsgen")
	res, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close() //nolint:errcheck // read-only response body
	if res.StatusCode != http.StatusOK {
		return ""
	}
	var repo struct {
		Stars *int `json:"stargazers_count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&repo); err != nil || repo.Stars == nil {
		return ""
	}
	n := *repo.Stars
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
}

func main() {
	sourceDir := flag.String("source", "docs", "directory containing canonical markdown files")
	websiteDir := flag.String("website", ".docs-preview", "output directory for the generated docs (the docktail-website repo passes its own site directory)")
	stylesheet := flag.String("stylesheet", "", "site-absolute URL of a compiled stylesheet for the docs pages; empty loads the Tailwind CDN instead, for a local preview")
	flag.Parse()

	pages, err := readPages(*sourceDir)
	if err != nil {
		fatal(err)
	}
	bySource := map[string]*page{}
	for _, p := range pages {
		bySource[p.Source] = p
	}
	if err := checkLinks(pages, bySource); err != nil {
		fatal(err)
	}
	redirects, err := legacyAnchors(pages)
	if err != nil {
		fatal(err)
	}

	generatedAt := time.Now().UTC().Format("2006-01-02")
	stars := githubStars()
	for i, p := range pages {
		html, err := renderPage(p, bySource)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", p.Source, err))
		}
		data := pageData{
			Title:       p.Title,
			SEOTitle:    p.SEOTitle,
			Description: p.Description,
			URL:         p.URL(),
			MarkdownURL: p.MarkdownURL(),
			Stylesheet:  *stylesheet,
			Body:        template.HTML(html), //nolint:gosec // G203: HTML rendered from this repo's own docs/*.md
			Groups:      buildNav(pages, p),
			JSONLD:      jsonLD(p, generatedAt),
			GeneratedAt: generatedAt,
			GitHubStars: stars,
		}
		if i > 0 {
			data.Prev = &navLink{Text: pages[i-1].NavTitle, URL: pages[i-1].URL()}
		}
		if i < len(pages)-1 {
			data.Next = &navLink{Text: pages[i+1].NavTitle, URL: pages[i+1].URL()}
		}
		if p.Slug == "" {
			data.Redirects = mustJS(redirects)
		}

		dir := filepath.Join(*websiteDir, "docs", filepath.FromSlash(p.Slug))
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: public website output, served as static files
			fatal(err)
		}
		if err := writeFile(filepath.Join(dir, "index.html"), renderTemplate(data)); err != nil {
			fatal(err)
		}
		if p.Slug != "" {
			md := generatedMarkdown(promoteHeadings(absoluteLinks(p, bySource)))
			if err := writeFile(filepath.Join(*websiteDir, "docs", filepath.FromSlash(p.Slug)+".md"), md); err != nil {
				fatal(err)
			}
		}
	}

	var full strings.Builder
	for i, p := range pages {
		if i > 0 {
			full.WriteString("\n\n")
		}
		full.WriteString(strings.TrimSpace(absoluteLinks(p, bySource)))
		full.WriteByte('\n')
	}
	if err := writeFile(filepath.Join(*websiteDir, "docs.md"), generatedMarkdown(full.String())); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "llms.txt"), renderLLMS(pages, redirects)); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "llms-full.txt"), renderLLMSFull(full.String())); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "sitemap.xml"), renderSitemap(pages, generatedAt)); err != nil {
		fatal(err)
	}
}

// readPages reads docs/*.md, then docs/guides/*.md, each sorted by name. The
// order is the sidebar order and the prev/next order. In the guides
// directory, README.md is the guides index and comes first.
func readPages(sourceDir string) ([]*page, error) {
	core, err := filepath.Glob(filepath.Join(sourceDir, "*.md"))
	if err != nil {
		return nil, err
	}
	if len(core) == 0 {
		return nil, fmt.Errorf("no markdown files found in %s", sourceDir)
	}
	sort.Strings(core)
	guides, err := filepath.Glob(filepath.Join(sourceDir, "guides", "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Slice(guides, func(i, j int) bool {
		ri, rj := filepath.Base(guides[i]) == "README.md", filepath.Base(guides[j]) == "README.md"
		if ri != rj {
			return ri
		}
		return guides[i] < guides[j]
	})

	var pages []*page
	slugs := map[string]string{}
	for _, file := range append(core, guides...) {
		rel, err := filepath.Rel(sourceDir, file)
		if err != nil {
			return nil, err
		}
		p, err := readPage(file, filepath.ToSlash(rel))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if prev, ok := slugs[p.Slug]; ok {
			return nil, fmt.Errorf("%s and %s both render to %s", prev, p.Source, p.URL())
		}
		slugs[p.Slug] = p.Source
		pages = append(pages, p)
	}
	if pages[0].Slug != "" {
		return nil, fmt.Errorf("%s must open with a level-1 heading: the first docs file is the /docs/ overview", pages[0].Source)
	}
	return pages, nil
}

// frontMatter is an optional HTML comment at the very top of a docs file, so
// GitHub renders nothing for it:
//
//	<!--
//	description: One or two sentences for the meta description.
//	nav: Short sidebar label
//	title: The <title>, when the H1 alone is not what should rank
//	-->
var frontMatter = regexp.MustCompile(`(?s)\A<!--(.*?)-->\s*`)

var numberPrefix = regexp.MustCompile(`^\d{2}-`)

func readPage(file, rel string) (*page, error) {
	body, err := os.ReadFile(file) //nolint:gosec // G304: path comes from globbing the docs source dir
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)

	meta := map[string]string{}
	if m := frontMatter.FindSubmatchIndex(body); m != nil {
		for _, line := range strings.Split(string(body[m[2]:m[3]]), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			switch key = strings.TrimSpace(key); key {
			case "description", "nav", "title":
				meta[key] = strings.TrimSpace(value)
			default:
				return nil, fmt.Errorf("unknown front matter key %q", key)
			}
		}
		body = body[m[1]:]
	}

	hs := parseHeadings(body)
	if len(hs) == 0 || hs[0].Level > 2 {
		return nil, fmt.Errorf("must start with a level-1 or level-2 heading")
	}
	section := hs[0].Level == 2
	if section {
		for i := range hs {
			hs[i].Level--
		}
	}

	dir, name := path.Split(rel)
	slug := numberPrefix.ReplaceAllString(strings.TrimSuffix(name, ".md"), "")
	guide := dir == "guides/"
	switch {
	case dir == "" && !section:
		slug = ""
	case guide && name == "README.md":
		slug = "guides"
	case guide:
		slug = "guides/" + slug
	case dir != "":
		return nil, fmt.Errorf("docs pages live in docs/ or docs/guides/")
	}

	p := &page{
		Source:      rel,
		Slug:        slug,
		Guide:       guide,
		Section:     section,
		Title:       hs[0].Text,
		NavTitle:    meta["nav"],
		SEOTitle:    meta["title"],
		Description: meta["description"],
		Markdown:    append(body, '\n'),
		Headings:    hs,
	}
	if p.NavTitle == "" {
		p.NavTitle = p.Title
	}
	if p.SEOTitle == "" {
		p.SEOTitle = p.Title + " | DockTail Docs"
	}
	if p.Description == "" {
		p.Description = firstParagraph(body)
	}
	return p, nil
}

// docLink matches a markdown link to another docs file, with an optional
// heading hash: 04-labels.md#funnel-labels, ../02-installation.md,
// guides/01-jellyfin-over-tailscale.md.
var docLink = regexp.MustCompile(`\]\(((?:\.\./)?(?:guides/)?[A-Za-z0-9-]+\.md)(#[a-z0-9-]+)?\)`)

// inPageLink matches a markdown link to a heading of the same file.
var inPageLink = regexp.MustCompile(`\]\(#([a-z0-9-]+)\)`)

// resolve maps a docs file link from p to its page.
func resolve(p *page, target string, bySource map[string]*page) (*page, error) {
	t, ok := bySource[path.Clean(path.Join(path.Dir(p.Source), target))]
	if !ok {
		return nil, fmt.Errorf("link to unknown docs file %s", target)
	}
	return t, nil
}

func hasHeading(p *page, id string) bool {
	for _, h := range p.Headings {
		if h.ID == id {
			return true
		}
	}
	return false
}

// checkLinks fails the build on a link to a file or heading that does not
// exist, which is also what a hash pointing at a duplicate heading's first
// copy looks like to a reader: renaming one of two equal headings fixes both.
func checkLinks(pages []*page, bySource map[string]*page) error {
	for _, p := range pages {
		for _, m := range docLink.FindAllSubmatch(p.Markdown, -1) {
			t, err := resolve(p, string(m[1]), bySource)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Source, err)
			}
			if id := strings.TrimPrefix(string(m[2]), "#"); id != "" && !hasHeading(t, id) {
				return fmt.Errorf("%s: link to missing heading %s#%s", p.Source, t.Source, id)
			}
		}
		for _, m := range inPageLink.FindAllSubmatch(p.Markdown, -1) {
			if !hasHeading(p, string(m[1])) {
				return fmt.Errorf("%s: link to missing heading #%s (a heading in another file needs NN-name.md#%s)", p.Source, m[1], m[1])
			}
		}
	}
	return nil
}

// rewriteLinks points docs file links at the rendered pages. base is
// prepended to every page URL ("" for the HTML, the site origin for the
// Markdown copies); in-page hashes are left alone unless absolute is set.
func rewriteLinks(p *page, bySource map[string]*page, base string, absolute bool) string {
	out := docLink.ReplaceAllFunc(p.Markdown, func(match []byte) []byte {
		parts := docLink.FindSubmatch(match)
		t, _ := resolve(p, string(parts[1]), bySource) // checked by checkLinks
		hash := string(parts[2])
		if t == p && hash != "" && !absolute {
			return []byte("](" + hash + ")")
		}
		return []byte("](" + base + t.URL() + hash + ")")
	})
	if absolute {
		out = inPageLink.ReplaceAll(out, []byte("]("+base+p.URL()+"#$1)"))
	}
	return string(out)
}

// absoluteLinks is p's Markdown with every docs link absolute, for the
// Markdown and plain-text copies an agent reads outside the site.
func absoluteLinks(p *page, bySource map[string]*page) string {
	return rewriteLinks(p, bySource, siteURL, true)
}

// legacyAnchors maps every heading id of the old one-page /docs/ to its new
// home, so /docs/#labels and the like keep working: the rendered overview
// page redirects from the hash. Released agent binaries log such links too,
// so every heading is mapped, not just the popular ones. The ids are
// recomputed from the concatenated core files exactly as the single page
// rendered them, duplicates' -1 suffixes included.
func legacyAnchors(pages []*page) (map[string]string, error) {
	var combined bytes.Buffer
	var core []*page
	for _, p := range pages {
		if p.Guide {
			continue
		}
		if combined.Len() > 0 {
			combined.WriteString("\n\n")
		}
		combined.Write(bytes.TrimSpace(p.Markdown))
		combined.WriteByte('\n')
		core = append(core, p)
	}
	old := parseHeadings(combined.Bytes())

	redirects := map[string]string{}
	k := 0
	for _, p := range core {
		for j, h := range p.Headings {
			if k >= len(old) || old[k].Text != h.Text {
				return nil, fmt.Errorf("legacy anchor map out of step at %s %q", p.Source, h.Text)
			}
			id := old[k].ID
			k++
			if p.Slug == "" || id == "" {
				continue // the overview's own headings are still on /docs/
			}
			target := p.URL()
			if j > 0 {
				target += "#" + h.ID
			}
			redirects[id] = target
		}
	}
	return redirects, nil
}

func newMarkdown() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.Strikethrough,
			extension.TaskList,
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(goldhtml.WithUnsafe()),
	)
}

func renderPage(p *page, bySource map[string]*page) (string, error) {
	source := []byte(rewriteLinks(p, bySource, "", false))
	md := newMarkdown()
	doc := md.Parser().Parse(text.NewReader(source))
	if p.Section {
		_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if h, ok := node.(*ast.Heading); ok && entering {
				h.Level--
			}
			return ast.WalkContinue, nil
		})
	}
	var rendered bytes.Buffer
	if err := md.Renderer().Render(&rendered, source, doc); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

// promoteHeadings turns a section file's ## title into the # of a standalone
// Markdown page, leaving fenced code (shell comments) alone.
func promoteHeadings(markdown string) string {
	lines := strings.Split(markdown, "\n")
	fenced := false
	shift := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(line, "#") {
			continue
		}
		if !shift && !strings.HasPrefix(line, "## ") {
			return markdown // already opens with an H1
		}
		shift = true
		if strings.HasPrefix(line, "##") {
			lines[i] = line[1:]
		}
	}
	return strings.Join(lines, "\n")
}

func collectHeadings(doc ast.Node, source []byte) []heading {
	var headings []heading
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := node.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}

		headings = append(headings, heading{
			Level: h.Level,
			Text:  strings.TrimSpace(headingText(h, source)),
			ID:    headingID(h),
		})
		return ast.WalkContinue, nil
	})
	return headings
}

func parseHeadings(source []byte) []heading {
	doc := newMarkdown().Parser().Parse(text.NewReader(source))
	return collectHeadings(doc, source)
}

// firstParagraph is the plain text of the first paragraph, the fallback meta
// description for a page without a description: key.
func firstParagraph(source []byte) string {
	doc := newMarkdown().Parser().Parse(text.NewReader(source))
	var out string
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if p, ok := node.(*ast.Paragraph); ok && entering {
			out = strings.Join(strings.Fields(headingText(p, source)), " ")
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return out
}

func headingText(node ast.Node, source []byte) string {
	var out strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch n := child.(type) {
		case *ast.Text:
			out.Write(n.Segment.Value(source))
			if n.SoftLineBreak() {
				out.WriteByte(' ')
			}
		case *ast.CodeSpan:
			for codeChild := n.FirstChild(); codeChild != nil; codeChild = codeChild.NextSibling() {
				if textNode, ok := codeChild.(*ast.Text); ok {
					out.Write(textNode.Segment.Value(source))
				}
			}
		default:
			out.WriteString(headingText(n, source))
		}
	}
	return out.String()
}

func headingID(node ast.Node) string {
	value, ok := node.AttributeString("id")
	if !ok {
		return ""
	}
	switch id := value.(type) {
	case []byte:
		return string(id)
	case string:
		return id
	default:
		return fmt.Sprint(id)
	}
}

// buildNav is the sidebar: every page, grouped into the docs and the guides,
// with the current page's H2s listed under it for in-page navigation.
func buildNav(pages []*page, current *page) []navGroup {
	groups := []navGroup{{Label: "Documentation"}, {Label: "How-To Guides"}}
	for _, p := range pages {
		item := navItem{Text: p.NavTitle, URL: p.URL(), Current: p == current}
		if p.Slug == "" {
			item.Text = "Overview"
		}
		if item.Current {
			for _, h := range p.Headings {
				if h.Level == 2 && h.ID != "" {
					item.Children = append(item.Children, sidebarItem{Text: h.Text, ID: h.ID})
				}
			}
		}
		g := &groups[0]
		if p.Guide {
			g = &groups[1]
		}
		g.Items = append(g.Items, item)
	}
	if len(groups[1].Items) == 0 {
		groups = groups[:1]
	}
	return groups
}

func jsonLD(p *page, generatedAt string) template.JS {
	url := siteURL + p.URL()
	crumbs := []map[string]any{
		{"@type": "ListItem", "position": 1, "name": "DockTail", "item": siteURL + "/"},
		{"@type": "ListItem", "position": 2, "name": "Docs", "item": siteURL + "/docs/"},
	}
	if p.Guide && p.Slug != "guides" {
		crumbs = append(crumbs, map[string]any{"@type": "ListItem", "position": 3, "name": "Guides", "item": siteURL + "/docs/guides/"})
	}
	if p.Slug != "" {
		crumbs = append(crumbs, map[string]any{"@type": "ListItem", "position": len(crumbs) + 1, "name": p.NavTitle, "item": url})
	}
	article := map[string]any{
		"@type":            "TechArticle",
		"headline":         p.Title,
		"description":      p.Description,
		"url":              url,
		"image":            siteURL + "/assets/og-image.jpeg",
		"dateModified":     generatedAt,
		"author":           map[string]any{"@type": "Person", "name": "marvinvr", "url": "https://marvinvr.ch"},
		"about":            []string{"Docker", "Tailscale", "Tailscale Services", "Container networking"},
		"isPartOf":         map[string]any{"@type": "WebSite", "name": "DockTail", "url": siteURL + "/"},
		"mainEntityOfPage": url,
	}
	return mustJS(map[string]any{
		"@context": "https://schema.org",
		"@graph":   []any{article, map[string]any{"@type": "BreadcrumbList", "itemListElement": crumbs}},
	})
}

// mustJS encodes v for a <script> block. encoding/json escapes <, > and &,
// so the result cannot close the script element.
func mustJS(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		fatal(err)
	}
	return template.JS(b) //nolint:gosec // G203: JSON-encoded, with <, > and & escaped
}

func generatedMarkdown(markdown string) string {
	return "<!-- Generated from docs/*.md. Do not edit directly. -->\n\n" + markdown
}

func renderTemplate(data pageData) string {
	var out bytes.Buffer
	if err := docsTemplate.Execute(&out, data); err != nil {
		fatal(err)
	}
	return out.String()
}

// renderLLMS is the /llms.txt index (https://llmstxt.org): a summary, then
// links. Details live in llms-full.txt; this file stays short. redirects
// resolves old anchors, so a link here to a heading that moved fails the
// build instead of going stale.
func renderLLMS(pages []*page, redirects map[string]string) string {
	doc := func(oldID string) string {
		target, ok := redirects[oldID]
		if !ok {
			fatal(fmt.Errorf("llms.txt links to unknown docs heading #%s", oldID))
		}
		return siteURL + target
	}

	var out strings.Builder
	out.WriteString("# DockTail\n\n")
	out.WriteString("> DockTail is an open-source container that exposes Docker containers as native Tailscale Services, configured with `docktail.*` labels. DockTail Cloud is an optional paid service that monitors those hosts and services, explains why something is unreachable, and lets AI agents find the root cause of an incident over MCP or a REST API.\n\n")
	out.WriteString("DockTail runs as one container next to your apps (`ghcr.io/marvinvr/docktail:latest`) and needs the Docker socket and a Tailscale socket. DockTail Cloud is opt-in through one environment variable, `DOCKTAIL_CLOUD_KEY`.\n\n")

	out.WriteString("## Docs\n\n")
	out.WriteString("- [Full documentation](https://docktail.org/llms-full.txt): every page below in one plain-text file\n")
	out.WriteString("- [Documentation as Markdown](https://docktail.org/docs.md): the same content, for Markdown readers\n")
	writePages := func(guides bool) {
		for _, p := range pages {
			if p.Guide != guides || p.Slug == "" || p.Slug == "guides" {
				continue
			}
			fmt.Fprintf(&out, "- [%s](%s%s): %s\n", p.Title, siteURL, p.MarkdownURL(), p.Description)
		}
	}
	writePages(false)

	out.WriteString("\n## Guides\n\n")
	writePages(true)

	out.WriteString("\n## DockTail Cloud\n\n")
	out.WriteString("- [DockTail Cloud](https://docktail.org/cloud.md): what it does, plans and the 30-day trial ([web page](https://docktail.org/cloud/))\n")
	out.WriteString("- [MCP server and API](https://docktail.org/cloud/agents.md): connect an AI agent to find the root cause of incidents ([web page](https://docktail.org/cloud/agents/))\n")
	fmt.Fprintf(&out, "- [Setup](%s): account, workspace, plan, then `DOCKTAIL_CLOUD_KEY` on the agent\n", doc("how-to-enable"))
	fmt.Fprintf(&out, "- [API and MCP reference](%s): keys, scopes, endpoints and tools\n", doc("api-and-mcp-server"))
	out.WriteString("- [Plans and pricing](https://docktail.org/cloud/#pricing)\n")
	out.WriteString("- [OpenAPI document](https://cloud.docktail.org/api/v1/openapi.json)\n\n")

	out.WriteString("## Agent Guidance\n\n")
	out.WriteString("- The user signs up at https://cloud.docktail.org/login and creates the workspace and plan; an agent cannot. Homelab and Team start with a 30-day trial that needs a card; a workspace connects no host until it has a plan or a running trial.\n")
	out.WriteString("- To connect a host, set `DOCKTAIL_CLOUD_KEY` (a `dtc_...` workspace key from Cloud) on the DockTail container and recreate it.\n")
	out.WriteString("- To use the MCP server (`https://cloud.docktail.org/api/mcp`) or the REST API (`https://cloud.docktail.org/api/v1`), the user creates an API key (`dtk_...`) on the MCP or API page in Cloud and sends it as `Authorization: Bearer dtk_...`. A `read` key is enough to investigate; only `read_write` can acknowledge, resolve or start maintenance.\n")
	out.WriteString("- Prefer https://docktail.org/llms-full.txt over the README for complete configuration details.\n\n")

	out.WriteString("## Optional\n\n")
	out.WriteString("- [Source code](https://github.com/marvinvr/docktail)\n")
	out.WriteString("- [Releases](https://github.com/marvinvr/docktail/releases)\n")
	out.WriteString("- [Security and data handling](https://docktail.org/security/)\n")
	out.WriteString("- [Changelog](https://docktail.org/changelog/)\n")
	return out.String()
}

func renderLLMSFull(markdown string) string {
	return "# DockTail Full Documentation\n\nSource: https://docktail.org/docs/ · Index: https://docktail.org/llms.txt · DockTail Cloud: https://docktail.org/cloud.md · MCP server and API: https://docktail.org/cloud/agents.md\n\n" + markdown
}

func renderSitemap(pages []*page, lastmod string) string {
	var out strings.Builder
	url := func(loc, priority string) {
		fmt.Fprintf(&out, "  <url>\n    <loc>%s%s</loc>\n    <lastmod>%s</lastmod>\n    <changefreq>weekly</changefreq>\n    <priority>%s</priority>\n  </url>\n", siteURL, loc, lastmod, priority)
	}
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	out.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	url("/", "1.0")
	url("/cloud/", "0.9")
	for _, p := range pages {
		priority := "0.8"
		if p.Slug == "" {
			priority = "0.9"
		}
		url(p.URL(), priority)
	}
	url("/docs.md", "0.7")
	url("/llms.txt", "0.5")
	url("/llms-full.txt", "0.5")
	out.WriteString("</urlset>\n")
	return out.String()
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644) //nolint:gosec // G306: public website output, served as static files
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "docsgen: %v\n", err)
	os.Exit(1)
}
