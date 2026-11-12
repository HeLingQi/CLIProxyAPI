package executor

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var issue6478Calls = []struct {
	id   string
	name string
}{
	{id: "Write:1#e1f0da81143649f3a378d24af6fc3ec9", name: "Write"},
	{id: "PowerShell:66#cd50350578c34275a26a09a477a81ffb", name: "PowerShell"},
	// A lossy replacement would collide with the first call's already legal ID.
	{id: "Write_1_e1f0da81143649f3a378d24af6fc3ec9", name: "Read"},
	{id: "call_8jAZYBK5veMdr1DD2DqObARK", name: "Bash"},
}

func issue6478History(ids []string) []byte {
	var uses, results, tools []string
	for i, call := range issue6478Calls {
		uses = append(uses, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{"literal_id":%q}}`, ids[i], call.name, call.id))
		results = append(results, fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":%q}`, ids[i], "result for "+call.id))
		tools = append(tools, fmt.Sprintf(`{"name":%q,"input_schema":{"type":"object","properties":{"literal_id":{"type":"string"}}}}`, call.name))
	}
	return []byte(fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":64,"tools":[%s],"messages":[{"role":"user","content":"run tools"},{"role":"assistant","content":[%s]},{"role":"user","content":[%s]}]}`, strings.Join(tools, ","), strings.Join(uses, ","), strings.Join(results, ",")))
}

func issue6478AssertPortableIDs(t *testing.T, ids []string) {
	t.Helper()
	if len(ids) != len(issue6478Calls) {
		t.Fatalf("tool IDs = %q, want %d calls", ids, len(issue6478Calls))
	}
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	seen := make(map[string]bool)
	for i, id := range ids {
		if !valid.MatchString(id) {
			t.Errorf("tool_use.id[%d] = %q violates Anthropic pattern ^[a-zA-Z0-9_-]+$", i, id)
		}
		if seen[id] {
			t.Errorf("distinct upstream calls collided at tool_use.id[%d] = %q", i, id)
		}
		seen[id] = true
		if i >= 2 && id != issue6478Calls[i].id {
			t.Errorf("already legal tool_use.id[%d] = %q, want unchanged %q", i, id, issue6478Calls[i].id)
		}
	}
}

func TestIssue6478_ClaudeReplaysLegacyToolIDs(t *testing.T) {
	var legacyIDs []string
	for _, call := range issue6478Calls {
		legacyIDs = append(legacyIDs, call.id)
	}
	var idsByMode [][]string
	for _, stream := range []bool{false, true} {
		name := "Execute"
		if stream {
			name = "ExecuteStream"
		}
		t.Run(name, func(t *testing.T) {
			body := executeClaudeContextManagementRequest(t, &config.Config{}, issue6478History(legacyIDs), stream)
			var useIDs, resultIDs []string
			for _, message := range ojson.GetBytes(body, "messages").Array() {
				for _, block := range message.Get("content").Array() {
					switch block.Get("type").String() {
					case "tool_use":
						i := len(useIDs)
						useIDs = append(useIDs, block.Get("id").String())
						if i < len(issue6478Calls) && block.Get("input.literal_id").String() != issue6478Calls[i].id {
							t.Errorf("tool input was rewritten along with ID: %s", block.Raw)
						}
					case "tool_result":
						i := len(resultIDs)
						id := block.Get("tool_use_id").String()
						resultIDs = append(resultIDs, id)
						if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(id) {
							t.Errorf("sent tool_result.tool_use_id[%d] = %q violates Anthropic pattern ^[a-zA-Z0-9_-]+$", i, id)
						}
						if i < len(issue6478Calls) && block.Get("content").String() != "result for "+issue6478Calls[i].id {
							t.Errorf("tool result content was rewritten along with ID: %s", block.Raw)
						}
					}
				}
			}
			issue6478AssertPortableIDs(t, useIDs)
			if !slices.Equal(useIDs, resultIDs) {
				t.Errorf("sent tool_use and tool_result IDs must pair: uses=%q results=%q", useIDs, resultIDs)
			}
			idsByMode = append(idsByMode, useIDs)
		})
	}
	if len(idsByMode) == 2 && !slices.Equal(idsByMode[0], idsByMode[1]) {
		t.Errorf("legacy history repair differs across stream modes: %q vs %q", idsByMode[0], idsByMode[1])
	}
}

func TestIssue6478_ClaudeToolIDRepairPrecedesPayloadRules(t *testing.T) {
	var legacyIDs []string
	for _, call := range issue6478Calls {
		legacyIDs = append(legacyIDs, call.id)
	}
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "*", Exist: []string{"messages.1.content.0.id"}, NotMatch: []map[string]any{{"messages.1.content.0.id": issue6478Calls[0].id}}}},
			Params: map[string]any{"temperature": 0.25},
		}, {
			Models: []config.PayloadModelRule{{Name: "*"}},
			Params: map[string]any{"messages.1.content.0.id": "configured:id#override", "messages.2.content.0.tool_use_id": "configured:id#override"},
		}},
		Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"messages.1.content.1.id", "messages.2.content.1.tool_use_id"}}},
	}}
	for _, stream := range []bool{false, true} {
		name := "Execute"
		if stream {
			name = "ExecuteStream"
		}
		t.Run(name, func(t *testing.T) {
			body := executeClaudeContextManagementRequest(t, cfg, issue6478History(legacyIDs), stream)
			if ojson.GetBytes(body, "temperature").Float() != 0.25 {
				t.Errorf("payload condition did not observe repaired tool ID before final barrier")
			}
			for _, path := range []string{"messages.1.content.0.id", "messages.2.content.0.tool_use_id"} {
				if got := ojson.GetBytes(body, path).String(); got != "configured:id#override" {
					t.Errorf("final user override at %s was changed: got %q", path, got)
				}
			}
			for _, path := range []string{"messages.1.content.1.id", "messages.2.content.1.tool_use_id"} {
				if ojson.GetBytes(body, path).Exists() {
					t.Errorf("final user filter at %s was undone", path)
				}
			}
		})
	}
}
