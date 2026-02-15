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
	"sort"
	"strings"

	"golang.org/x/pkgsite/internal/fetch"
	"golang.org/x/pkgsite/internal/frontend"
	"golang.org/x/pkgsite/internal/log"
	"golang.org/x/pkgsite/static"
	thirdparty "golang.org/x/pkgsite/third_party"
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

	// Copy static assets.
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
// it injects a strict Content-Security-Policy meta tag.
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

	// Inject CSP meta tag into HTML responses.
	contentType := w.Header().Get("Content-Type")
	if strings.Contains(contentType, "text/html") || contentType == "" {
		body = injectCSP(body)
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

// copyEmbeddedFS recursively copies all files from an embedded filesystem
// to a destination directory on disk.
func copyEmbeddedFS(fsys fs.FS, root, destDir string) error {
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
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}
