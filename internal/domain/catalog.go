package domain

import (
	"errors"
	"fmt"
	"sort"
)

type catalogKey struct {
	dbms DBMS
	name string
}

// Catalog is an immutable, validated set of targets.
type Catalog struct {
	targets map[catalogKey]Target
}

// NewCatalog validates the targets and builds a catalog. Every problem found is
// reported, not just the first one, so a broken configuration can be fixed in one go.
func NewCatalog(targets []Target) (*Catalog, error) {
	c := &Catalog{targets: make(map[catalogKey]Target, len(targets))}
	var errs []error
	for _, t := range targets {
		if err := t.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Source, err))
			continue
		}
		key := catalogKey{t.DBMS, t.Name}
		if prev, ok := c.targets[key]; ok {
			errs = append(errs, fmt.Errorf("%w: %s database %q is defined in both %s and %s",
				ErrDuplicateDatabase, t.DBMS, t.Name, prev.Source, t.Source))
			continue
		}
		c.targets[key] = t
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// Lookup finds the target a client asked for.
func (c *Catalog) Lookup(dbms DBMS, name string) (Target, error) {
	t, ok := c.targets[catalogKey{dbms, name}]
	if !ok {
		return Target{}, fmt.Errorf("%w: %q", ErrUnknownDatabase, name)
	}
	return t, nil
}

// Names returns the sorted database names configured for a DBMS.
func (c *Catalog) Names(dbms DBMS) []string {
	var names []string
	for k := range c.targets {
		if k.dbms == dbms {
			names = append(names, k.name)
		}
	}
	sort.Strings(names)
	return names
}
