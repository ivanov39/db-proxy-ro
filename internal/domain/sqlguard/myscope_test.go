package sqlguard

import (
	"strings"
	"testing"
)

func TestScopeMySQL(t *testing.T) {
	cases := []struct {
		sql        string
		ok, info   bool
		qualifiers string
		literals   string
	}{
		{"/* ide */ -- List Schemas\nselect schema_name from information_schema.schemata", true, true, "", ""},
		{"select T.table_name from INFORMATION_SCHEMA.tables T where T.table_schema = 'shop' and x = 'y';", true, true, "T", "shop,y"},
		{"select * from Shop.orders o join `other db`.t on o.id = t.id", true, false, "Shop,other db,o,t", ""},
		{"select a.b.c from a.b", true, false, "a", ""},
		{"select 1", true, false, "", ""},
		{"show tables", false, false, "", ""},
		{"select 1; select 2", false, false, "", ""},
		{`select 'a\'' from shop.t`, false, false, "", ""},
		{"select 'unterminated", false, false, "", ""},
	}
	for _, c := range cases {
		got, ok := ScopeMySQL(c.sql)
		if ok != c.ok {
			t.Errorf("ScopeMySQL(%q) ok = %v", c.sql, ok)
			continue
		}
		if !ok {
			continue
		}
		if got.InformationSchema != c.info || strings.Join(got.Qualifiers, ",") != c.qualifiers ||
			strings.Join(got.Literals, ",") != c.literals {
			t.Errorf("ScopeMySQL(%q) = %+v", c.sql, got)
		}
	}
}
