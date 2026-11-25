package domain

import (
	"errors"
	"testing"
)

func target(dbms DBMS, name, src string) Target {
	return Target{DBMS: dbms, Name: name, Database: name, Host: "h", Port: 1, User: "u", TLS: TLSPrefer, Source: src}
}

func TestCatalogLookup(t *testing.T) {
	c, err := NewCatalog([]Target{
		target(PostgreSQL, "app", "a.toml"),
		target(MySQL, "app", "b.toml"), // same name in another DBMS is fine
		target(MySQL, "shop", "c.toml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Lookup(MySQL, "app"); err != nil || got.Source != "b.toml" {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	if _, err := c.Lookup(PostgreSQL, "shop"); !errors.Is(err, ErrUnknownDatabase) {
		t.Fatalf("want ErrUnknownDatabase, got %v", err)
	}
	if n := c.Names(MySQL); len(n) != 2 || n[0] != "app" || n[1] != "shop" {
		t.Fatalf("Names = %v", n)
	}
}

func TestCatalogRejectsDuplicates(t *testing.T) {
	_, err := NewCatalog([]Target{
		target(PostgreSQL, "app", "a.toml"),
		target(PostgreSQL, "app", "b.toml"),
	})
	if !errors.Is(err, ErrDuplicateDatabase) {
		t.Fatalf("want ErrDuplicateDatabase, got %v", err)
	}
}

func TestCatalogReportsAllProblems(t *testing.T) {
	bad := target(MySQL, "x", "bad.toml")
	bad.Port = 0
	_, err := NewCatalog([]Target{
		target(PostgreSQL, "app", "a.toml"),
		target(PostgreSQL, "app", "b.toml"),
		bad,
	})
	if !errors.Is(err, ErrDuplicateDatabase) || !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("want both errors, got %v", err)
	}
}
