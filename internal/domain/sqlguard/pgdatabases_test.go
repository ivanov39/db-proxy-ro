package sqlguard

import (
	"strings"
	"testing"
)

func TestVirtualize(t *testing.T) {
	v := PgDatabases{Names: []string{"a", "it's"}, Current: "a", Real: "a"}
	sub := v.subquery(nil)
	if !strings.Contains(sub, "'it''s'") {
		t.Fatalf("names are not quoted: %s", sub)
	}
	cases := []struct{ in, want string }{
		{"SELECT datname FROM pg_database", "SELECT datname FROM " + sub + " AS pg_database"},
		{"SELECT 1 FROM pg_catalog.pg_database d WHERE true", "SELECT 1 FROM " + sub + " d WHERE true"},
		{"SELECT 1 FROM pg_database WHERE true", "SELECT 1 FROM " + sub + " AS pg_database WHERE true"},
		{"SELECT 1 FROM pg_database AS d", "SELECT 1 FROM " + sub + " AS d"},
		{`SELECT 1 FROM x JOIN "pg_database" ON true`, "SELECT 1 FROM x JOIN " + sub + " AS pg_database ON true"},
		{"SELECT 1 FROM x, pg_database", "SELECT 1 FROM x, " + sub + " AS pg_database"},
		// Not a reference to the table.
		{"SELECT pg_database.datname, 'FROM pg_database' FROM t", ""},
		{"SELECT 1 FROM other.pg_database", ""},
		{"SELECT pg_database_size('a')", ""},
		{"SELECT 1 FROM pg_database_owner", ""},
		{"SELECT current_database()", ""}, // the names are equal: nothing to hide
		// Would be read differently depending on standard_conforming_strings.
		{`SELECT '\' FROM pg_database --'`, ""},
	}
	for _, c := range cases {
		got, changed := v.Virtualize(c.in)
		if c.want == "" {
			if changed {
				t.Errorf("Virtualize(%q) changed the query: %s", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("Virtualize(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}

	// Hidden columns are carried only when the query uses them.
	got, _ := v.Virtualize("SELECT age(T.xmin), T.ctid FROM pg_database T")
	if want := "SELECT age(T.xmin), T.ctid FROM " + v.subquery([]string{"xmin", "ctid"}) + " T"; got != want {
		t.Errorf("system columns: got %s", got)
	}
	if !strings.Contains(got, ".*, _d.xmin, _d.ctid FROM") {
		t.Errorf("system columns are missing: %s", got)
	}

	alias := PgDatabases{Names: []string{"alias"}, Current: "alias", Real: "real"}
	got, _ = alias.Virtualize("SELECT current_database(), pg_catalog.current_database ( ), x.current_database()")
	want := "SELECT ('alias'::name), ('alias'::name), x.current_database()"
	if got != want {
		t.Errorf("current_database: got %s", got)
	}
}
