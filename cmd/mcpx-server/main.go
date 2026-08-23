package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"mcpx/internal/logging"
	"mcpx/internal/server"
	buildversion "mcpx/internal/version"
)

// Set by GoReleaser / -ldflags at release build time.
var (
	version = buildversion.Current
	commit  = "none"
	date    = "unknown"
)

type buildProvenance struct {
	Version string
	Commit  string
	Date    string
}

func resolveBuildProvenance(versionValue, commitValue, dateValue string, settings []debug.BuildSetting) buildProvenance {
	resolved := buildProvenance{Version: versionValue, Commit: commitValue, Date: dateValue}
	if strings.TrimSpace(resolved.Version) == "" {
		resolved.Version = buildversion.Current
	}
	if strings.TrimSpace(resolved.Commit) != "" && resolved.Commit != "none" {
		return resolved
	}
	var revision string
	modified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision != "" {
		resolved.Commit = revision
		if modified {
			resolved.Commit += "-dirty"
		}
	} else if strings.TrimSpace(resolved.Commit) == "" {
		resolved.Commit = "none"
	}
	if strings.TrimSpace(resolved.Date) == "" {
		resolved.Date = "unknown"
	}
	return resolved
}

func currentBuildProvenance() buildProvenance {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return resolveBuildProvenance(version, commit, date, settings)
}

func main() {
	build := currentBuildProvenance()
	backgroundChild := false
	if len(os.Args) >= 2 && os.Args[1] == backgroundChildSubcommand {
		backgroundChild = true
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	// Subcommands (before flag.Parse so they own their flags).
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "observe":
			os.Exit(runObserve(os.Args[2:]))
		case "workspace":
			os.Exit(runWorkspaceCommand(os.Args[2:]))
		case "oauth-register":
			os.Exit(runOAuthRegister(os.Args[2:]))
		case "update":
			os.Exit(runUpdate(os.Args[2:], build))
		case "help", "-h", "--help":
			printUsage()
			os.Exit(0)
		}
	}

	addr := flag.String("addr", "", "override listen addr host:port")
	logLevel := flag.String("log-level", "", "debug|info|warn|error (or MCPX_LOG_LEVEL)")
	logFormat := flag.String("log-format", "", "text|json (or MCPX_LOG_FORMAT)")
	background := flag.Bool("d", false, "run server in background")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = printUsage
	flag.Parse()

	if *showVersion {
		fmt.Printf("mcpx %s (commit=%s date=%s)\n", build.Version, build.Commit, build.Date)
		os.Exit(0)
	}
	if *background {
		pid, logPath, stoppedPIDs, err := startBackground(os.Args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "start background server: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(backgroundStartMessage(pid, logPath, stoppedPIDs))
		return
	}

	if !backgroundChild {
		stoppedPIDs, err := stopExistingBackground()
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop previous background daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(backgroundStopMessage(stoppedPIDs))
	}

	logging.Init(logging.Options{Level: *logLevel, Format: *logFormat})
	logging.Info("mcpx", "version", build.Version, "commit", build.Commit)

	rt, err := server.New(server.Options{
		AddrOverride: *addr,
		Version:      build.Version,
		Commit:       build.Commit,
		Date:         build.Date,
	})
	if err != nil {
		logging.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := rt.Start(); err != nil {
		logging.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func backgroundStopMessage(stoppedPIDs []int) string {
	var output strings.Builder
	for _, stoppedPID := range stoppedPIDs {
		fmt.Fprintf(&output, "mcpx stopped previous background daemon (pid=%d)\n", stoppedPID)
	}
	return output.String()
}

func backgroundStartMessage(pid int, logPath string, stoppedPIDs []int) string {
	var output strings.Builder
	output.WriteString(backgroundStopMessage(stoppedPIDs))
	fmt.Fprintf(&output, "mcpx started in background (pid=%d, log=%s)\n", pid, logPath)
	return output.String()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `mcpx — MCPX Runtime

Usage:
  mcpx [flags]                     启动 Streamable HTTP 服务
  mcpx observe [flags] <name>      终端只读观测 Workspace 事件
  mcpx workspace <command>        管理 Workspace registry（list/register/rename/unregister/prune）
  mcpx oauth-register [url]        动态注册 OAuth 客户端（粘贴 ChatGPT 回调 URL）
  mcpx update [flags]              从 GitHub Release 检查并安装新版本
  mcpx -version

oauth-register:
  mcpx oauth-register 'https://chatgpt.com/connector/oauth/…'
  mcpx oauth-register          # 交互粘贴回调
  mcpx oauth-register -base https://mcp.example.com 'https://…'

Flags (server):
`)
	flag.PrintDefaults()
}
