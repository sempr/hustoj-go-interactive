package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	nobodyUID = 65534
	nobodyGID = 65534
)

type SandboxConfig struct {
	JudgeRootfs  string
	JudgeCmd     string
	PlayerRootfs string
	PlayerCmd    string
	TimeoutMS    int
}

type CgroupStats struct {
	MemoryPeakBytes uint64
	CPUUsageUser    uint64
	CPUUsageSystem  uint64
}

type CgroupSetup struct {
	JudgePath  string
	PlayerPath string
}

type PipeSetup struct {
	JudgeToPlayerRead  *os.File
	JudgeToPlayerWrite *os.File
	PlayerToJudgeRead  *os.File
	PlayerToJudgeWrite *os.File
	ReportRead         *os.File
	ReportWrite        *os.File
}

type MonitoringState struct {
	Done         chan bool
	MaxJudgeMem  uint64
	MaxPlayerMem uint64
}

func must(err error) {
	if err != nil {
		slog.Error("fatal error", "error", err)
		panic(err)
	}
}

func parseArgs() SandboxConfig {
	cfg := SandboxConfig{}
	flag.StringVar(&cfg.JudgeRootfs, "judge-rootfs", "", "judge rootfs path")
	flag.StringVar(&cfg.JudgeCmd, "judge-cmd", "/bin/judge", "judge command path")
	flag.StringVar(&cfg.PlayerRootfs, "player-rootfs", "", "player rootfs path")
	flag.StringVar(&cfg.PlayerCmd, "player-cmd", "/bin/player", "player command path")
	flag.IntVar(&cfg.TimeoutMS, "timeout", 5000, "timeout in milliseconds")
	flag.Parse()

	if cfg.JudgeRootfs == "" || cfg.PlayerRootfs == "" {
		slog.Error("must provide rootfs paths")
		os.Exit(1)
	}
	return cfg
}

func createCgroup(name string, memoryLimitMB, cpuMax string) string {
	cgroupPath := filepath.Join("/sys/fs/cgroup", name)
	must(os.MkdirAll(cgroupPath, 0755))

	if memoryLimitMB != "" {
		memFile := filepath.Join(cgroupPath, "memory.max")
		must(os.WriteFile(memFile, []byte(memoryLimitMB+"M"), 0644))
	}

	if cpuMax != "" {
		cpuFile := filepath.Join(cgroupPath, "cpu.max")
		must(os.WriteFile(cpuFile, []byte(cpuMax), 0644))
	}

	return cgroupPath
}

func deleteCgroup(cgroupPath string) {
	_ = os.RemoveAll(cgroupPath)
}

func setupCgroups() CgroupSetup {
	judgeCgroup := createCgroup("guess_judge", "100", "100000 1000000")
	playerCgroup := createCgroup("guess_player", "100", "100000 1000000")

	slog.Info("cgroup setup", "judge", judgeCgroup, "player", playerCgroup)

	return CgroupSetup{
		JudgePath:  judgeCgroup,
		PlayerPath: playerCgroup,
	}
}

func setupPipes() PipeSetup {
	jToP_R, jToP_W, _ := os.Pipe()
	pToJ_R, pToJ_W, _ := os.Pipe()
	reportR, reportW, _ := os.Pipe()

	return PipeSetup{
		JudgeToPlayerRead:  jToP_R,
		JudgeToPlayerWrite: jToP_W,
		PlayerToJudgeRead:  pToJ_R,
		PlayerToJudgeWrite: pToJ_W,
		ReportRead:         reportR,
		ReportWrite:        reportW,
	}
}

func addProcessToCgroup(cgroupPath string, pid int) {
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644)
	if err != nil {
		slog.Error("failed to add process to cgroup", "pid", pid, "cgroup", cgroupPath, "error", err)
	} else {
		slog.Info("added process to cgroup", "pid", pid, "cgroup", cgroupPath)
	}
}

func spawnSandbox(cmdPath, rootfs, cgroupPath string, stdin, stdout *os.File, extraFiles []*os.File) *exec.Cmd {
	selfPath, err := os.Executable()
	must(err)

	cmd := exec.Command(selfPath)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	if len(extraFiles) > 0 {
		cmd.ExtraFiles = extraFiles
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
			{ContainerID: nobodyUID, HostID: nobodyUID, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
			{ContainerID: nobodyGID, HostID: nobodyGID, Size: 1},
		},
	}

	cmd.Env = append(os.Environ(),
		"SANDBOX_INIT=1",
		"SANDBOX_ROOTFS="+rootfs,
		"SANDBOX_TARGET="+cmdPath,
	)

	slog.Info("starting sandbox setup", "rootfs", rootfs, "target", cmdPath)

	err = cmd.Start()
	must(err)

	slog.Info("process started", "pid", cmd.Process.Pid)

	if cgroupPath != "" {
		slog.Info("adding process to cgroup", "pid", cmd.Process.Pid, "cgroup", cgroupPath)
		addProcessToCgroup(cgroupPath, cmd.Process.Pid)
		if procs, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs")); err == nil {
			slog.Debug("cgroup.procs after add", "procs", string(procs))
		}
	}

	return cmd
}

func startMonitoring(judgeCgroup, playerCgroup string) MonitoringState {
	done := make(chan bool)
	state := MonitoringState{
		Done:         done,
		MaxJudgeMem:  0,
		MaxPlayerMem: 0,
	}

	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				slog.Info("monitoring stopped", "maxJudgeMem", state.MaxJudgeMem, "maxPlayerMem", state.MaxPlayerMem)
				return
			case <-ticker.C:
				if data, err := os.ReadFile(filepath.Join(judgeCgroup, "memory.current")); err == nil {
					var val uint64
					fmt.Sscanf(string(data), "%d", &val)
					if val > state.MaxJudgeMem {
						state.MaxJudgeMem = val
						slog.Debug("judge memory current (new peak)", "bytes", val)
					}
				}
				if data, err := os.ReadFile(filepath.Join(playerCgroup, "memory.current")); err == nil {
					var val uint64
					fmt.Sscanf(string(data), "%d", &val)
					if val > state.MaxPlayerMem {
						state.MaxPlayerMem = val
						slog.Debug("player memory current (new peak)", "bytes", val)
					}
				}
			}
		}
	}()

	return state
}

func waitForResult(reportR *os.File, timeout time.Duration) string {
	resultCh := make(chan string, 1)

	go func() {
		reader := bufio.NewReader(reportR)
		line, _ := reader.ReadString('\n')
		resultCh <- strings.TrimSpace(line)
	}()

	select {
	case res := <-resultCh:
		return res
	case <-time.After(timeout):
		return "timeout"
	}
}

func readMemoryStat(cgroupPath string) uint64 {
	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.stat")); err == nil {
		lines := strings.Split(string(data), "\n")
		var maxVal uint64
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				if val, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					switch fields[0] {
					case "file", "anon", "rss", "shmem":
						if val > maxVal {
							maxVal = val
						}
					}
				}
			}
		}
		return maxVal
	}
	return 0
}

func getCgroupStats(cgroupPath string) CgroupStats {
	stats := CgroupStats{}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.peak")); err == nil {
		peakStr := strings.TrimSpace(string(data))
		slog.Debug("memory.peak", "value", peakStr)
		fmt.Sscanf(peakStr, "%d", &stats.MemoryPeakBytes)
	} else {
		slog.Error("failed to read memory.peak", "cgroup", cgroupPath, "error", err)
	}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current")); err == nil {
		slog.Debug("memory.current", "value", strings.TrimSpace(string(data)))
	}

	statMem := readMemoryStat(cgroupPath)
	if statMem > stats.MemoryPeakBytes {
		stats.MemoryPeakBytes = statMem
	}
	slog.Debug("memory.stat total", "bytes", stats.MemoryPeakBytes)

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.stat")); err == nil {
		slog.Debug("cpu.stat", "content", string(data))
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				switch fields[0] {
				case "user_usec":
					stats.CPUUsageUser, _ = strconv.ParseUint(fields[1], 10, 64)
				case "system_usec":
					stats.CPUUsageSystem, _ = strconv.ParseUint(fields[1], 10, 64)
				}
			}
		}
	}

	return stats
}

func printStats(name string, stats CgroupStats) {
	fmt.Printf("\n[CGROUP STATS] %s:\n", name)
	fmt.Printf("  Memory Peak: %.2f MB\n", float64(stats.MemoryPeakBytes)/1024/1024)
	fmt.Printf("  CPU Usage: user=%.2f ms, system=%.2f ms\n", float64(stats.CPUUsageUser)/1000, float64(stats.CPUUsageSystem)/1000)
}

func collectAndPrintStats(judgeCgroup, playerCgroup string, state MonitoringState) {
	judgeStats := getCgroupStats(judgeCgroup)
	playerStats := getCgroupStats(playerCgroup)

	if state.MaxJudgeMem > judgeStats.MemoryPeakBytes {
		judgeStats.MemoryPeakBytes = state.MaxJudgeMem
	}
	if state.MaxPlayerMem > playerStats.MemoryPeakBytes {
		playerStats.MemoryPeakBytes = state.MaxPlayerMem
	}

	printStats("Judge", judgeStats)
	printStats("Player", playerStats)

	if judgeProcs, err := os.ReadFile(filepath.Join(judgeCgroup, "cgroup.procs")); err == nil {
		slog.Debug("judge cgroup.procs", "content", string(judgeProcs))
	}
	if playerProcs, err := os.ReadFile(filepath.Join(playerCgroup, "cgroup.procs")); err == nil {
		slog.Debug("player cgroup.procs", "content", string(playerProcs))
	}

	files, _ := os.ReadDir(judgeCgroup)
	fileNames := make([]string, len(files))
	for i, f := range files {
		fileNames[i] = f.Name()
	}
	slog.Debug("judge cgroup files", "files", fileNames)
}

func childInit(rootfs string) {
	slog.Info("childInit started", "pid", os.Getpid(), "ppid", os.Getppid(), "uid", os.Getuid(), "gid", os.Getgid(), "rootfs", rootfs)

	slog.Debug("creating private mount namespace")
	must(syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""))

	slog.Debug("bind mounting rootfs", "rootfs", rootfs)
	must(syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""))

	os.Mkdir(rootfs+"/proc", 0755)

	oldRoot := rootfs + "/old_root"
	slog.Debug("creating old_root directory", "path", oldRoot)
	must(os.Mkdir(oldRoot, 0755))

	slog.Debug("executing pivot_root")
	must(syscall.PivotRoot(rootfs, oldRoot))

	slog.Debug("changing directory to /")
	must(os.Chdir("/"))

	slog.Debug("unmounting /old_root")
	must(syscall.Unmount("/old_root", syscall.MNT_DETACH))
	_ = os.RemoveAll("/old_root")

	slog.Debug("mounting /proc via bind")
	err := syscall.Mount("/proc", "/proc", "", syscall.MS_BIND|syscall.MS_REC|syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "")
	if err != nil {
		slog.Error("failed to mount /proc", "error", err)
	} else {
		slog.Debug("/proc mounted successfully")
	}

	slog.Debug("switching to nobody", "uid", nobodyUID, "gid", nobodyGID)
	must(syscall.Setgid(nobodyGID))
	must(syscall.Setuid(nobodyUID))

	slog.Info("childInit completed")
}

func maybeSandboxInit() {
	sandboxInit := os.Getenv("SANDBOX_INIT")
	slog.Debug("checking SANDBOX_INIT", "value", sandboxInit)

	if sandboxInit != "1" {
		slog.Info("not in sandbox mode, returning to controller")
		return
	}

	slog.Info("detected sandbox environment")
	rootfs := os.Getenv("SANDBOX_ROOTFS")
	target := os.Getenv("SANDBOX_TARGET")
	slog.Info("sandbox env vars", "rootfs", rootfs, "target", target)

	if rootfs == "" {
		slog.Error("SANDBOX_ROOTFS missing")
		panic("SANDBOX_ROOTFS missing")
	}

	if target == "" {
		slog.Error("SANDBOX_TARGET missing")
		panic("SANDBOX_TARGET missing")
	}

	slog.Debug("about to call childInit")
	childInit(rootfs)

	os.Unsetenv("SANDBOX_INIT")
	os.Unsetenv("SANDBOX_ROOTFS")
	os.Unsetenv("SANDBOX_TARGET")

	slog.Debug("calling syscall.Exec")
	must(syscall.Exec(target, []string{target}, os.Environ()))
	panic("syscall.Exec returned unexpectedly!")
}

func runGame(cfg SandboxConfig, cgroupSetup CgroupSetup, pipeSetup PipeSetup) string {
	playerCmd := spawnSandbox(cfg.PlayerCmd, cfg.PlayerRootfs, cgroupSetup.PlayerPath, pipeSetup.JudgeToPlayerRead, pipeSetup.PlayerToJudgeWrite, nil)
	judgeCmd := spawnSandbox(cfg.JudgeCmd, cfg.JudgeRootfs, cgroupSetup.JudgePath, pipeSetup.PlayerToJudgeRead, pipeSetup.JudgeToPlayerWrite, []*os.File{pipeSetup.ReportWrite})

	monitoring := startMonitoring(cgroupSetup.JudgePath, cgroupSetup.PlayerPath)
	result := waitForResult(pipeSetup.ReportRead, time.Duration(cfg.TimeoutMS)*time.Millisecond)

	if result == "timeout" {
		fmt.Println("[controller] timeout")
		judgeCmd.Process.Kill()
		playerCmd.Process.Kill()
	} else {
		fmt.Println("[controller] result:", result)
	}

	judgeCmd.Wait()
	playerCmd.Wait()
	close(monitoring.Done)
	time.Sleep(100 * time.Millisecond)

	collectAndPrintStats(cgroupSetup.JudgePath, cgroupSetup.PlayerPath, monitoring)

	return result
}

func main() {
	wd, _ := os.Getwd()
	slog.Info("starting program", "pid", os.Getpid(), "ppid", os.Getppid(), "cwd", wd)

	maybeSandboxInit()

	cfg := parseArgs()
	slog.Info("parsed config, continuing as controller")

	cgroupSetup := setupCgroups()
	defer deleteCgroup(cgroupSetup.JudgePath)
	defer deleteCgroup(cgroupSetup.PlayerPath)

	pipeSetup := setupPipes()

	runGame(cfg, cgroupSetup, pipeSetup)
}
