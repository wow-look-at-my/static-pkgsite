// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgsite

import (
	"html"
	"strings"
	"testing"
)

func TestRelativePrefix(t *testing.T) {
	tests := []struct {
		urlPath string
		want    string
	}{
		{"/", "./"},
		{"/about", "../"},
		{"/net/http", "../../"},
		{"/net/http/httptest", "../../../"},
		{"/search-help", "../"},
	}
	for _, tt := range tests {
		got := relativePrefix(tt.urlPath)
		if got != tt.want {
			t.Errorf("relativePrefix(%q) = %q, want %q", tt.urlPath, got, tt.want)
		}
	}
}

func TestIsURLAttr(t *testing.T) {
	tests := []struct {
		attr string
		want bool
	}{
		{"href", true},
		{"src", true},
		{"action", true},
		{"poster", true},
		{"data", true},
		{"class", false},
		{"id", false},
		{"style", false},
		{"value", false},
	}
	for _, tt := range tests {
		got := isURLAttr(tt.attr)
		if got != tt.want {
			t.Errorf("isURLAttr(%q) = %v, want %v", tt.attr, got, tt.want)
		}
	}
}

func TestRelativizeScriptText(t *testing.T) {
	tests := []struct {
		name   string
		script string
		prefix string
		want   string
	}{
		{
			name:   "double-quoted static path",
			script: `loadScript("/static/frontend/frontend.js")`,
			prefix: "../",
			want:   `loadScript("../static/frontend/frontend.js")`,
		},
		{
			name:   "single-quoted static path",
			script: `loadScript('/static/frontend/frontend.js')`,
			prefix: "../",
			want:   `loadScript('../static/frontend/frontend.js')`,
		},
		{
			name:   "third_party path",
			script: `loadScript("/third_party/dialog-polyfill/dialog-polyfill.js")`,
			prefix: "../../",
			want:   `loadScript("../../third_party/dialog-polyfill/dialog-polyfill.js")`,
		},
		{
			name:   "root prefix",
			script: `loadScript("/static/frontend/frontend.js")`,
			prefix: "./",
			want:   `loadScript("./static/frontend/frontend.js")`,
		},
		{
			name:   "no matching paths",
			script: `console.log("hello world")`,
			prefix: "../",
			want:   `console.log("hello world")`,
		},
		{
			name:   "multiple paths in one script",
			script: `loadScript("/static/a.js"); loadScript("/third_party/b.js")`,
			prefix: "../",
			want:   `loadScript("../static/a.js"); loadScript("../third_party/b.js")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativizeScriptText(tt.script, tt.prefix)
			if got != tt.want {
				t.Errorf("relativizeScriptText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAbsoluteToRelativeAsset(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		filePath string
		want     string
	}{
		{
			name:     "CSS url() at depth 3",
			content:  `background: url(/static/shared/icon/search.svg)`,
			filePath: "static/frontend/homepage/homepage.css",
			want:     `background: url(../../../static/shared/icon/search.svg)`,
		},
		{
			name:     "double-quoted path in JS",
			content:  `import "/static/frontend/frontend.js"`,
			filePath: "static/frontend/unit/main/main.js",
			want:     `import "../../../../static/frontend/frontend.js"`,
		},
		{
			name:     "single-quoted path at depth 2",
			content:  `@import '/static/shared/shared.css'`,
			filePath: "static/frontend/frontend.css",
			want:     `@import '../../static/shared/shared.css'`,
		},
		{
			name:     "third_party reference at depth 2",
			content:  `url(/third_party/fonts/font.woff2)`,
			filePath: "static/frontend/frontend.css",
			want:     `url(../../third_party/fonts/font.woff2)`,
		},
		{
			name:     "file at root level",
			content:  `url(/static/foo.png)`,
			filePath: "style.css",
			want:     `url(static/foo.png)`,
		},
		{
			name:     "no matching paths",
			content:  `.foo { color: red; }`,
			filePath: "static/frontend/frontend.css",
			want:     `.foo { color: red; }`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(absoluteToRelativeAsset([]byte(tt.content), tt.filePath))
			if got != tt.want {
				t.Errorf("absoluteToRelativeAsset() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestURLPathToFilePath(t *testing.T) {
	tests := []struct {
		urlPath string
		outDir  string
		want    string
	}{
		{"/", "out", "out/index.html"},
		{"/about", "out", "out/about/index.html"},
		{"/net/http", "out", "out/net/http/index.html"},
		{"/favicon.ico", "out", "out/favicon.ico"},
		{"/static/frontend/frontend.css", "out", "out/static/frontend/frontend.css"},
	}
	for _, tt := range tests {
		got := urlPathToFilePath(tt.urlPath, tt.outDir)
		if got != tt.want {
			t.Errorf("urlPathToFilePath(%q, %q) = %q, want %q", tt.urlPath, tt.outDir, got, tt.want)
		}
	}
}

func TestProcessHTML(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		urlPath string
		checks  []func(t *testing.T, result string)
	}{
		{
			name:    "rewrites href attributes",
			html:    `<html><head></head><body><a href="/about">About</a></body></html>`,
			urlPath: "/",
			checks: []func(t *testing.T, result string){
				contains(`href="./about"`),
			},
		},
		{
			name:    "rewrites src attributes",
			html:    `<html><head></head><body><img src="/static/img/logo.png"></body></html>`,
			urlPath: "/about",
			checks: []func(t *testing.T, result string){
				contains(`src="../static/img/logo.png"`),
			},
		},
		{
			name:    "rewrites link href in head",
			html:    `<html><head><link rel="stylesheet" href="/static/frontend/frontend.css"></head><body></body></html>`,
			urlPath: "/net/http",
			checks: []func(t *testing.T, result string){
				contains(`href="../../static/frontend/frontend.css"`),
			},
		},
		{
			name:    "does not rewrite protocol-relative URLs",
			html:    `<html><head></head><body><a href="//example.com">Link</a></body></html>`,
			urlPath: "/",
			checks: []func(t *testing.T, result string){
				contains(`href="//example.com"`),
			},
		},
		{
			name:    "does not rewrite fragment-only hrefs",
			html:    `<html><head></head><body><a href="#section">Link</a></body></html>`,
			urlPath: "/",
			checks: []func(t *testing.T, result string){
				contains(`href="#section"`),
			},
		},
		{
			name:    "injects CSP meta tag in head",
			html:    `<html><head><title>Test</title></head><body></body></html>`,
			urlPath: "/",
			checks: []func(t *testing.T, result string){
				contains(`http-equiv="Content-Security-Policy"`),
				// html.Render escapes single quotes as &#39; in attribute values.
				contains(`content="` + html.EscapeString(cspContent) + `"`),
			},
		},
		{
			name:    "rewrites inline script paths",
			html:    `<html><head></head><body><script>loadScript("/static/frontend/frontend.js")</script></body></html>`,
			urlPath: "/net/http",
			checks: []func(t *testing.T, result string){
				contains(`loadScript("../../static/frontend/frontend.js")`),
			},
		},
		{
			name:    "deep path gets correct prefix",
			html:    `<html><head><link href="/static/style.css"></head><body><a href="/about">About</a></body></html>`,
			urlPath: "/github.com/user/repo/pkg",
			checks: []func(t *testing.T, result string){
				contains(`href="../../../../static/style.css"`),
				contains(`href="../../../../about"`),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := processHTML([]byte(tt.html), tt.urlPath)
			if err != nil {
				t.Fatalf("processHTML() error: %v", err)
			}
			resultStr := string(result)
			for _, check := range tt.checks {
				check(t, resultStr)
			}
		})
	}
}

// contains returns a check function that verifies the result contains the substring.
func contains(substr string) func(t *testing.T, result string) {
	return func(t *testing.T, result string) {
		t.Helper()
		if !strings.Contains(result, substr) {
			t.Errorf("result does not contain %q\nresult: %s", substr, result)
		}
	}
}
