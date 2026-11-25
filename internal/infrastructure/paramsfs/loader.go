// Package paramsfs loads database definitions from TOML files on disk:
// <dir>/pgsql/*.toml and <dir>/mysql/*.toml, one database per file.
package paramsfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

// file is the on-disk format of one definition.
type file struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	// Name is the real database name.
	Name string `toml:"name"`
	// Alias is the name clients use; it defaults to Name.
	Alias   string `toml:"alias"`
	SSLMode string `toml:"sslmode"`
}

// Source reads the definitions of one DBMS from one directory.
type Source struct {
	dir  string
	dbms domain.DBMS
}

// New returns a source for <paramsDir>/<dbms>.
func New(paramsDir string, dbms domain.DBMS) Source {
	return Source{dir: filepath.Join(paramsDir, string(dbms)), dbms: dbms}
}

// Dir returns the directory the source reads.
func (s Source) Dir() string { return s.dir }

// Load parses every *.toml file of the directory. A missing directory is not
// an error: it simply holds no databases.
func (s Source) Load() ([]domain.Target, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read params directory: %w", err)
	}
	var (
		targets []domain.Target
		errs    []error
	)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		t, err := s.parse(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		targets = append(targets, t)
	}
	return targets, errors.Join(errs...)
}

func (s Source) parse(path string) (domain.Target, error) {
	var f file
	meta, err := toml.DecodeFile(path, &f)
	if err != nil {
		// The decoder quotes the offending line, which may hold the password.
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return domain.Target{}, fmt.Errorf("invalid TOML at line %d: %s", perr.Position.Line, perr.Message)
		}
		return domain.Target{}, errors.New("invalid TOML: a value has the wrong type")
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return domain.Target{}, fmt.Errorf("unknown key %q", undecoded[0].String())
	}
	t := domain.Target{
		DBMS:     s.dbms,
		Name:     f.Alias,
		Database: f.Name,
		Host:     f.Host,
		Port:     f.Port,
		User:     f.User,
		Password: f.Password,
		TLS:      domain.TLSMode(f.SSLMode),
		Source:   path,
	}
	if t.Name == "" {
		t.Name = f.Name
	}
	if t.TLS == "" {
		t.TLS = domain.TLSPrefer
	}
	return t, nil
}
