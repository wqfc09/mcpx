package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureResult reports what bootstrap created.
type EnsureResult struct {
	HomeDir       string
	CreatedHome   bool
	CreatedConfig bool
	CreatedMCP    bool
	CreatedLogs   bool
	CreatedSkills bool
	ConfigPath    string
	MCPPath       string
	LogDir        string
	SkillsDir     string
}

// EnsureGlobalLayout creates ~/.mcpx (or $MCPX_HOME) skeleton if missing:
//   - config.yaml   (defaults, only if absent)
//   - .mcp.json     (empty mcpServers, only if absent)
//   - logs/         directory
//   - skills/       directory (optional empty skill root)
//
// Existing files are never overwritten.
func EnsureGlobalLayout() (EnsureResult, error) {
	var res EnsureResult
	home, err := HomeDir()
	if err != nil {
		return res, err
	}
	res.HomeDir = home

	if st, err := os.Stat(home); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(home, 0o755); err != nil {
				return res, fmt.Errorf("mkdir home: %w", err)
			}
			res.CreatedHome = true
		} else {
			return res, err
		}
	} else if !st.IsDir() {
		return res, fmt.Errorf("mcpx home is not a directory: %s", home)
	}

	cfgPath, err := GlobalConfigPath()
	if err != nil {
		return res, err
	}
	res.ConfigPath = cfgPath
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfg := DefaultConfig()
		// Persist a concrete log dir under home for clarity in the written file.
		cfg.Logging.Dir = filepath.Join(home, "logs")
		if err := WriteGlobal(cfgPath, cfg); err != nil {
			return res, fmt.Errorf("write config.yaml: %w", err)
		}
		res.CreatedConfig = true
	} else if err != nil {
		return res, err
	}

	mcpPath, err := GlobalMCPPath()
	if err != nil {
		return res, err
	}
	res.MCPPath = mcpPath
	if _, err := os.Stat(mcpPath); os.IsNotExist(err) {
		if err := WriteMCPFile(mcpPath, MCPFile{MCPServers: map[string]MCPServer{}}); err != nil {
			return res, fmt.Errorf("write .mcp.json: %w", err)
		}
		res.CreatedMCP = true
	} else if err != nil {
		return res, err
	}

	res.LogDir = filepath.Join(home, "logs")
	if err := os.MkdirAll(res.LogDir, 0o755); err != nil {
		return res, err
	}
	// MkdirAll is idempotent; mark created only if we just made home or dir was empty new — optional
	if res.CreatedHome {
		res.CreatedLogs = true
	} else if _, err := os.Stat(res.LogDir); err == nil {
		// already existed or created now — check if we needed create
	}
	if entries, err := os.ReadDir(res.LogDir); err == nil && len(entries) == 0 {
		// fine
	}
	if _, err := os.Stat(res.LogDir); os.IsNotExist(err) {
		res.CreatedLogs = true
	} else {
		// ensure exists
		_ = os.MkdirAll(res.LogDir, 0o755)
	}

	res.SkillsDir = filepath.Join(home, "skills")
	if _, err := os.Stat(res.SkillsDir); os.IsNotExist(err) {
		if err := os.MkdirAll(res.SkillsDir, 0o755); err != nil {
			return res, err
		}
		res.CreatedSkills = true
		// optional README so user knows what goes here
		_ = os.WriteFile(filepath.Join(res.SkillsDir, "README.md"), []byte(""+
			"# MCPX skills\n\n"+
			"Put executable skills here as:\n\n"+
			"```\n"+
			"skills/\n"+
			"  my-skill/\n"+
			"    skill.yaml   # or SKILL.md\n"+
			"    main.py\n"+
			"```\n"+
			"\nAlso scanned by default: `~/.agents/skills`, `~/.codex/skills`, etc.\n"), 0o644)
	}

	// Example snippet (never overwrite)
	exPath := filepath.Join(home, "workspaces.example.yaml")
	if _, err := os.Stat(exPath); os.IsNotExist(err) {
		_ = os.WriteFile(exPath, []byte(workspacesExampleYAML), 0o644)
	}

	return res, nil
}

// workspacesExampleYAML is written to ~/.mcpx/workspaces.example.yaml on first boot.
const workspacesExampleYAML = `# 复制下列 workspaces 段到 config.yaml（路径改成你的真实目录）
#
# 也可用命令管理（会写回 config.yaml）：
#   mcpx workspace register --name my-app /Users/you/code/my-app
#   mcpx workspace list
#   mcpx workspace rename my-app new-name
#   mcpx workspace unregister new-name
#   mcpx workspace prune [--apply]
#
# 字段：
#   name         逻辑名，创建 Remote Session 时通过 workspace 使用
#   path         项目根目录（绝对路径）
#   description  给 AI 看的项目说明（可选）

workspaces:
  - name: mcpx
    path: /Users/star/Workspace/code/ai/mcpx
    description: "MCPX Runtime 本体（Go）"

  - name: hospital-app
    path: /Users/star/Workspace/code/java/hospital-app
    description: "医院业务应用"

# 项目级可选覆盖：{path}/.mcpx.yaml
# ---
# description: "更详细的项目描述（覆盖全局 description）"
# ---
`

// WriteMCPFile writes .mcp.json with pretty JSON.
func WriteMCPFile(path string, f MCPFile) error {
	if f.MCPServers == nil {
		f.MCPServers = map[string]MCPServer{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
