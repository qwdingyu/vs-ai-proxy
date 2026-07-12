package web

import (
	"io/fs"
	"regexp"
	"testing"
)

func TestIndexHTMLHasNoDuplicateIDsAndScriptReferencesExist(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	idRe := regexp.MustCompile(`\bid="([^"]+)"`)
	ids := map[string]int{}
	for _, match := range idRe.FindAllStringSubmatch(html, -1) {
		ids[match[1]]++
	}
	for id, count := range ids {
		if count > 1 {
			t.Fatalf("duplicate id %q appears %d times", id, count)
		}
	}

	refRe := regexp.MustCompile(`\$\('([^']+)'\)`)
	for _, match := range refRe.FindAllStringSubmatch(html, -1) {
		if ids[match[1]] == 0 {
			t.Fatalf("script references missing id %q", match[1])
		}
	}
}
