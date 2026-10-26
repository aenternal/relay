package storage

import "relay/internal/domain"

// ValidateProjectState uses the same ownership, protocol and descriptor checks
// as Save without beginning a transaction or modifying the caller's snapshot.
func ValidateProjectState(state domain.State) error {
	_, err := sanitize(state)
	return err
}
