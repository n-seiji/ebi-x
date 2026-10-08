package codex

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// loadConfigOverrides converts a local Codex TOML file into deterministic
// command-line overrides. The caller keeps --ignore-user-config enabled and
// appends ebi-x's mandatory security overrides after these values.
func loadConfigOverrides(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	var config map[string]any
	if _, err := toml.DecodeFile(path, &config); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("parse local Codex config %q: %w", path, err)
	}

	var overrides []string
	if err := flattenConfig(nil, config, &overrides); err != nil {
		return nil, fmt.Errorf("encode local Codex config %q: %w", path, err)
	}
	sort.Strings(overrides)
	return overrides, nil
}

func flattenConfig(prefix []string, value map[string]any, overrides *[]string) error {
	if len(value) == 0 && len(prefix) > 0 {
		*overrides = append(*overrides, encodeDottedKey(prefix)+"={}")
		return nil
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		if len(prefix) == 0 && isSecurityOwnedKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := append(append([]string(nil), prefix...), key)
		if nested, ok := value[key].(map[string]any); ok {
			if err := flattenConfig(path, nested, overrides); err != nil {
				return err
			}
			continue
		}
		encoded, err := encodeTOMLValue(value[key])
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Join(path, "."), err)
		}
		*overrides = append(*overrides, encodeDottedKey(path)+"="+encoded)
	}
	return nil
}

func isSecurityOwnedKey(key string) bool {
	for _, prefix := range []string{
		"approval",
		"dangerously_",
		"default_permission",
		"developer_instruction",
		"permission",
		"profile",
		"sandbox",
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func encodeDottedKey(path []string) string {
	encoded := make([]string, len(path))
	for i, key := range path {
		if isBareTOMLKey(key) {
			encoded[i] = key
		} else {
			encoded[i] = strconv.Quote(key)
		}
	}
	return strings.Join(encoded, ".")
}

func isBareTOMLKey(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func encodeTOMLValue(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return strconv.Quote(value), nil
	case bool:
		return strconv.FormatBool(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case float64:
		if math.IsNaN(value) {
			return "nan", nil
		}
		if math.IsInf(value, 1) {
			return "+inf", nil
		}
		if math.IsInf(value, -1) {
			return "-inf", nil
		}
		encoded := strconv.FormatFloat(value, 'g', -1, 64)
		if !strings.ContainsAny(encoded, ".eE") {
			encoded += ".0"
		}
		return encoded, nil
	case time.Time:
		switch value.Location().String() {
		case "datetime-local":
			return value.Format("2006-01-02T15:04:05.999999999"), nil
		case "date-local":
			return value.Format("2006-01-02"), nil
		case "time-local":
			return value.Format("15:04:05.999999999"), nil
		default:
			return value.Format(time.RFC3339Nano), nil
		}
	case []map[string]any:
		items := make([]string, len(value))
		for i, item := range value {
			encoded, err := encodeInlineTable(item)
			if err != nil {
				return "", err
			}
			items[i] = encoded
		}
		return "[" + strings.Join(items, ",") + "]", nil
	case []any:
		items := make([]string, len(value))
		for i, item := range value {
			encoded, err := encodeTOMLValue(item)
			if err != nil {
				return "", err
			}
			items[i] = encoded
		}
		return "[" + strings.Join(items, ",") + "]", nil
	case map[string]any:
		return encodeInlineTable(value)
	default:
		return "", fmt.Errorf("unsupported TOML value type %T", value)
	}
}

func encodeInlineTable(value map[string]any) (string, error) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		encoded, err := encodeTOMLValue(value[key])
		if err != nil {
			return "", err
		}
		items = append(items, encodeDottedKey([]string{key})+"="+encoded)
	}
	return "{" + strings.Join(items, ",") + "}", nil
}
