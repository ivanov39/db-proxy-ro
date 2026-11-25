package usecase

import (
	"errors"
	"testing"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

type fakeSource struct {
	targets []domain.Target
	err     error
}

func (f fakeSource) Load() ([]domain.Target, error) { return f.targets, f.err }

type fakeAudit struct{ records []QueryRecord }

func (f *fakeAudit) Record(r QueryRecord) { f.records = append(f.records, r) }

func target(dbms domain.DBMS, name string) domain.Target {
	return domain.Target{DBMS: dbms, Name: name, Database: name, Host: "h", Port: 1, User: "u",
		TLS: domain.TLSPrefer, Source: string(dbms) + "/" + name}
}

func TestLoadCatalog(t *testing.T) {
	c, err := LoadCatalog(
		fakeSource{targets: []domain.Target{target(domain.PostgreSQL, "a")}},
		fakeSource{targets: []domain.Target{target(domain.MySQL, "a")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup(domain.MySQL, "a"); err != nil {
		t.Fatal(err)
	}

	_, err = LoadCatalog(fakeSource{targets: []domain.Target{
		target(domain.MySQL, "a"), target(domain.MySQL, "a"),
	}})
	if !errors.Is(err, domain.ErrDuplicateDatabase) {
		t.Fatalf("want ErrDuplicateDatabase, got %v", err)
	}

	boom := errors.New("boom")
	if _, err = LoadCatalog(fakeSource{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("want source error, got %v", err)
	}
}

func TestGatekeeperAudit(t *testing.T) {
	catalog, _ := domain.NewCatalog(nil)
	audit := &fakeAudit{}
	g := NewGatekeeper(catalog, audit)
	s := Session{DBMS: domain.MySQL, Database: "a"}

	if dec := g.Inspect(s, "query", "SELECT 1"); !dec.Allowed {
		t.Fatalf("SELECT refused: %s", dec.Reason)
	}
	if len(audit.records) != 0 {
		t.Fatal("allowed query must be logged on completion, not on inspection")
	}
	g.Completed(s, "query", "SELECT 1", Outcome{Rows: 1})

	if dec := g.Inspect(s, "query", "DELETE FROM t"); dec.Allowed {
		t.Fatal("DELETE allowed")
	}
	// "$$" quotes a string in PostgreSQL only: the dialect must follow the session.
	if dec := g.Inspect(s, "query", "SELECT $$ ; DELETE FROM t $$"); dec.Allowed {
		t.Fatal("MySQL session inspected with PostgreSQL rules")
	}
	if len(audit.records) != 3 || !audit.records[0].Allowed || audit.records[1].Allowed ||
		audit.records[1].Reason == "" {
		t.Fatalf("unexpected audit trail: %+v", audit.records)
	}
}
