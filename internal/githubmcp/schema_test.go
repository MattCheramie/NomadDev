package githubmcp

import (
	"reflect"
	"testing"
)

func TestConvertSchema_Empty(t *testing.T) {
	out, err := ConvertSchema(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Type != "object" {
		t.Fatalf("type = %q, want object", out.Type)
	}
}

func TestConvertSchema_BasicObject(t *testing.T) {
	in := []byte(`{
		"type": "object",
		"properties": {
			"owner": {"type": "string", "description": "repo owner"},
			"page":  {"type": "integer", "minimum": 1, "maximum": 100}
		},
		"required": ["owner"]
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Type != "object" {
		t.Fatalf("type = %q", out.Type)
	}
	if !reflect.DeepEqual(out.Required, []string{"owner"}) {
		t.Fatalf("required = %v", out.Required)
	}
	owner, ok := out.Properties["owner"]
	if !ok {
		t.Fatalf("missing owner property")
	}
	if owner.Type != "string" || owner.Description != "repo owner" {
		t.Fatalf("owner = %+v", owner)
	}
	page, ok := out.Properties["page"]
	if !ok {
		t.Fatalf("missing page property")
	}
	if page.Type != "integer" || page.Minimum == nil || *page.Minimum != 1 || page.Maximum == nil || *page.Maximum != 100 {
		t.Fatalf("page = %+v", page)
	}
}

func TestConvertSchema_StringEnum(t *testing.T) {
	in := []byte(`{
		"type": "string",
		"enum": ["open", "closed", "all"]
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !reflect.DeepEqual(out.Enum, []string{"open", "closed", "all"}) {
		t.Fatalf("enum = %v", out.Enum)
	}
}

func TestConvertSchema_NonStringEnum_Flattened(t *testing.T) {
	in := []byte(`{
		"type": "integer",
		"enum": [1, 2, 3]
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !reflect.DeepEqual(out.Enum, []string{"1", "2", "3"}) {
		t.Fatalf("enum = %v", out.Enum)
	}
}

func TestConvertSchema_ArrayOfStrings(t *testing.T) {
	in := []byte(`{
		"type": "array",
		"items": {"type": "string"}
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Type != "array" {
		t.Fatalf("type = %q", out.Type)
	}
	if out.Items == nil || out.Items.Type != "string" {
		t.Fatalf("items = %+v", out.Items)
	}
}

func TestConvertSchema_MissingTopLevelType_DefaultsToObject(t *testing.T) {
	// Some MCP tools emit schemas with properties but no top-level "type".
	in := []byte(`{
		"properties": {
			"x": {"type": "string"}
		}
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Type != "object" {
		t.Fatalf("type = %q, want object", out.Type)
	}
}

func TestConvertSchema_BadJSON_GracefulDegrade(t *testing.T) {
	out, err := ConvertSchema([]byte(`{not json`))
	if err == nil {
		t.Fatal("want err on bad json")
	}
	if out.Type != "object" {
		t.Fatalf("graceful fallback type = %q, want object", out.Type)
	}
	if out.Description == "" {
		t.Fatal("graceful fallback should carry a description")
	}
}

func TestConvertSchema_NestedObject(t *testing.T) {
	in := []byte(`{
		"type": "object",
		"properties": {
			"filter": {
				"type": "object",
				"properties": {
					"state": {"type": "string", "enum": ["open", "closed"]}
				}
			}
		}
	}`)
	out, err := ConvertSchema(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	filter := out.Properties["filter"]
	if filter == nil || filter.Type != "object" {
		t.Fatalf("filter = %+v", filter)
	}
	state := filter.Properties["state"]
	if state == nil || state.Type != "string" || !reflect.DeepEqual(state.Enum, []string{"open", "closed"}) {
		t.Fatalf("state = %+v", state)
	}
}

func TestPrefixedName_RoundTrip(t *testing.T) {
	cases := []struct {
		raw, prefixed string
	}{
		{"list_repositories", "github_list_repositories"},
		{"create_issue", "github_create_issue"},
	}
	for _, tc := range cases {
		got := PrefixedName(tc.raw)
		if got != tc.prefixed {
			t.Errorf("PrefixedName(%q) = %q, want %q", tc.raw, got, tc.prefixed)
		}
		if PrefixedName(got) != got {
			t.Errorf("PrefixedName not idempotent: %q", got)
		}
		if UnprefixedName(tc.prefixed) != tc.raw {
			t.Errorf("UnprefixedName(%q) = %q", tc.prefixed, UnprefixedName(tc.prefixed))
		}
	}
}

func TestIsGitHubTool(t *testing.T) {
	if !IsGitHubTool("github_create_pull_request") {
		t.Fatal("expected true")
	}
	if IsGitHubTool("execute_script") {
		t.Fatal("expected false for sandbox tool")
	}
}

func TestEnvTokenSource(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOK", "ghp_xxx")
	src := EnvTokenSource{Var: "TEST_GITHUB_TOK"}
	tok, err := src.Token(t.Context())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if tok != "ghp_xxx" {
		t.Fatalf("tok = %q", tok)
	}

	t.Setenv("TEST_GITHUB_TOK", "")
	if _, err := src.Token(t.Context()); err == nil {
		t.Fatal("want ErrNoToken when env unset")
	}
}

func TestStaticTokenSource(t *testing.T) {
	src := StaticTokenSource{Value: "tok"}
	got, err := src.Token(t.Context())
	if err != nil || got != "tok" {
		t.Fatalf("got=%q err=%v", got, err)
	}

	empty := StaticTokenSource{}
	if _, err := empty.Token(t.Context()); err == nil {
		t.Fatal("want ErrNoToken when empty")
	}
}

func TestStubNew_ReturnsNotBuilt(t *testing.T) {
	// Compiled without -tags github, so the stub New must surface ErrNotBuilt.
	_, err := New(t.Context(), Options{})
	if err == nil {
		t.Fatal("want ErrNotBuilt from stub New")
	}
}
