package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"os"
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

type heading struct {
	Level int
	Text  string
	ID    string
}

type sidebarItem struct {
	Text string
	ID   string
}

type sidebarSection struct {
	Text     string
	ID       string
	Children []sidebarItem
}

type fileSpec struct {
	Path     string
	IsIntro  bool
	H1ID     string
	Headings []heading
}

type pageData struct {
	Title       string
	Description string
	Body        template.HTML
	Headings    []heading
	Sections    []sidebarSection
	GeneratedAt string
}

func main() {
	sourceDir := flag.String("source", "docs", "directory containing canonical markdown files")
	websiteDir := flag.String("website", "website", "website output directory")
	flag.Parse()

	markdown, err := readMarkdown(*sourceDir)
	if err != nil {
		fatal(err)
	}

	rendered, headings, err := renderMarkdown([]byte(markdown))
	if err != nil {
		fatal(err)
	}

	specs, err := readFileSpecs(*sourceDir)
	if err != nil {
		fatal(err)
	}

	title := "DockTail Documentation"
	if len(headings) > 0 && headings[0].Level == 1 {
		title = headings[0].Text
	}

	generatedAt := time.Now().UTC().Format("2006-01-02")
	data := pageData{
		Title:       title,
		Description: "Documentation for DockTail, a tool that automatically exposes Docker containers as Tailscale Services using label-based configuration.",
		Body:        template.HTML(rendered),
		Headings:    headings,
		Sections:    buildSidebar(specs),
		GeneratedAt: generatedAt,
	}

	if err := os.MkdirAll(filepath.Join(*websiteDir, "docs"), 0o755); err != nil {
		fatal(err)
	}

	if err := writeFile(filepath.Join(*websiteDir, "docs", "index.html"), renderTemplate(data)); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "docs.md"), generatedMarkdown(markdown)); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "llms.txt"), renderLLMS(data)); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "llms-full.txt"), renderLLMSFull(markdown)); err != nil {
		fatal(err)
	}
	if err := writeFile(filepath.Join(*websiteDir, "sitemap.xml"), renderSitemap(generatedAt)); err != nil {
		fatal(err)
	}
}

func readMarkdown(sourceDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(sourceDir, "*.md"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no markdown files found in %s", sourceDir)
	}
	sort.Strings(matches)

	var out strings.Builder
	for i, path := range matches {
		body, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if i > 0 {
			out.WriteString("\n\n")
		}
		// Source files use NN-name.md#anchor so GitHub can jump across
		// pages. The generated site is one HTML page, so collapse those
		// to in-page hashes.
		out.Write(bytes.TrimSpace(rewriteDocFileLinks(body)))
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// docFileLink matches a markdown link to another numbered docs file, with
// an optional heading hash. The generated site concatenates those files.
var docFileLink = regexp.MustCompile(`\]\(\d{2}-[a-z0-9-]+\.md(#[a-z0-9-]+)?\)`)

func rewriteDocFileLinks(body []byte) []byte {
	return docFileLink.ReplaceAll(body, []byte("]($1)"))
}

func renderMarkdown(source []byte) (string, []heading, error) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.Strikethrough,
			extension.TaskList,
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(goldhtml.WithUnsafe()),
	)

	reader := text.NewReader(source)
	doc := md.Parser().Parse(reader)
	headings := collectHeadings(doc, source)

	var rendered bytes.Buffer
	if err := md.Renderer().Render(&rendered, source, doc); err != nil {
		return "", nil, err
	}
	return rendered.String(), headings, nil
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

func buildSidebar(specs []fileSpec) []sidebarSection {
	var sections []sidebarSection
	for _, spec := range specs {
		if spec.IsIntro {
			sec := sidebarSection{Text: "Overview", ID: spec.H1ID}
			for _, h := range spec.Headings {
				if h.ID == "" || h.Level != 2 {
					continue
				}
				sec.Children = append(sec.Children, sidebarItem{Text: h.Text, ID: h.ID})
			}
			sections = append(sections, sec)
			continue
		}
		var current *sidebarSection
		for _, h := range spec.Headings {
			if h.ID == "" {
				continue
			}
			switch h.Level {
			case 2:
				sections = append(sections, sidebarSection{Text: h.Text, ID: h.ID})
				current = &sections[len(sections)-1]
			case 3:
				if current == nil {
					continue
				}
				current.Children = append(current.Children, sidebarItem{Text: h.Text, ID: h.ID})
			}
		}
	}
	return sections
}

func readFileSpecs(sourceDir string) ([]fileSpec, error) {
	matches, err := filepath.Glob(filepath.Join(sourceDir, "*.md"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no markdown files found in %s", sourceDir)
	}
	sort.Strings(matches)

	specs := make([]fileSpec, 0, len(matches))
	for _, path := range matches {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		hs, err := parseHeadings(bytes.TrimSpace(body))
		if err != nil {
			return nil, err
		}
		spec := fileSpec{Path: path}
		for _, h := range hs {
			if h.Level == 1 && !spec.IsIntro {
				spec.IsIntro = true
				spec.H1ID = h.ID
				continue
			}
			spec.Headings = append(spec.Headings, h)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func parseHeadings(source []byte) ([]heading, error) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.Strikethrough,
			extension.TaskList,
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	reader := text.NewReader(source)
	doc := md.Parser().Parse(reader)
	return collectHeadings(doc, source), nil
}

func headingText(node ast.Node, source []byte) string {
	var out strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch n := child.(type) {
		case *ast.Text:
			out.Write(n.Segment.Value(source))
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

func renderLLMS(data pageData) string {
	var out strings.Builder
	out.WriteString("# DockTail\n\n")
	out.WriteString("DockTail automatically exposes Docker containers as Tailscale Services using label-based configuration.\n\n")
	out.WriteString("## Primary URLs\n\n")
	out.WriteString("- Homepage: https://docktail.org/\n")
	out.WriteString("- Human documentation: https://docktail.org/docs/\n")
	out.WriteString("- Markdown documentation: https://docktail.org/docs.md\n")
	out.WriteString("- Full LLM documentation: https://docktail.org/llms-full.txt\n")
	out.WriteString("- Source code: https://github.com/marvinvr/docktail\n")
	out.WriteString("- Container image: ghcr.io/marvinvr/docktail:latest\n\n")
	out.WriteString("## Documentation Sections\n\n")
	for _, h := range data.Headings {
		if h.Level > 3 || h.ID == "" {
			continue
		}
		prefix := "-"
		if h.Level == 3 {
			prefix = "  -"
		}
		fmt.Fprintf(&out, "%s %s: https://docktail.org/docs/#%s\n", prefix, h.Text, h.ID)
	}
	out.WriteString("\n## Agent Guidance\n\n")
	out.WriteString("Read https://docktail.org/llms-full.txt when you need the complete documentation in one text document. Use https://docktail.org/docs.md when Markdown structure matters. Prefer the generated docs over README for complete configuration details.\n")
	return out.String()
}

func renderLLMSFull(markdown string) string {
	return "# DockTail Full Documentation\n\nSource: https://docktail.org/docs/\n\n" + markdown
}

func renderSitemap(lastmod string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>https://docktail.org/</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>1.0</priority>
  </url>
  <url>
    <loc>https://docktail.org/docs/</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.9</priority>
  </url>
  <url>
    <loc>https://docktail.org/cloud/</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.9</priority>
  </url>
  <url>
    <loc>https://docktail.org/docs.md</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.7</priority>
  </url>
  <url>
    <loc>https://docktail.org/llms.txt</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.5</priority>
  </url>
  <url>
    <loc>https://docktail.org/llms-full.txt</loc>
    <lastmod>%s</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.5</priority>
  </url>
</urlset>
`, lastmod, lastmod, lastmod, lastmod, lastmod, lastmod)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "docsgen: %v\n", err)
	os.Exit(1)
}

var docsTemplate = template.Must(template.New("docs").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Docs — DockTail</title>
  <meta name="description" content="{{ .Description }}">
  <meta property="og:title" content="Docs — DockTail">
  <meta property="og:description" content="{{ .Description }}">
  <meta property="og:type" content="website">
  <meta property="og:url" content="https://docktail.org/docs/">
  <meta property="og:image" content="https://docktail.org/assets/og-image.jpeg">
  <meta name="twitter:card" content="summary_large_image">
  <meta name="twitter:title" content="Docs — DockTail">
  <meta name="twitter:description" content="{{ .Description }}">
  <meta name="twitter:image" content="https://docktail.org/assets/og-image.jpeg">
  <meta name="author" content="marvinvr">
  <meta name="keywords" content="docker, tailscale, containers, service mesh, tailnet, networking, devops, documentation">
  <link rel="canonical" href="https://docktail.org/docs/">
  <link rel="sitemap" type="application/xml" href="/sitemap.xml">
  <link rel="alternate" type="text/markdown" href="/docs.md" title="DockTail documentation in Markdown">
  <link rel="alternate" type="text/plain" href="/llms.txt" title="DockTail guide for agents">
  <link rel="alternate" type="text/plain" href="/llms-full.txt" title="DockTail full documentation for agents">
  <link rel="icon" href="/assets/favicon.ico" sizes="any">
  <link rel="icon" type="image/png" href="/assets/favicon-light-32.png" sizes="32x32" media="(prefers-color-scheme: light)">
  <link rel="icon" type="image/png" href="/assets/favicon-dark-32.png" sizes="32x32" media="(prefers-color-scheme: dark)">
  <link rel="apple-touch-icon" href="/assets/apple-touch-icon.png" sizes="180x180">
  <link rel="apple-touch-icon" href="/assets/apple-touch-icon-light.png" sizes="180x180" media="(prefers-color-scheme: light)">
  <link rel="apple-touch-icon" href="/assets/apple-touch-icon-dark.png" sizes="180x180" media="(prefers-color-scheme: dark)">
  <script type="application/ld+json">
    {
      "@context": "https://schema.org",
      "@graph": [
        {
          "@type": "TechArticle",
          "headline": "{{ .Title }}",
          "description": "{{ .Description }}",
          "url": "https://docktail.org/docs/",
          "image": "https://docktail.org/assets/og-image.jpeg",
          "dateModified": "{{ .GeneratedAt }}",
          "author": {
            "@type": "Person",
            "name": "marvinvr",
            "url": "https://marvinvr.ch"
          },
          "about": ["Docker", "Tailscale", "Tailscale Services", "Container networking"],
          "isPartOf": {
            "@type": "WebSite",
            "name": "DockTail",
            "url": "https://docktail.org/"
          },
          "mainEntityOfPage": "https://docktail.org/docs/"
        },
        {
          "@type": "BreadcrumbList",
          "itemListElement": [
            {
              "@type": "ListItem",
              "position": 1,
              "name": "DockTail",
              "item": "https://docktail.org/"
            },
            {
              "@type": "ListItem",
              "position": 2,
              "name": "Docs",
              "item": "https://docktail.org/docs/"
            }
          ]
        }
      ]
    }
  </script>
  <script
    src="https://stats.marvinvr.ch/api/script.js"
    data-site-id="ef0b4c42a7c2"
    defer
  ></script>
  <script src="https://cdn.tailwindcss.com"></script>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/prismjs@1.29.0/themes/prism-tomorrow.min.css">
  <style>
    html { scroll-behavior: smooth; scroll-padding-top: 5rem; }
    .sidebar-link.active { color: #111827; background-color: #f3f4f6; }

    /* Markdown body styling — emulates the hand-crafted Tailwind layout */
    .doc h1 { font-size: 1.5rem; font-weight: 700; color: #111827; margin-bottom: 1rem; }
    .doc > h2 {
      font-size: 1.5rem; font-weight: 700; color: #111827;
      margin-top: 4rem; margin-bottom: 1rem;
    }
    .doc > h2:first-child { margin-top: 0; }
    .doc > h3 {
      font-size: 1.125rem; font-weight: 600; color: #111827;
      margin-top: 2rem; margin-bottom: 0.75rem;
    }
    .doc p { font-size: 0.875rem; color: #6b7280; margin-bottom: 1rem; line-height: 1.65; }
    .doc strong { color: #374151; font-weight: 600; }
    .doc a { color: #111827; text-decoration: underline; text-underline-offset: 2px; }
    .doc a:hover { color: #374151; }
    .doc ul, .doc ol { font-size: 0.875rem; color: #6b7280; margin-bottom: 1rem; padding-left: 1.25rem; }
    .doc ul { list-style: disc; }
    .doc ol { list-style: decimal; }
    .doc li + li { margin-top: 0.4rem; }
    .doc li > p { margin-bottom: 0.4rem; }

    .doc :not(pre) > code {
      background-color: #f3f4f6;
      padding: 0.1rem 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.75rem;
      color: #374151;
    }

    /* Code blocks: dark with copy button */
    .doc .code-block { position: relative; margin-bottom: 1rem; }
    .doc .code-block .copy-btn {
      position: absolute;
      top: 0.75rem;
      right: 0.75rem;
      font-size: 0.75rem;
      color: #6b7280;
      opacity: 0;
      transition: opacity 0.15s, color 0.15s;
      cursor: pointer;
      background: transparent;
      border: 0;
      padding: 0;
      font-family: inherit;
      z-index: 2;
    }
    .doc .code-block:hover .copy-btn { opacity: 1; }
    .doc .code-block .copy-btn:hover { color: #d1d5db; }
    .doc pre[class*="language-"] {
      background: #111827 !important;
      color: #f3f4f6;
      border-radius: 0.375rem;
      padding: 1rem;
      font-size: 0.875rem;
      overflow-x: auto;
      margin: 0;
      text-shadow: none;
    }
    .doc pre code { font-size: 0.875rem; line-height: 1.6; text-shadow: none; }

    /* Prism token colors tuned to match the prior hand-tuned palette */
    .doc .token.comment,
    .doc .token.prolog,
    .doc .token.doctype,
    .doc .token.cdata { color: #6b7280; font-style: normal; }
    .doc .token.punctuation { color: #d1d5db; }
    .doc .token.property,
    .doc .token.tag,
    .doc .token.key,
    .doc .token.atrule,
    .doc .token.attr-name,
    .doc .token.selector { color: #60a5fa; }
    .doc .token.string,
    .doc .token.attr-value,
    .doc .token.char,
    .doc .token.regex { color: #fbbf24; }
    .doc .token.boolean,
    .doc .token.number,
    .doc .token.constant,
    .doc .token.symbol,
    .doc .token.deleted { color: #4ade80; }
    .doc .token.scalar { color: #4ade80; }
    .doc .token.operator,
    .doc .token.entity,
    .doc .token.url { color: #d1d5db; }
    .doc .token.keyword { color: #c084fc; }
    .doc .token.function { color: #f472b6; }

    /* Tables */
    .doc table {
      width: 100%;
      font-size: 0.875rem;
      border: 1px solid #e5e7eb;
      border-radius: 0.375rem;
      overflow: hidden;
      margin-bottom: 1rem;
      display: block;
      overflow-x: auto;
      white-space: nowrap;
    }
    .doc thead { background-color: #f3f4f6; color: #111827; }
    .doc thead th { padding: 0.625rem 1rem; font-weight: 600; text-align: left; }
    .doc tbody { background-color: #ffffff; color: #4b5563; }
    .doc tbody tr { border-top: 1px solid #e5e7eb; }
    .doc tbody td { padding: 0.625rem 1rem; vertical-align: top; }
    .doc tbody td:last-child { white-space: normal; min-width: 18rem; }

    .doc .comparison-table-wrap {
      overflow-x: auto;
      border: 1px solid #e5e7eb;
      border-radius: 0.5rem;
      margin-bottom: 1rem;
      background: #ffffff;
    }
    .doc .comparison-table {
      display: table;
      table-layout: fixed;
      min-width: 42.5rem;
      width: 100%;
      border: 0;
      border-radius: 0;
      margin-bottom: 0;
      overflow: visible;
      white-space: normal;
    }
    .doc .comparison-table thead { background-color: #f9fafb; }
    .doc .comparison-table th,
    .doc .comparison-table td {
      padding: 0.625rem 0.875rem;
      text-align: center;
      vertical-align: middle;
      white-space: nowrap;
    }
    .doc .comparison-table th:first-child,
    .doc .comparison-table td:first-child {
      text-align: left;
      white-space: normal;
      width: 14.375rem;
    }
    .doc .comparison-table col:first-child { width: 14.375rem; }
    .doc .comparison-table col:not(:first-child) { width: 5.625rem; }
    .doc .comparison-table tbody th {
      color: #374151;
      font-weight: 600;
    }
    .doc .comparison-table tbody td {
      font-size: 1rem;
    }
    .doc .comparison-table tbody td:last-child {
      min-width: 0;
      white-space: nowrap;
    }
    .doc .comparison-table tbody td {
      position: relative;
      text-align: center;
    }
    .doc .comparison-table .status-mark {
      display: inline-block;
      min-width: 1.5rem;
      text-align: center;
    }
    .doc .info-tip {
      position: relative;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 1rem;
      height: 1rem;
      margin-left: 0.25rem;
      border: 1px solid #d1d5db;
      border-radius: 9999px;
      color: #6b7280;
      font-size: 0.625rem;
      font-weight: 700;
      line-height: 1;
      cursor: help;
    }
    .doc .comparison-table tbody td .info-tip {
      position: absolute;
      left: calc(50% + 1.35rem);
      top: 50%;
      transform: translateY(-50%);
    }
    .doc .info-tip:focus {
      outline: 2px solid #9ca3af;
      outline-offset: 2px;
    }
    .doc .info-tip > span {
      display: none;
    }
    .tooltip-portal {
      position: fixed;
      z-index: 100;
      width: min(16rem, calc(100vw - 2rem));
      border: 1px solid #e5e7eb;
      border-radius: 0.375rem;
      background: #111827;
      color: #f9fafb;
      padding: 0.625rem 0.75rem;
      font-size: 0.75rem;
      font-weight: 400;
      line-height: 1.45;
      text-align: left;
      white-space: normal;
      opacity: 0;
      pointer-events: none;
      transition: opacity 0.15s ease-out;
      box-shadow: 0 10px 24px rgba(17, 24, 39, 0.16);
    }
    .tooltip-portal.is-visible {
      opacity: 1;
    }

    /* Blockquote rendered as a callout note */
    .doc blockquote {
      background: #ffffff;
      border: 1px solid #e5e7eb;
      border-radius: 0.375rem;
      padding: 1rem;
      font-size: 0.875rem;
      color: #6b7280;
      margin-bottom: 1rem;
    }
    .doc blockquote p { margin: 0; }
    .doc blockquote p + p { margin-top: 0.5rem; }
  </style>
</head>
<body class="bg-gray-50 text-gray-900 font-mono min-h-screen">

  <!-- Nav -->
  <nav class="sticky top-0 z-50 bg-white/95 backdrop-blur border-b border-gray-200">
    <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 h-14 flex items-center justify-between">
      <div class="flex items-center gap-3 sm:gap-5">
        <button id="sidebar-toggle" class="lg:hidden text-gray-500 hover:text-gray-900 transition-colors" aria-label="Open documentation navigation">
          <svg class="w-5 h-5" aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 6h16M4 12h16M4 18h16"/></svg>
        </button>
        <a href="/" class="inline-flex items-center gap-2 text-lg font-bold tracking-tight text-gray-900">
          <img src="/assets/docktail-logo-light.webp" width="28" height="28" alt="" class="h-7 w-7 object-contain">
          <span>docktail</span>
        </a>
        <span class="text-gray-300">/</span>
        <span class="text-sm text-gray-500">docs</span>
      </div>
      <div class="flex items-center gap-4 sm:gap-6">
        <a href="/docs/" aria-current="page" class="hidden sm:inline text-sm text-gray-900 transition-colors">docs</a>
        <a href="/cloud/?utm_source=docktail_org&amp;utm_medium=docs_nav" class="hidden sm:inline text-sm text-gray-500 hover:text-gray-900 transition-colors">cloud</a>
        <a href="https://github.com/marvinvr/docktail" class="hidden sm:inline text-sm text-gray-500 hover:text-gray-900 transition-colors">github</a>
        <a href="https://cloud.docktail.org/login?utm_source=docktail_org&amp;utm_medium=docs_nav"
           class="inline-flex items-center px-3 py-1.5 text-sm bg-gray-900 text-white rounded hover:bg-gray-800 transition-colors">
          Open DockTail Cloud
        </a>
      </div>
    </div>
  </nav>

  <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
    <div class="flex gap-8">

      <!-- Sidebar -->
      <aside id="sidebar" class="hidden lg:block w-64 flex-shrink-0">
        <div class="sticky top-14 overflow-y-auto h-[calc(100vh-3.5rem)] py-8 pr-4">
          <nav class="space-y-1 text-sm" aria-label="Documentation navigation">
            {{ range $i, $s := .Sections }}
              <div{{ if gt $i 0 }} class="pt-3"{{ end }}>
                <a href="#{{ $s.ID }}" class="sidebar-link block px-3 py-1.5 rounded text-gray-500 hover:text-gray-900 transition-colors font-semibold">{{ $s.Text }}</a>
                {{ range $s.Children }}
                  <a href="#{{ .ID }}" class="sidebar-link block px-3 py-1.5 pl-6 rounded text-gray-400 hover:text-gray-900 transition-colors">{{ .Text }}</a>
                {{ end }}
              </div>
            {{ end }}
            <div class="pt-4 mt-4 border-t border-gray-200">
              <a href="/docs.md" class="block px-3 py-1.5 rounded text-gray-400 hover:text-gray-900 transition-colors">markdown</a>
              <a href="/llms.txt" class="block px-3 py-1.5 rounded text-gray-400 hover:text-gray-900 transition-colors">agents</a>
            </div>
          </nav>
        </div>
      </aside>

      <!-- Content -->
      <main class="doc flex-1 min-w-0 py-8 lg:py-12 max-w-3xl">
        {{ .Body }}

        <!-- Footer -->
        <div class="border-t border-gray-200 pt-8 pb-16 mt-16">
          <div class="flex flex-col sm:flex-row items-center justify-between gap-4">
            <a href="/" class="text-sm text-gray-400 hover:text-gray-600 transition-colors">docktail</a>
            <div class="flex flex-wrap items-center justify-center gap-x-6 gap-y-3">
              <a href="https://github.com/marvinvr/docktail" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">GitHub</a>
              <a href="/docs/" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">Docs</a>
              <a href="/cloud/?utm_source=docktail_org&amp;utm_medium=docs_footer" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">Cloud</a>
              <a href="/docs.md" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">Markdown</a>
              <a href="/llms.txt" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">Agents</a>
              <a href="https://tailscale.com/kb/1552/tailscale-services" class="text-xs text-gray-400 hover:text-gray-600 transition-colors">Tailscale Services</a>
            </div>
          </div>
          <div class="mt-6 flex flex-col items-center gap-3">
            <span class="text-xs text-gray-400">AGPL v3 · built by <a href="https://marvinvr.ch" class="text-gray-500 hover:text-gray-700 transition-colors">marvinvr</a> · generated {{ .GeneratedAt }}</span>
            <a
              href="https://github.com/sponsors/marvinvr?o=esb"
              class="inline-flex items-center gap-2 rounded border border-gray-200 bg-white px-3 py-1.5 text-xs text-gray-500 transition-colors hover:border-gray-300 hover:text-gray-700 hover:bg-gray-50"
            >
              <svg class="h-3.5 w-3.5 text-rose-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">
                <path stroke-linecap="round" stroke-linejoin="round" d="m21 8.25c0-2.485-2.239-4.5-5-4.5-1.74 0-3.273.88-4.165 2.217A4.98 4.98 0 0 0 7.67 3.75c-2.761 0-5 2.015-5 4.5 0 7.22 9.165 12 9.165 12S21 15.47 21 8.25Z"></path>
              </svg>
              <span>Sponsor</span>
            </a>
          </div>
        </div>
      </main>
    </div>
  </div>

  <!-- Mobile sidebar overlay -->
  <div id="sidebar-overlay" class="hidden fixed inset-0 z-40 bg-black/20 lg:hidden"></div>
  <div id="sidebar-mobile" class="hidden fixed top-14 left-0 z-40 w-64 bg-white border-r border-gray-200 h-[calc(100vh-3.5rem)] overflow-y-auto py-6 px-2 lg:hidden"></div>
  <div id="tooltip-portal" class="tooltip-portal" role="tooltip" hidden></div>

  <script src="https://cdn.jsdelivr.net/npm/prismjs@1.29.0/components/prism-core.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/prismjs@1.29.0/plugins/autoloader/prism-autoloader.min.js"></script>
  <script>
    const tooltip = document.getElementById('tooltip-portal');

    function positionTooltip(trigger) {
      if (!tooltip) return;

      const margin = 8;
      const triggerRect = trigger.getBoundingClientRect();
      tooltip.hidden = false;
      tooltip.classList.add('is-visible');

      const tooltipRect = tooltip.getBoundingClientRect();
      let left = triggerRect.left + (triggerRect.width / 2) - (tooltipRect.width / 2);
      left = Math.max(margin, Math.min(left, window.innerWidth - tooltipRect.width - margin));

      let top = triggerRect.top - tooltipRect.height - margin;
      if (top < margin) top = triggerRect.bottom + margin;

      tooltip.style.left = left + 'px';
      tooltip.style.top = top + 'px';
    }

    function showTooltip(event) {
      if (!tooltip) return;

      const trigger = event.currentTarget;
      const text = trigger.getAttribute('data-tooltip') || trigger.getAttribute('aria-label');
      if (!text) return;

      tooltip.textContent = text;
      positionTooltip(trigger);
    }

    function hideTooltip() {
      if (!tooltip) return;

      tooltip.classList.remove('is-visible');
      tooltip.hidden = true;
    }

    document.querySelectorAll('.info-tip').forEach(trigger => {
      trigger.addEventListener('mouseenter', showTooltip);
      trigger.addEventListener('focus', showTooltip);
      trigger.addEventListener('mouseleave', hideTooltip);
      trigger.addEventListener('blur', hideTooltip);
    });

    window.addEventListener('scroll', hideTooltip, { passive: true });
    window.addEventListener('resize', hideTooltip);

    // Wrap each <pre> in a .code-block container with a copy button.
    document.querySelectorAll('.doc pre').forEach(pre => {
      const wrapper = document.createElement('div');
      wrapper.className = 'code-block';
      pre.parentNode.insertBefore(wrapper, pre);
      wrapper.appendChild(pre);

      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'copy-btn';
      btn.textContent = 'copy';
      btn.addEventListener('click', () => {
        const code = pre.querySelector('code') || pre;
        navigator.clipboard.writeText(code.textContent).then(() => {
          btn.textContent = 'copied!';
          setTimeout(() => btn.textContent = 'copy', 2000);
        });
      });
      wrapper.appendChild(btn);
    });

    // Scroll spy
    const links = [...document.querySelectorAll('.sidebar-link')];
    const sections = links
      .map(l => ({ link: l, el: document.getElementById(l.hash.slice(1)) }))
      .filter(s => s.el);

    function updateActive() {
      let current = sections[0]?.link;
      for (const s of sections) {
        if (s.el.getBoundingClientRect().top <= 100) current = s.link;
      }
      links.forEach(l => l.classList.toggle('active', l === current));
    }
    window.addEventListener('scroll', updateActive, { passive: true });
    updateActive();

    // Mobile sidebar
    const toggle = document.getElementById('sidebar-toggle');
    const overlay = document.getElementById('sidebar-overlay');
    const mobileSidebar = document.getElementById('sidebar-mobile');
    const desktopNav = document.querySelector('#sidebar nav');

    if (desktopNav && mobileSidebar) {
      mobileSidebar.appendChild(desktopNav.cloneNode(true));
      mobileSidebar.querySelectorAll('.sidebar-link').forEach(link => {
        link.addEventListener('click', closeSidebar);
      });
    }

    function closeSidebar() {
      overlay.classList.add('hidden');
      mobileSidebar.classList.add('hidden');
    }
    toggle?.addEventListener('click', () => {
      overlay.classList.toggle('hidden');
      mobileSidebar.classList.toggle('hidden');
    });
    overlay?.addEventListener('click', closeSidebar);
  </script>
</body>
</html>
`))
