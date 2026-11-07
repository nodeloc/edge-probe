// edge-probe 安装在用户网络中的拨测程序：测量到各边缘节点的延迟并上报，用于按地区和运营商调度。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nodeloc/edge-probe/internal/agent"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bind":
		err = bind(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "doctor":
		err = doctor(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(agent.Version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  edge-probe bind --server https://example.com --token TOKEN [--name NAME] [--config PATH]
  edge-probe run [--config PATH]
  edge-probe doctor [--config PATH]
  edge-probe version
`)
}

func bind(args []string) error {
	var flags = flag.NewFlagSet("bind", flag.ExitOnError)
	var server = flags.String("server", "", "platform URL, for example https://example.com")
	var token = flags.String("token", "", "binding token from the console")
	var name = flags.String("name", "", "display name (default: hostname)")
	var configPath = flags.String("config", agent.DefaultConfigPath(), "config file")
	var limit = flags.Int64("monthly-limit-mb", agent.DefaultMonthlyLimitMB, "monthly probe traffic limit in MB")
	_ = flags.Parse(args)
	if *server == "" || *token == "" {
		return errors.New("--server and --token are required")
	}
	if err := agent.ValidateServer(*server); err != nil {
		return err
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := agent.NewClient(*server).Bind(ctx, &agent.BindRequest{Token: strings.TrimSpace(*token), Name: *name, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: agent.Version})
	if err != nil {
		return err
	}
	var config = &agent.Config{Server: strings.TrimRight(*server, "/"), AgentID: resp.AgentID, Secret: resp.Secret, Name: *name, MonthlyLimitMB: *limit}
	if err = agent.SaveConfig(*configPath, config); err != nil {
		return err
	}
	fmt.Printf("bound as agent %d, config saved to %s\n", resp.AgentID, *configPath)
	return nil
}

func run(args []string) error {
	var flags = flag.NewFlagSet("run", flag.ExitOnError)
	var configPath = flags.String("config", agent.DefaultConfigPath(), "config file")
	var noDirect = flags.Bool("no-direct", false, "do not bypass a local TUN proxy")
	_ = flags.Parse(args)
	agent.SetDirect(!*noDirect)
	config, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	var logger = log.New(os.Stderr, "", log.LstdFlags)
	var runner = agent.NewRunner(config, *configPath)
	runner.Logf = logger.Printf
	runner.Restart = restart
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("edge-probe %s (%s) started as agent %d, server %s", agent.Version, agent.Platform(), config.AgentID, config.Server)
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// restart 执行替换后的新程序；Windows 不支持 exec，启动新进程后退出，计划任务不会因为进程退出而重新启动
func restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		var next = exec.Command(exe, os.Args[1:]...)
		next.Stdout, next.Stderr = os.Stdout, os.Stderr
		if err = next.Start(); err != nil {
			return err
		}
		os.Exit(0)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

func doctor(args []string) error {
	var flags = flag.NewFlagSet("doctor", flag.ExitOnError)
	var configPath = flags.String("config", agent.DefaultConfigPath(), "config file")
	var noDirect = flags.Bool("no-direct", false, "do not bypass a local TUN proxy")
	_ = flags.Parse(args)
	agent.SetDirect(!*noDirect)
	fmt.Printf("edge-probe %s (%s)\nconfig: %s\n", agent.Version, agent.Platform(), *configPath)
	config, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	fmt.Printf("agent %d, server %s\n", config.AgentID, config.Server)
	var runner = agent.NewRunner(config, *configPath)
	runner.Updater = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = runner.SyncOnce(ctx); err != nil {
		return fmt.Errorf("cannot reach the platform: %w", err)
	}
	fmt.Println("platform: ok")
	runner.MarkHTTPDue()
	runner.ProbeOnce(ctx)
	for _, line := range runner.Summary() {
		fmt.Println(line)
	}
	for _, line := range runner.ResolveSummary(ctx) {
		fmt.Println(line)
	}
	return nil
}
