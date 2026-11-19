package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// ErrConflict reports that a save would overwrite a concurrent change.
var ErrConflict = errors.New("account changed concurrently")

// ErrDeleted reports that a save's previously observed account no longer exists.
var ErrDeleted = errors.New("account deleted")

// MergeJSON applies the fields changed since baseline to the current record.
// Comparing decoded values makes JSON formatting and key order immaterial.
func MergeJSON(baseline, proposed, current []byte) ([]byte, error) {
	before, err := decodeObject(baseline)
	if err != nil {
		return nil, err
	}
	next, err := decodeObject(proposed)
	if err != nil {
		return nil, err
	}
	latest, err := decodeObject(current)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{})
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range next {
		keys[k] = struct{}{}
	}
	for k := range keys {
		old, had := before[k]
		value, has := next[k]
		if had == has && reflect.DeepEqual(old, value) {
			continue
		}
		live, exists := latest[k]
		if !(had == exists && reflect.DeepEqual(old, live)) && !(has == exists && reflect.DeepEqual(value, live)) {
			return nil, fmt.Errorf("%w: field %s", ErrConflict, k)
		}
		if has {
			latest[k] = value
		} else {
			delete(latest, k)
		}
	}
	return json.Marshal(latest)
}
