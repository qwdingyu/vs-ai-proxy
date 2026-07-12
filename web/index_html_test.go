package web

import (
	"io/fs"
	"regexp"
	"strings"
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

func TestLogsFilterInputsAreWiredToQueryPayload(t *testing.T) {
	data, err := fs.ReadFile(MustSubFS(), "index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)
	filters := map[string]string{
		"q":            "logFilterSearch",
		"provider":     "logFilterProvider",
		"model":        "logFilterModel",
		"error_code":   "logFilterErrorCode",
		"error_reason": "logFilterErrorReason",
		"request_id":   "logFilterRequestID",
		"status_code":  "logFilterStatusCode",
	}
	for queryName, id := range filters {
		t.Run(queryName, func(t *testing.T) {
			if !strings.Contains(html, `id="`+id+`"`) {
				t.Fatalf("missing filter input %s", id)
			}
			if !strings.Contains(html, queryName+": $('"+id+"').value") {
				t.Fatalf("filter %s is not read into query payload", queryName)
			}
			if !strings.Contains(html, "$('"+id+"').value = filters."+queryName+" || ''") {
				t.Fatalf("filter %s is not reset from filter state", queryName)
			}
		})
	}
}
