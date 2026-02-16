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
	"path"
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
// All absolute URL references in the generated HTML, CSS, and JS are converted
// to relative paths so the site works when served from any directory, including
// GitHub Pages project subpaths.
func GenerateStaticSite(ctx context.Context, serverCfg ServerConfig, outDir string) error {
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
	if err := renderAndWrite(mux, "/", outDir); err != nil {
		return fmt.Errorf("rendering homepage: %w", err)
	}

	// Render static informational pages.
	for _, p := range staticPages {
		progress(p)
		if err := renderAndWrite(mux, p, outDir); err != nil {
			log.Errorf(ctx, "rendering %s: %v", p, err)
		}
	}

	// Render each unit (package/module/directory) page.
	for _, p := range paths {
		urlPath := "/" + p
		progress(urlPath)
		if err := renderAndWrite(mux, urlPath, outDir); err != nil {
			log.Errorf(ctx, "rendering %s: %v", urlPath, err)
		}
	}

	// Copy static assets, converting absolute paths to relative in CSS/JS.
	fmt.Fprintf(os.Stderr, "Copying static assets...\n")
	if err := copyEmbeddedFS(static.FS, ".", filepath.Join(outDir, "static")); err != nil {
		return fmt.Errorf("copying static assets: %w", err)
	}
	if err := copyEmbeddedFS(thirdparty.FS, ".", filepath.Join(outDir, "third_party")); err != nil {
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
// it injects a strict Content-Security-Policy meta tag and converts absolute
// URL paths to relative paths.
func renderAndWrite(mux *http.ServeMux, urlPath, outDir string) error {
	return renderAndWriteN(mux, urlPath, outDir, 0)
}

func renderAndWriteN(mux *http.ServeMux, urlPath, outDir string, depth int) error {
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
			return renderAndWriteN(mux, loc, outDir, depth+1)
		}
	}

	if w.Code != http.StatusOK {
		return fmt.Errorf("GET %s returned status %d", urlPath, w.Code)
	}

	body := w.Body.Bytes()

	// Inject CSP meta tag and relativize paths in HTML responses.
	contentType := w.Header().Get("Content-Type")
	if strings.Contains(contentType, "text/html") || contentType == "" {
		body = injectCSP(body)
		body = absoluteToRelativeHTML(body, urlPath)
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

// relativePrefix returns the "../" prefix needed to navigate from a page at
// urlPath back to the site root. Pages are written as directory/index.html,
// so /about becomes /about/index.html (depth 1), /net/http becomes
// /net/http/index.html (depth 2), etc.
//
// Examples:
//
//	"/"           → "./"
//	"/about"      → "../"
//	"/net/http"   → "../../"
func relativePrefix(urlPath string) string {
	clean := strings.TrimPrefix(urlPath, "/")
	if clean == "" {
		return "./"
	}
	depth := strings.Count(clean, "/") + 1
	return strings.Repeat("../", depth)
}

// attrAbsPathRe matches href, src, or action attributes whose value is an
// absolute path. Group 1 captures the attribute and opening quote, group 2
// captures the path (without leading slash). Protocol-relative URLs (//...)
// are excluded by requiring [^/] after the slash.
var attrAbsPathRe = regexp.MustCompile(`((?:href|src|action)\s*=\s*["'])/([^/"'][^"']*)`)

// absoluteToRelativeHTML converts absolute URL paths in HTML content to
// relative paths based on the page's position in the URL hierarchy.
// This makes the generated site work when served from any directory.
func absoluteToRelativeHTML(content []byte, urlPath string) []byte {
	prefix := []byte(relativePrefix(urlPath))

	// Rewrite href="/about" → href="../about", src="/static/x" → src="../static/x", etc.
	content = attrAbsPathRe.ReplaceAll(content, append([]byte("$1"), append(prefix, []byte("$2")...)...))

	// Rewrite root path references: href="/" → href="../" (or "./" for the root page).
	for _, attr := range []string{"href", "src", "action"} {
		for _, q := range []string{`"`, `'`} {
			old := []byte(attr + "=" + q + "/" + q)
			repl := []byte(attr + "=" + q + string(prefix) + q)
			content = bytes.ReplaceAll(content, old, repl)
		}
	}

	// Rewrite non-attribute contexts: loadScript("/static/..."), loadScript("/third_party/...").
	for _, dir := range []string{"/static/", "/third_party/"} {
		for _, q := range []byte{'"', '\''} {
			old := append([]byte{q}, []byte(dir)...)
			repl := append([]byte{q}, append(prefix, []byte(dir[1:])...)...)
			content = bytes.ReplaceAll(content, old, repl)
		}
	}

	return content
}

// absoluteToRelativeAsset converts absolute URL path references in CSS and JS
// files to relative paths. The file's path within the output directory
// determines the depth.
//
// For example, static/frontend/homepage/homepage.css references
// /static/shared/icon/search.svg. Since the CSS file is 3 levels deep
// (static/frontend/homepage/), the result is ../../../static/shared/icon/search.svg.
func absoluteToRelativeAsset(content []byte, filePath string) []byte {
	// Compute depth: number of directory separators in the file's path
	// gives us how many "../" we need to reach the site root.
	dir := path.Dir(filePath)
	depth := 0
	if dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	prefix := []byte(strings.Repeat("../", depth))

	for _, dir := range []string{"/static/", "/third_party/"} {
		for _, q := range []byte{'"', '\''} {
			old := append([]byte{q}, []byte(dir)...)
			repl := append([]byte{q}, append(prefix, []byte(dir[1:])...)...)
			content = bytes.ReplaceAll(content, old, repl)
		}
		// CSS url() without quotes: url(/static/...)
		old := append([]byte("("), []byte(dir)...)
		repl := append([]byte("("), append(prefix, []byte(dir[1:])...)...)
		content = bytes.ReplaceAll(content, old, repl)
	}

	return content
}

// copyEmbeddedFS recursively copies all files from an embedded filesystem
// to a destination directory on disk. CSS and JS files have their absolute
// URL path references converted to relative paths.
func copyEmbeddedFS(fsys fs.FS, root, destDir string) error {
	return fs.WalkDir(fsys, root, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(destDir, filepath.FromSlash(fpath))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := fs.ReadFile(fsys, fpath)
		if err != nil {
			return err
		}
		ext := filepath.Ext(fpath)
		if ext == ".css" || ext == ".js" {
			// The file's path relative to the site root includes the
			// top-level directory name (e.g., "static/" or "third_party/").
			// We derive this from destDir's base name + the embedded path.
			siteRelPath := path.Join(filepath.Base(destDir), fpath)
			data = absoluteToRelativeAsset(data, siteRelPath)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}
