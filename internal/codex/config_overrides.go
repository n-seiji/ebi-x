package codex

import (
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// localConfig is what ebi-x takes from the local Codex TOML file.
type localConfig struct {
	// Overrides are deterministic command-line overrides. The caller keeps
	// --ignore-user-config enabled and appends ebi-x's mandatory security
	// overrides after these values.
	Overrides []string
	// MCPNote tells the model which MCP tools the operator left out, so a
	// missing tool is not reported as a failed connection.
	MCPNote string
}

// loadConfigOverrides converts a local Codex TOML file into deterministic
// command-line overrides.
func loadConfigOverrides(path string) ([]string, error) {
	config, err := loadLocalConfig(path)
	return config.Overrides, err
}

func loadLocalConfig(path string) (localConfig, error) {
	if path == "" {
		return localConfig{}, nil
	}
	var config map[string]any
	if _, err := toml.DecodeFile(path, &config); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return localConfig{}, nil
		}
		return localConfig{}, fmt.Errorf("parse local Codex config %q: %w", path, err)
	}

	var overrides []string
	if err := flattenConfig(nil, config, &overrides); err != nil {
		return localConfig{}, fmt.Errorf("encode local Codex config %q: %w", path, err)
	}
	servers, _ := config["mcp_servers"].(map[string]any)
	overrides = append(overrides, bigQueryReadOnlyOverrides(servers)...)
	sort.Strings(overrides)
	return localConfig{Overrides: overrides, MCPNote: mcpToolNote(servers)}, nil
}

// bigQueryReadOnlyOverrides keeps the SQL tool of MCP Toolbox's prebuilt
// BigQuery server read-only. Toolbox allows any statement unless its write
// mode says otherwise, so a server exposing execute_sql without an explicit
// BIGQUERY_WRITE_MODE or BIGQUERY_READONLY runs in "blocked" mode, where
// only SELECT statements are accepted.
func bigQueryReadOnlyOverrides(servers map[string]any) []string {
	var overrides []string
	for _, name := range sortedKeys(servers) {
		server, _ := servers[name].(map[string]any)
		if !isToolboxBigQuery(server) || !exposesTool(server, "execute_sql") {
			continue
		}
		env, _ := server["env"].(map[string]any)
		if _, ok := env["BIGQUERY_WRITE_MODE"]; ok {
			continue
		}
		if _, ok := env["BIGQUERY_READONLY"]; ok {
			continue
		}
		path := []string{"mcp_servers", name, "env", "BIGQUERY_WRITE_MODE"}
		overrides = append(overrides, encodeDottedKey(path)+`="blocked"`)
	}
	return overrides
}

func isToolboxBigQuery(server map[string]any) bool {
	if server == nil {
		return false
	}
	if env, ok := server["env"].(map[string]any); ok {
		if _, ok := env["BIGQUERY_PROJECT"]; ok {
			return true
		}
	}
	args := stringList(server["args"])
	for i, arg := range args {
		if arg == "--prebuilt=bigquery" || (arg == "--prebuilt" && i+1 < len(args) && args[i+1] == "bigquery") {
			return true
		}
	}
	return false
}

// exposesTool reports whether Codex offers the tool of an enabled server.
func exposesTool(server map[string]any, tool string) bool {
	if enabled, ok := server["enabled"].(bool); ok && !enabled {
		return false
	}
	if _, ok := server["enabled_tools"]; ok && !slices.Contains(stringList(server["enabled_tools"]), tool) {
		return false
	}
	return !slices.Contains(stringList(server["disabled_tools"]), tool)
}

// mcpToolNote lists the MCP servers whose tools the local config limits.
func mcpToolNote(servers map[string]any) string {
	var lines []string
	for _, name := range sortedKeys(servers) {
		server, _ := servers[name].(map[string]any)
		if server == nil {
			continue
		}
		if enabled, ok := server["enabled"].(bool); ok && !enabled {
			continue
		}
		var limits []string
		if _, ok := server["enabled_tools"]; ok {
			limits = append(limits, "使えるのは "+joinTools(stringList(server["enabled_tools"]))+" だけ")
		}
		if disabled := stringList(server["disabled_tools"]); len(disabled) > 0 {
			limits = append(limits, joinTools(disabled)+" は使えない")
		}
		if len(limits) == 0 {
			continue
		}
		lines = append(lines, "- "+name+": "+strings.Join(limits, "、"))
	}
	if len(lines) == 0 {
		return ""
	}
	return "# MCP ツールの公開範囲\n\n" +
		"次の MCP サーバーは、運用者の設定（EBIX_HOME/.codex/config.toml の enabled_tools / disabled_tools）で使えるツールを限定しています。\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		"必要なツールがこの範囲にないときは、MCP への接続失敗ではなく設定で公開していないためです。「接続できない」と報告せず、どのサーバーのどのツールが公開されていないか、運用者が設定に追加すれば使えることを依頼者に伝え、公開されているツールでできる範囲を進めてください。"
}

func joinTools(tools []string) string {
	if len(tools) == 0 {
		return "（なし）"
	}
	return strings.Join(tools, ", ")
}

func stringList(value any) []string {
	items, _ := value.([]any)
	list := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			list = append(list, s)
		}
	}
	return list
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
