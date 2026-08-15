package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

func TestRemoteRequestAllowsReadWithoutPurpose(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	request := mcpresult.Request(map[string]any{"workspace": "demo"})

	_, _, failure := runtime.remoteRequest(context.Background(), request)
	if failure != nil {
		t.Fatalf("read request should not require purpose: %+v", failure)
	}
}

func TestMutatingRequestRejectsOversizedPurpose(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	request := mcpresult.Request(map[string]any{"purpose": strings.Repeat("x", 513), "workspace": "demo"})

	_, _, _, failure := runtime.changeRequest(context.Background(), request, true)
	if failure == nil {
		t.Fatal("oversized purpose was accepted")
	}
	response := decodeToolResult(t, failure)
	if errorCode(response) != "purpose_required" {
		t.Fatalf("response=%+v", response)
	}
}

func TestPublicToolSchemasExcludeObservationBookkeeping(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	forbidden := []string{"reasoning_summary", "progress_summary", "next_step", "goal", "task_id"}
	for name, registered := range runtime.listedToolMap() {
		schema := decodedToolSchema(t, registered)
		assertNoSchemaFields(t, name, schema, forbidden)
		if name != "observe" {
			assertNoSchemaFields(t, name, schema, []string{"call_id"})
		}
	}

	for _, name := range []string{"workspace", "read", "observe", "runtime_read", "environment_read"} {
		schema := decodedToolSchema(t, runtime.listedToolMap()[name])
		properties, _ := schema["properties"].(map[string]any)
		if properties["purpose"] != nil {
			t.Errorf("read-only tool %q must not expose purpose: %+v", name, properties)
		}
	}

	read := decodedToolSchema(t, runtime.listedToolMap()["read"])
	readProperties, _ := read["properties"].(map[string]any)
	if readProperties["execution_mode"] != nil {
		t.Fatalf("read must not expose generic execution_mode: %+v", readProperties)
	}
}

func TestEffectfulToolsOwnPurposeContract(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := runtime.listedToolMap()

	for _, name := range []string{"edit", "operation_batch", "screenshot_capture", "secret_provide"} {
		schema := decodedToolSchema(t, registered[name])
		properties, _ := schema["properties"].(map[string]any)
		if properties["purpose"] == nil || !schemaRequires(schema, "purpose") {
			t.Errorf("effectful tool %q must explicitly require purpose: %+v", name, schema)
		}
	}
	for _, name := range []string{"skill_tool", "mcp_tool", "plugin_tool"} {
		schema := decodedToolSchema(t, registered[name])
		effectActions := []string{"call"}
		if name == "plugin_tool" {
			effectActions = []string{"inbox", "signal"}
		}
		for _, effectAction := range effectActions {
			if branch := actionBranch(schema, effectAction); branch == nil || !schemaRequires(branch, "purpose") {
				t.Fatalf("%s(%s) must require purpose: %+v", name, effectAction, schema)
			}
		}
		readActions := []string{"list", "describe"}
		for _, action := range readActions {
			if branch := actionBranch(schema, action); branch == nil || schemaRequires(branch, "purpose") {
				t.Fatalf("%s(%s) must remain read-only and not require purpose: %+v", name, action, schema)
			}
		}
	}

	execute := decodedToolSchema(t, registered["execute"])
	if branch := actionBranch(execute, "run"); branch == nil || !schemaRequires(branch, "purpose") {
		t.Fatalf("execute(run) must require purpose: %+v", execute)
	}
	moveOut := decodedToolSchema(t, registered["move_out"])
	if branch := actionBranch(moveOut, "prepare"); branch == nil || !schemaRequires(branch, "purpose") {
		t.Fatalf("move_out(prepare) must require purpose: %+v", moveOut)
	}
	if branch := actionBranch(moveOut, "submit"); branch == nil || schemaRequires(branch, "purpose") {
		t.Fatalf("move_out(submit) must use frozen server purpose, not client purpose: %+v", moveOut)
	}
}

func TestPurposeDescriptionsAreLocalToEffect(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := runtime.listedToolMap()

	checks := map[string]string{
		"execute":            "用户目标",
		"edit":               "文件变更",
		"screenshot_capture": "屏幕",
		"secret_provide":     "Secret",
	}
	for name, fragment := range checks {
		schema := decodedToolSchema(t, registered[name])
		properties, _ := schema["properties"].(map[string]any)
		purpose, _ := properties["purpose"].(map[string]any)
		description, _ := purpose["description"].(string)
		if !strings.Contains(description, fragment) {
			t.Errorf("tool %q purpose description missing %q: %q", name, fragment, description)
		}
	}
}

func decodedToolSchema(t *testing.T, tool mcp.Tool) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tool), &schema); err != nil {
		t.Fatalf("tool %q schema: %v", tool.Name, err)
	}
	return schema
}

func assertNoSchemaFields(t *testing.T, toolName string, schema map[string]any, forbidden []string) {
	t.Helper()
	if properties, _ := schema["properties"].(map[string]any); properties != nil {
		for _, field := range forbidden {
			if properties[field] != nil {
				t.Errorf("tool %q must not expose protocol bookkeeping %q: %+v", toolName, field, properties)
			}
		}
	}
	branches, _ := schema["oneOf"].([]any)
	for _, raw := range branches {
		branch, _ := raw.(map[string]any)
		if branch != nil {
			assertNoSchemaFields(t, toolName, branch, forbidden)
		}
	}
}

func schemaRequires(schema map[string]any, field string) bool {
	required, _ := schema["required"].([]any)
	for _, raw := range required {
		if raw == field {
			return true
		}
	}
	return false
}

func actionBranch(schema map[string]any, action string) map[string]any {
	branches, _ := schema["oneOf"].([]any)
	for _, raw := range branches {
		branch, _ := raw.(map[string]any)
		properties, _ := branch["properties"].(map[string]any)
		actionSchema, _ := properties["action"].(map[string]any)
		if actionSchema["const"] == action {
			return branch
		}
	}
	return nil
}
