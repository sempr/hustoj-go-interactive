package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Result 表示 judge 向 controller 汇报的结果
type Result struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// 最低权限 nobody
const nobodyUID = 65534
const nobodyGID = 65534

// SandboxConfig 保存命令和 rootfs 路径
type SandboxConfig struct {
	JudgeRootfs  string
	JudgeCmd     string
	PlayerRootfs string
	PlayerCmd    string
	TimeoutMS    int
}

// helper
func must(err error) {
	if err != nil {
		panic(err)
		log.Fatal(err)
	}
}

// CgroupStats holds resource usage statistics
type CgroupStats struct {
	MemoryPeakBytes uint64
	CPUUsageUser    uint64
	CPUUsageSystem  uint64
}

// createCgroup creates a cgroup v2 directory and sets limits
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

// addProcessToCgroup adds a process to a cgroup
func addProcessToCgroup(cgroupPath string, pid int) {
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[CGROUP] Failed to add PID %d to cgroup %s: %v\n", pid, cgroupPath, err)
	} else {
		fmt.Fprintf(os.Stderr, "[CGROUP] Successfully added PID %d to cgroup %s\n", pid, cgroupPath)
	}
}

// getCgroupStats reads resource usage from cgroup
func getCgroupStats(cgroupPath string) CgroupStats {
	stats := CgroupStats{}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.peak")); err == nil {
		peakStr := strings.TrimSpace(string(data))
		fmt.Fprintf(os.Stderr, "[CGROUP] memory.peak: %s\n", peakStr)
		fmt.Sscanf(peakStr, "%d", &stats.MemoryPeakBytes)
	} else {
		fmt.Fprintf(os.Stderr, "[CGROUP] Failed to read memory.peak: %v\n", err)
	}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current")); err == nil {
		currentStr := strings.TrimSpace(string(data))
		fmt.Fprintf(os.Stderr, "[CGROUP] memory.current: %s\n", currentStr)
	}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.stat")); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				if val, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					if fields[0] == "file" && val > stats.MemoryPeakBytes {
						stats.MemoryPeakBytes = val
					}
					if fields[0] == "anon" && val > stats.MemoryPeakBytes {
						stats.MemoryPeakBytes = val
					}
					if fields[0] == "rss" && val > stats.MemoryPeakBytes {
						stats.MemoryPeakBytes = val
					}
					if fields[0] == "shmem" && val > stats.MemoryPeakBytes {
						stats.MemoryPeakBytes = val
					}
				}
			}
		}
		fmt.Fprintf(os.Stderr, "[CGROUP] memory.stat total from rss/anon/file/shmem: %d bytes\n", stats.MemoryPeakBytes)
	}

	if data, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.stat")); err == nil {
		fmt.Fprintf(os.Stderr, "[CGROUP] cpu.stat: %s\n", string(data))
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

// deleteCgroup removes a cgroup directory
func deleteCgroup(cgroupPath string) {
	_ = os.RemoveAll(cgroupPath)
}

// 解析 CLI 参数
func parseArgs() SandboxConfig {
	cfg := SandboxConfig{}
	flag.StringVar(&cfg.JudgeRootfs, "judge-rootfs", "", "judge rootfs path")
	flag.StringVar(&cfg.JudgeCmd, "judge-cmd", "/bin/judge", "judge command path")
	flag.StringVar(&cfg.PlayerRootfs, "player-rootfs", "", "player rootfs path")
	flag.StringVar(&cfg.PlayerCmd, "player-cmd", "/bin/player", "player command path")
	flag.IntVar(&cfg.TimeoutMS, "timeout", 5000, "timeout in milliseconds")
	flag.Parse()

	if cfg.JudgeRootfs == "" || cfg.PlayerRootfs == "" {
		log.Fatal("must provide rootfs paths")
	}
	return cfg
}

// 在子进程中执行 pivot_root + mount /proc + setuid
func childInit(rootfs string) {
	fmt.Fprintf(os.Stderr, "[SANDBOX] === childInit STARTED ===\n")
	fmt.Fprintf(os.Stderr, "[SANDBOX] PID: %d, PPID: %d\n", os.Getpid(), os.Getppid())
	fmt.Fprintf(os.Stderr, "[SANDBOX] UID: %d, GID: %d\n", os.Getuid(), os.Getgid())
	fmt.Fprintf(os.Stderr, "[SANDBOX] Rootfs: %s\n", rootfs)

	// mount namespace 私有化
	fmt.Fprintf(os.Stderr, "[SANDBOX] Creating private mount namespace...\n")
	must(syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""))
	fmt.Fprintf(os.Stderr, "[SANDBOX] Private mount namespace created\n")

	// bind mount rootfs
	fmt.Fprintf(os.Stderr, "[SANDBOX] Bind mounting rootfs: %s\n", rootfs)
	must(syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""))
	fmt.Fprintf(os.Stderr, "[SANDBOX] Rootfs bind mounted\n")

	os.Mkdir(rootfs+"/proc", 0755)
	// 创建 old_root 目录用于 pivot_root
	oldRoot := rootfs + "/old_root"
	fmt.Fprintf(os.Stderr, "[SANDBOX] Creating old_root directory: %s\n", oldRoot)
	must(os.Mkdir(oldRoot, 0755))
	fmt.Fprintf(os.Stderr, "[SANDBOX] old_root directory created\n")

	// pivot_root
	fmt.Fprintf(os.Stderr, "[SANDBOX] Executing pivot_root...\n")
	must(syscall.PivotRoot(rootfs, oldRoot))
	fmt.Fprintf(os.Stderr, "[SANDBOX] pivot_root completed\n")

	fmt.Fprintf(os.Stderr, "[SANDBOX] Changing directory to /...\n")
	must(os.Chdir("/"))
	fmt.Fprintf(os.Stderr, "[SANDBOX] Current directory changed to /\n")

	// 卸载 old_root
	fmt.Fprintf(os.Stderr, "[SANDBOX] Unmounting /old_root...\n")
	must(syscall.Unmount("/old_root", syscall.MNT_DETACH))
	_ = os.RemoveAll("/old_root")
	fmt.Fprintf(os.Stderr, "[SANDBOX] old_root unmounted and removed\n")

	// 挂载 /proc (使用 bind mount，必须在切换到 nobody 之前执行)
	fmt.Fprintf(os.Stderr, "[SANDBOX] Mounting /proc via bind...\n")
	err := syscall.Mount("/proc", "/proc", "", syscall.MS_BIND|syscall.MS_REC|syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[SANDBOX] Failed to mount /proc: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[SANDBOX] /proc mounted successfully\n")
	}

	// 切换到 nobody (在 user namespace 中已经是非特权)
	fmt.Fprintf(os.Stderr, "[SANDBOX] Switching to nobody (UID=%d, GID=%d)...\n", nobodyUID, nobodyGID)
	must(syscall.Setgid(nobodyGID))
	must(syscall.Setuid(nobodyUID))
	fmt.Fprintf(os.Stderr, "[SANDBOX] UID/GID switched to nobody\n")
	fmt.Fprintf(os.Stderr, "[SANDBOX] Final UID: %d, GID: %d\n", os.Getuid(), os.Getgid())

	fmt.Fprintf(os.Stderr, "[SANDBOX] === childInit COMPLETED ===\n")
}

// 根据 SANDBOX_ENV 判断是否是 sandbox 子进程
func maybeSandboxInit() {
	fmt.Fprintf(os.Stderr, "[CHECK] Checking SANDBOX_INIT: %s\n", os.Getenv("SANDBOX_INIT"))

	if os.Getenv("SANDBOX_INIT") != "1" {
		fmt.Fprintf(os.Stderr, "[CHECK] Not in sandbox mode, returning to controller\n")
		return
	}

	fmt.Fprintf(os.Stderr, "[CHECK] Detected sandbox environment!\n")
	rootfs := os.Getenv("SANDBOX_ROOTFS")
	target := os.Getenv("SANDBOX_TARGET")
	fmt.Fprintf(os.Stderr, "[CHECK] SANDBOX_ROOTFS=%s\n", rootfs)
	fmt.Fprintf(os.Stderr, "[CHECK] SANDBOX_TARGET=%s\n", target)

	if rootfs == "" {
		panic("SANDBOX_ROOTFS missing")
	}

	if target == "" {
		panic("SANDBOX_TARGET missing")
	}

	fmt.Fprintf(os.Stderr, "[CHECK] About to call childInit...\n")
	childInit(rootfs)
	fmt.Fprintf(os.Stderr, "[CHECK] childInit returned, sandbox setup complete\n")

	fmt.Fprintf(os.Stderr, "[CHECK] Preparing to exec into target: %s\n", target)

	// Clean environment before exec
	os.Unsetenv("SANDBOX_INIT")
	os.Unsetenv("SANDBOX_ROOTFS")
	os.Unsetenv("SANDBOX_TARGET")

	fmt.Fprintf(os.Stderr, "[CHECK] Environment cleaned, calling syscall.Exec...\n")

	// syscall.Exec replaces this process with the target binary
	// Process keeps all the sandbox setup (namespaces, mounts, UID/GID)
	// but runs the target binary's code instead
	must(syscall.Exec(target, []string{target}, os.Environ()))

	// This line never reached because process is replaced
	panic("syscall.Exec returned unexpectedly!")
}

// spawnSandbox 创建一个命令在独立 namespace 下运行
func spawnSandbox(cmdPath, rootfs string, cgroupPath string, stdin, stdout *os.File, extraFiles []*os.File) (*exec.Cmd, error) {
	// Get path to this controller binary (we'll exec ourselves first)
	selfPath, err := os.Executable()
	must(err)

	// Start THIS binary with sandbox setup mode
	cmd := exec.Command(selfPath)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	if len(extraFiles) > 0 {
		cmd.ExtraFiles = extraFiles
	}

	// Create namespaces
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

	// Set environment to trigger sandbox setup mode
	cmd.Env = append(os.Environ(),
		"SANDBOX_INIT=1",
		"SANDBOX_ROOTFS="+rootfs,
		"SANDBOX_TARGET="+cmdPath,
	)
	fmt.Fprintf(os.Stderr, "[SPAWN] Starting sandbox setup with controller binary\n")
	fmt.Fprintf(os.Stderr, "[SPAWN] Environment: SANDBOX_INIT=1, ROOTFS=%s, TARGET=%s\n", rootfs, rootfs+cmdPath)

	err = cmd.Start()
	fmt.Fprintf(os.Stderr, "[SPAWN] Process started, PID: %d\n", cmd.Process.Pid)

	if cgroupPath != "" {
		fmt.Fprintf(os.Stderr, "[SPAWN] Adding PID %d to cgroup %s\n", cmd.Process.Pid, cgroupPath)
		addProcessToCgroup(cgroupPath, cmd.Process.Pid)

		// Verify the process was added
		if procs, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs")); err == nil {
			fmt.Fprintf(os.Stderr, "[SPAWN] cgroup.procs after add: %s", string(procs))
		}
	}

	return cmd, err
}

func main() {
	fmt.Fprintf(os.Stderr, "[MAIN] Starting program, PID: %d, PPID: %d\n", os.Getpid(), os.Getppid())
	fmt.Fprintf(os.Stderr, "[MAIN] Current working dir: %s\n", func() string { wd, _ := os.Getwd(); return wd }())

	maybeSandboxInit()

	cfg := parseArgs()
	fmt.Fprintf(os.Stderr, "[MAIN] Parsed config, continuing as controller\n")

	// Create cgroups
	judgeCgroup := createCgroup("guess_judge", "100", "100000 1000000")
	playerCgroup := createCgroup("guess_player", "100", "100000 1000000")
	defer deleteCgroup(judgeCgroup)
	defer deleteCgroup(playerCgroup)

	fmt.Fprintf(os.Stderr, "[CGROUP] Judge cgroup: %s\n", judgeCgroup)
	fmt.Fprintf(os.Stderr, "[CGROUP] Player cgroup: %s\n", playerCgroup)

	// Judge -> Player pipes
	jToP_R, jToP_W, _ := os.Pipe()
	pToJ_R, pToJ_W, _ := os.Pipe()

	// Judge -> Controller (fd=3)
	reportR, reportW, _ := os.Pipe()

	// spawn player
	playerCmd, err := spawnSandbox(cfg.PlayerCmd, cfg.PlayerRootfs, playerCgroup, jToP_R, pToJ_W, nil)
	must(err)
	// spawn judge
	judgeCmd, err := spawnSandbox(cfg.JudgeCmd, cfg.JudgeRootfs, judgeCgroup, pToJ_R, jToP_W, []*os.File{reportW})
	must(err)

	timeout := time.After(time.Duration(cfg.TimeoutMS) * time.Millisecond)
	resultCh := make(chan string, 1)

	// Monitor memory usage periodically
	done := make(chan bool)
	var maxJudgeMem, maxPlayerMem uint64
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				fmt.Fprintf(os.Stderr, "[MONITOR] Sampling stopped. maxJudgeMem=%d, maxPlayerMem=%d\n", maxJudgeMem, maxPlayerMem)
				return
			case <-ticker.C:
				if data, err := os.ReadFile(filepath.Join(judgeCgroup, "memory.current")); err == nil {
					var val uint64
					fmt.Sscanf(string(data), "%d", &val)
					if val > maxJudgeMem {
						maxJudgeMem = val
						fmt.Fprintf(os.Stderr, "[MONITOR] Judge memory.current: %d (new peak)\n", val)
					}
				}
				if data, err := os.ReadFile(filepath.Join(playerCgroup, "memory.current")); err == nil {
					var val uint64
					fmt.Sscanf(string(data), "%d", &val)
					if val > maxPlayerMem {
						maxPlayerMem = val
						fmt.Fprintf(os.Stderr, "[MONITOR] Player memory.current: %d (new peak)\n", val)
					}
				}
			}
		}
	}()

	// 读取 judge 的 fd=3
	go func() {
		reader := bufio.NewReader(reportR)
		line, _ := reader.ReadString('\n')
		resultCh <- strings.TrimSpace(line)
	}()

	select {
	case res := <-resultCh:
		fmt.Println("[controller] result:", res)
	case <-timeout:
		fmt.Println("[controller] timeout")
		_ = judgeCmd.Process.Kill()
		_ = playerCmd.Process.Kill()
	}

	judgeCmd.Wait()
	playerCmd.Wait()
	close(done)
	time.Sleep(100 * time.Millisecond)

	// Print cgroup statistics
	judgeStats := getCgroupStats(judgeCgroup)
	playerStats := getCgroupStats(playerCgroup)

	if maxJudgeMem > judgeStats.MemoryPeakBytes {
		judgeStats.MemoryPeakBytes = maxJudgeMem
	}
	if maxPlayerMem > playerStats.MemoryPeakBytes {
		playerStats.MemoryPeakBytes = maxPlayerMem
	}

	fmt.Printf("\n[CGROUP STATS] Judge:\n")
	fmt.Printf("  Memory Peak: %.2f MB\n", float64(judgeStats.MemoryPeakBytes)/1024/1024)
	fmt.Printf("  CPU Usage: user=%.2f ms, system=%.2f ms\n", float64(judgeStats.CPUUsageUser)/1000, float64(judgeStats.CPUUsageSystem)/1000)

	fmt.Printf("\n[CGROUP STATS] Player:\n")
	fmt.Printf("  Memory Peak: %.2f MB\n", float64(playerStats.MemoryPeakBytes)/1024/1024)
	fmt.Printf("  CPU Usage: user=%.2f ms, system=%.2f ms\n", float64(playerStats.CPUUsageUser)/1000, float64(playerStats.CPUUsageSystem)/1000)

	// Debug: check cgroup.procs
	if judgeProcs, err := os.ReadFile(filepath.Join(judgeCgroup, "cgroup.procs")); err == nil {
		fmt.Fprintf(os.Stderr, "[CGROUP] Judge cgroup.procs: %s", string(judgeProcs))
	}
	if playerProcs, err := os.ReadFile(filepath.Join(playerCgroup, "cgroup.procs")); err == nil {
		fmt.Fprintf(os.Stderr, "[CGROUP] Player cgroup.procs: %s", string(playerProcs))
	}

	// Debug: list cgroup files
	files, _ := os.ReadDir(judgeCgroup)
	fmt.Fprintf(os.Stderr, "[CGROUP] Judge cgroup files: ")
	for _, f := range files {
		fmt.Fprintf(os.Stderr, "%s ", f.Name())
	}
	fmt.Fprintf(os.Stderr, "\n")
}
