package paramsfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pgsql")
	write(t, dir, "a.toml", `
host     = "db.example"
user     = "reader"
password = "secret"
name     = "app"
port     = 5432`)
	write(t, dir, "b.toml", `
host = "db2.example"
user = "reader"
name = "app"
alias = "app2"
port = 5433
sslmode = "require"`)
	write(t, dir, "notes.txt", "ignored")

	targets, err := New(root, domain.PostgreSQL).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	a, b := targets[0], targets[1]
	if a.Name != "app" || a.Database != "app" || a.Password != "secret" || a.TLS != domain.TLSPrefer ||
		a.DBMS != domain.PostgreSQL || a.Addr() != "db.example:5432" {
		t.Errorf("a = %+v", a)
	}
	if b.Name != "app2" || b.Database != "app" || b.TLS != domain.TLSRequire {
		t.Errorf("b = %+v", b)
	}
	if _, err := domain.NewCatalog(targets); err != nil {
		t.Errorf("catalog: %v", err)
	}
}

func TestLoadMissingDirectory(t *testing.T) {
	targets, err := New(t.TempDir(), domain.MySQL).Load()
	if err != nil || len(targets) != 0 {
		t.Fatalf("got %v, %v", targets, err)
	}
}

func TestLoadErrorsDoNotLeakPassword(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mysql")
	write(t, dir, "syntax.toml", `password = "hunter2`)
	write(t, dir, "type.toml", "host = \"h\"\npassword = \"hunter2\"\nport = \"x\"")
	write(t, dir, "typo.toml", "host = \"h\"\npasword = \"hunter2\"")

	_, err := New(root, domain.MySQL).Load()
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	for _, name := range []string{"syntax.toml", "type.toml", "typo.toml"} {
		if !strings.Contains(msg, name) {
			t.Errorf("error does not mention %s: %s", name, msg)
		}
	}
	if strings.Contains(msg, "hunter2") {
		t.Errorf("error leaks the password: %s", msg)
	}
}
