package proxycmd

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		sql  string
		want Command
	}{
		{"SHOW DATABASES", Command{Kind: ShowDatabases}},
		{"  show   databases ;\n", Command{Kind: ShowDatabases}},
		{"SHOW SCHEMAS", Command{Kind: ShowDatabases}},
		{"show databases like 'es\\_%'", Command{Kind: ShowDatabases, Pattern: `es\_%`, HasPattern: true}},
		{"show databases like 'it''s';", Command{Kind: ShowDatabases, Pattern: "it's", HasPattern: true}},
		{"SHOW DATABASES WHERE `Database` = 'x'", Command{Kind: Unsupported}},
		{"SHOW DATABASES LIKE 'a' OR 1", Command{Kind: Unsupported}},
		{"USE shop", Command{Kind: Use, Name: "shop"}},
		{"use `my``db` ;", Command{Kind: Use, Name: "my`db"}},
		{"SHOW DATABASE_SIZE", Command{}},
		{"SHOW TABLES", Command{}},
		{"SELECT 'SHOW DATABASES'", Command{}},
		{"USE a b", Command{}},
		{"SHOW DATABASES; DROP TABLE t", Command{Kind: Unsupported}},
	}
	for _, c := range cases {
		if got := Parse(c.sql); got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.sql, got, c.want)
		}
	}
}

func TestFilter(t *testing.T) {
	names := []string{"app", "app_test", "appXtest", "Shop", "a.b"}
	cases := map[string]string{
		"app%":      "app,app_test,appXtest",
		"app_test":  "app_test,appXtest",
		`app\_test`: "app_test",
		"shop":      "Shop",
		"a.b":       "a.b",
		"a_b":       "a.b",
		"%":         "app,app_test,appXtest,Shop,a.b",
		"nope":      "",
	}
	for pattern, want := range cases {
		got := strings.Join(Command{Pattern: pattern, HasPattern: true}.Filter(names), ",")
		if got != want {
			t.Errorf("LIKE %q = %q, want %q", pattern, got, want)
		}
	}
	if got := (Command{}).Filter(names); len(got) != len(names) {
		t.Errorf("no pattern: %v", got)
	}
}
