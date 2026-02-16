// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgsite

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wow-look-at-my/static-pkgsite/internal/fetch"
	"github.com/wow-look-at-my/static-pkgsite/internal/frontend"
	"github.com/wow-look-at-my/static-pkgsite/internal/log"
	"github.com/wow-look-at-my/static-pkgsite/static"
	thirdparty "github.com/wow-look-at-my/static-pkgsite/third_party"
)

// cspMeta is the Content-Security-Policy meta tag injected into every generated
// HTML page. It forbids all off-domain resource loading.
const cspMeta = `<meta http-equiv="Content-Security-Policy" content="` +
	`default-src 'self'; ` +
	`script-src 'self' 'unsafe-inline'; ` +
	`style-src 'self' 'unsafe-inline'; ` +
	`img-src 'self' data:; ` +
	`font-src 'self'; ` +
	`connect-src 'none'; ` +
	`frame-src 'none'; ` +
	`object-src 'none'; ` +
	`base-uri 'none'` +
	`">`

// GenerateStaticSite generates a fully static HTML/CSS/JS site into outDir
// using the same server infrastructure as the dynamic mode. The output can be
// served by any static file server with no Go backend required.
//
// basePath controls the URL path prefix for the generated site. When serving
// from the root of a domain, basePath should be "/". When serving from a
// subpath (e.g., GitHub Pages project sites), basePath should be "/<repo>/".
// All absolute URL references in the generated HTML, CSS, and JS are rewritten
// to include this prefix.
func GenerateStaticSite(ctx context.Context, serverCfg ServerConfig, outDir, basePath string) error {
	// Normalize base path to always have leading and trailing slashes.
	if basePath == "" {
		basePath = "/"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath = basePath + "/"
	}
	// Build the server and get the getters/modules for package enumeration.
	result, err := buildServerAndGetters(ctx, serverCfg)
	if err != nil {
		return fmt.Errorf("building server: %w", err)
	}

	// Install all routes on a ServeMux.
	mux := http.NewServeMux()
	result.Server.Install(mux.Handle, nil, nil)

	// Enumerate all package/directory paths from the loaded modules.
	paths, err := enumerateUnitPaths(ctx, result.Getters, result.AllModules)
	if err != nil {
		return fmt.Errorf("enumerating packages: %w", err)
	}

	// Count total pages for progress reporting.
	staticPages := []string{"/about", "/license-policy", "/search-help"}
	total := 1 + len(staticPages) + len(paths) // homepage + static pages + unit pages
	current := 0

	progress := func(urlPath string) {
		current++
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s\n", current, total, urlPath)
	}

	fmt.Fprintf(os.Stderr, "Generating %d pages...\n", total)

	// Render the homepage.
	progress("/")
	if err := renderAndWrite(mux, "/", outDir, basePath); err != nil {
		return fmt.Errorf("rendering homepage: %w", err)
	}

	// Render static informational pages.
	for _, p := range staticPages {
		progress(p)
		if err := renderAndWrite(mux, p, outDir, basePath); err != nil {
			log.Errorf(ctx, "rendering %s: %v", p, err)
		}
	}

	// Render each unit (package/module/directory) page.
	for _, p := range paths {
		urlPath := "/" + p
		progress(urlPath)
		if err := renderAndWrite(mux, urlPath, outDir, basePath); err != nil {
			log.Errorf(ctx, "rendering %s: %v", urlPath, err)
		}
	}

	// Copy static assets, rewriting absolute paths in CSS/JS files.
	fmt.Fprintf(os.Stderr, "Copying static assets...\n")
	rewriter := newBasePathRewriter(basePath)
	if err := copyEmbeddedFS(static.FS, ".", filepath.Join(outDir, "static"), rewriter); err != nil {
		return fmt.Errorf("copying static assets: %w", err)
	}
	if err := copyEmbeddedFS(thirdparty.FS, ".", filepath.Join(outDir, "third_party"), rewriter); err != nil {
		return fmt.Errorf("copying third_party assets: %w", err)
	}

	// Copy favicon to root.
	favicon, err := fs.ReadFile(static.FS, "shared/icon/favicon.ico")
	if err == nil {
		_ = os.WriteFile(filepath.Join(outDir, "favicon.ico"), favicon, 0o644)
	}

	fmt.Fprintf(os.Stderr, "Static site generated in %s\n", outDir)
	return nil
}

// enumerateUnitPaths discovers all package/directory paths from the given
// modules by fetching each module with the available getters and collecting
// their UnitMetas.
func enumerateUnitPaths(ctx context.Context, getters []fetch.ModuleGetter, modules []frontend.LocalModule) ([]string, error) {
	seen := make(map[string]bool)
	var paths []string

	for _, mod := range modules {
		for _, g := range getters {
			lm := fetch.FetchLazyModule(ctx, mod.ModulePath, fetch.LocalVersion, g)
			if lm.Error != nil {
				continue // this getter doesn't have this module, try next
			}
			for _, um := range lm.UnitMetas {
				if !seen[um.Path] {
					seen[um.Path] = true
					paths = append(paths, um.Path)
				}
			}
			break // found it with this getter, no need to try others
		}
	}

	sort.Strings(paths)
	return paths, nil
}

// renderAndWrite renders the given URL path using the mux and writes the
// response body to the appropriate file under outDir. For HTML responses,
// it injects a strict Content-Security-Policy meta tag and rewrites absolute
// URL paths to include the base path prefix.
func renderAndWrite(mux *http.ServeMux, urlPath, outDir, basePath string) error {
	return renderAndWriteN(mux, urlPath, outDir, basePath, 0)
}

func renderAndWriteN(mux *http.ServeMux, urlPath, outDir, basePath string, depth int) error {
	if depth > 5 {
		return fmt.Errorf("too many redirects for %s", urlPath)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", urlPath, nil)
	mux.ServeHTTP(w, r)

	// Follow redirects.
	if w.Code == http.StatusMovedPermanently || w.Code == http.StatusFound {
		loc := w.Header().Get("Location")
		if loc != "" {
			return renderAndWriteN(mux, loc, outDir, basePath, depth+1)
		}
	}

	if w.Code != http.StatusOK {
		return fmt.Errorf("GET %s returned status %d", urlPath, w.Code)
	}

	body := w.Body.Bytes()

	// Inject CSP meta tag and rewrite paths in HTML responses.
	contentType := w.Header().Get("Content-Type")
	if strings.Contains(contentType, "text/html") || contentType == "" {
		body = injectCSP(body)
		body = rewriteAbsolutePathsInHTML(body, basePath)
	}

	// Determine output file path.
	outPath := urlPathToFilePath(urlPath, outDir)

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, body, 0o644)
}

// urlPathToFilePath maps a URL path to a filesystem path under outDir.
// "/" becomes "outDir/index.html", "/foo/bar" becomes "outDir/foo/bar/index.html",
// and paths with file extensions (like "/favicon.ico") stay as-is.
func urlPathToFilePath(urlPath, outDir string) string {
	clean := strings.TrimPrefix(urlPath, "/")
	if clean == "" {
		return filepath.Join(outDir, "index.html")
	}
	// If the path has a file extension, keep it as-is.
	if ext := filepath.Ext(clean); ext != "" {
		return filepath.Join(outDir, filepath.FromSlash(clean))
	}
	// Otherwise, treat it as a directory with index.html.
	return filepath.Join(outDir, filepath.FromSlash(clean), "index.html")
}

// injectCSP inserts a Content-Security-Policy meta tag into the <head> of an
// HTML document. This ensures that even if the static site is served without
// server-side headers, no off-domain resources can be loaded.
func injectCSP(html []byte) []byte {
	// Insert after <head> (or <head ...>).
	idx := bytes.Index(html, []byte("<head>"))
	if idx >= 0 {
		insertion := idx + len("<head>")
		return bytes.Join([][]byte{
			html[:insertion],
			[]byte("\n    " + cspMeta),
			html[insertion:],
		}, nil)
	}
	// Try <head with attributes.
	idx = bytes.Index(html, []byte("<head "))
	if idx >= 0 {
		// Find the closing >.
		end := bytes.IndexByte(html[idx:], '>')
		if end >= 0 {
			insertion := idx + end + 1
			return bytes.Join([][]byte{
				html[:insertion],
				[]byte("\n    " + cspMeta),
				html[insertion:],
			}, nil)
		}
	}
	return html
}

// basePathRewriter rewrites absolute URL paths in file content to include
// a base path prefix. This is needed when serving the static site from a
// subpath (e.g., GitHub Pages project sites).
type basePathRewriter struct {
	basePath string
}

func newBasePathRewriter(basePath string) *basePathRewriter {
	if basePath == "/" {
		return nil // no rewriting needed
	}
	return &basePathRewriter{basePath: basePath}
}

// attrAbsPathRe matches href, src, or action attributes whose value starts
// with an absolute path (e.g., href="/about"). It captures the attribute
// prefix (group 1) and the first path character (group 2). Protocol-relative
// URLs like href="//..." are not matched because [^/"'] excludes "/".
var attrAbsPathRe = regexp.MustCompile(`((?:href|src|action)\s*=\s*["'])/([^/"'])`)

// rewriteAbsolutePathsInHTML rewrites absolute URL paths in HTML content
// to include the base path prefix. It handles href/src/action attributes,
// inline script URL strings, and other common patterns.
func rewriteAbsolutePathsInHTML(content []byte, basePath string) []byte {
	if basePath == "/" {
		return content
	}
	bp := []byte(basePath)

	// Step 1: Rewrite href/src/action attributes that point to absolute
	// paths (e.g., href="/about", src="/static/..."). This must run first
	// so that the simpler string replacements in step 2 don't double-match
	// already-rewritten attribute values.
	content = attrAbsPathRe.ReplaceAll(content, append([]byte("$1"), append(bp, []byte("$2")...)...))

	// Handle root path references: href="/" → href="/base-path/"
	for _, attr := range []string{"href", "src", "action"} {
		for _, q := range []string{`"`, `'`} {
			old := []byte(attr + "=" + q + "/" + q)
			repl := []byte(attr + "=" + q + basePath + q)
			content = bytes.ReplaceAll(content, old, repl)
		}
	}

	// Step 2: Replace well-known absolute path prefixes in non-attribute
	// contexts (e.g., loadScript("/static/...") in inline scripts). After
	// step 1 rewrites attribute values, these patterns only remain in
	// non-attribute positions, so there is no double-replacement risk.
	for _, prefix := range []string{"/static/", "/third_party/", "/favicon.ico"} {
		for _, q := range []byte{'"', '\''} {
			old := append([]byte{q}, []byte(prefix)...)
			repl := append([]byte{q}, append(bp, []byte(prefix[1:])...)...)
			content = bytes.ReplaceAll(content, old, repl)
		}
	}

	return content
}

// rewriteAbsolutePathsInAsset rewrites absolute URL path references in
// CSS and JS files. These files use url() in CSS and string literals in JS
// to reference other static assets.
func rewriteAbsolutePathsInAsset(content []byte, basePath string) []byte {
	if basePath == "/" {
		return content
	}
	bp := []byte(basePath)

	// Replace quoted references: "/static/..." and '/static/...'
	for _, prefix := range []string{"/static/", "/third_party/"} {
		for _, q := range []byte{'"', '\''} {
			old := append([]byte{q}, []byte(prefix)...)
			repl := append([]byte{q}, append(bp, []byte(prefix[1:])...)...)
			content = bytes.ReplaceAll(content, old, repl)
		}
		// Also handle CSS url() without quotes: url(/static/...)
		old := append([]byte("("), []byte(prefix)...)
		repl := append([]byte("("), append(bp, []byte(prefix[1:])...)...)
		content = bytes.ReplaceAll(content, old, repl)
	}

	return content
}

// copyEmbeddedFS recursively copies all files from an embedded filesystem
// to a destination directory on disk. If rewriter is non-nil, CSS and JS
// file contents are transformed to rewrite absolute URL paths.
func copyEmbeddedFS(fsys fs.FS, root, destDir string, rewriter *basePathRewriter) error {
	return fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(destDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		if rewriter != nil {
			ext := filepath.Ext(path)
			if ext == ".css" || ext == ".js" {
				data = rewriteAbsolutePathsInAsset(data, rewriter.basePath)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}
