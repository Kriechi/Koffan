package i18n

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var placeholder = regexp.MustCompile(`\{\{[^}]*\}\}`)

func flatten(prefix string, value interface{}, out map[string]string) {
	if m, ok := value.(map[string]interface{}); ok {
		for k, v := range m {
			flatten(prefix+k+".", v, out)
		}
		return
	}
	s, _ := value.(string)
	out[strings.TrimSuffix(prefix, ".")] = s
}

func loadFlat(t *testing.T, file string) map[string]string {
	t.Helper()
	data, err := localesFS.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%s is not valid JSON: %v", file, err)
	}
	out := map[string]string{}
	flatten("", raw, out)
	return out
}

func placeholders(s string) string {
	found := placeholder.FindAllString(s, -1)
	sort.Strings(found)
	return strings.Join(found, " ")
}

// Every language must offer exactly the keys English has, with the same
// {{placeholders}}: a missing key would show up as a raw key in the UI.
func TestEveryLocaleHasTheSameKeysAsEnglish(t *testing.T) {
	english := loadFlat(t, "en.json")
	files, err := localesFS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range files {
		name := entry.Name()
		if name == "en.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		checked++
		other := loadFlat(t, name)
		for key, want := range english {
			got, ok := other[key]
			switch {
			case !ok:
				t.Errorf("%s: missing key %q", name, key)
			case got == "" && want != "":
				t.Errorf("%s: empty value for %q", name, key)
			case placeholders(got) != placeholders(want):
				t.Errorf("%s: %q has placeholders [%s], English has [%s]", name, key, placeholders(got), placeholders(want))
			}
		}
		for key := range other {
			if _, ok := english[key]; !ok {
				t.Errorf("%s: key %q does not exist in en.json", name, key)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no other locales found")
	}
}
