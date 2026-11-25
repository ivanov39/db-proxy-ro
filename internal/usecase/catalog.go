package usecase

import (
	"errors"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

// LoadCatalog reads every source and builds a validated catalog. It fails if
// any definition is broken or if database names collide.
func LoadCatalog(sources ...TargetSource) (*domain.Catalog, error) {
	var (
		all  []domain.Target
		errs []error
	)
	for _, s := range sources {
		targets, err := s.Load()
		if err != nil {
			errs = append(errs, err)
		}
		all = append(all, targets...)
	}
	catalog, err := domain.NewCatalog(all)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return catalog, nil
}
