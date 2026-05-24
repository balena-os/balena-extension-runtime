package manager

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
)

func runDeactivateHook(logger *slog.Logger, c Container) error {
	var errs []error
	for _, m := range c.Mounts {
		if m.Type != "volume" || m.Source == "" {
			continue
		}
		if err := hooks.ExecuteIfPresent(logger, m.Source, "deactivate", c.Labels, nil); err != nil {
			errs = append(errs, fmt.Errorf("deactivate from volume %s: %w", m.Destination, err))
		}
	}
	return errors.Join(errs...)
}
