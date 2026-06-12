package fileutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FindFileEqualFold resolves a canonical file name in dir without depending on
// the host filesystem's case-sensitivity rules.
func FindFileEqualFold(dir, canonicalName string) (string, error) {
	canonicalName = strings.TrimSpace(canonicalName)
	if canonicalName == "" {
		return "", fmt.Errorf("canonical file name is required")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), canonicalName) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%s not found under %s: %w", canonicalName, dir, os.ErrNotExist)
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous case-insensitive file name %q under %s: %s", canonicalName, dir, strings.Join(matches, ", "))
	}
	return filepath.Join(dir, matches[0]), nil
}

func IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
