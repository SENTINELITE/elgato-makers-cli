package listing

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Reorder returns a detached config with gallery entries in a complete 1-based
// permutation. It performs no file access, authentication, or remote changes.
func Reorder(c Config, order []int) (Config, error) {
	count := len(c.Assets.Gallery)
	if count < 3 || count > 10 {
		return Config{}, errors.New("gallery requires 3-10 items before reordering")
	}
	if len(order) != count {
		return Config{}, fmt.Errorf("gallery order must contain all %d positions exactly once", count)
	}
	seen := make([]bool, count)
	for _, position := range order {
		if position < 1 || position > count {
			return Config{}, fmt.Errorf("gallery position %d is outside 1-%d", position, count)
		}
		if seen[position-1] {
			return Config{}, fmt.Errorf("gallery position %d is repeated", position)
		}
		seen[position-1] = true
	}
	data, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	var result Config
	if err = json.Unmarshal(data, &result); err != nil {
		return Config{}, err
	}
	for i, position := range order {
		result.Assets.Gallery[i] = c.Assets.Gallery[position-1]
	}
	return result, nil
}
