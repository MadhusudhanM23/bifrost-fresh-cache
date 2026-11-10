// Package envinfo records where a measurement ran: client machine, Go, clock,
// Docker, and a fingerprint of the Bifrost source under test. Every probe is
// best effort; what cannot be read is recorded as unavailable, never guessed.
package envinfo

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"bifrost-eval-harness/internal/clock"
)

// Env is the recorded environment.
type Env struct {
	CapturedAt        string            `json:"captured_at"`
	ClientLocation    string            `json:"client_location"`
	OS                string            `json:"os"`
	Kernel            string            `json:"kernel,omitempty"`
	CPUModel          string            `json:"cpu_model"`
	LogicalCPUs       int               `json:"logical_cpus"`
	MemoryBytes       uint64            `json:"memory_bytes,omitempty"`
	GoVersion         string            `json:"go_version"`
	GOOS              string            `json:"goos"`
	GOARCH            string            `json:"goarch"`
	ClockSource       string            `json:"latency_clock"`
	ClockResolutionNs float64           `json:"latency_clock_median_step_ns"`
	Docker            map[string]any    `json:"docker"`
	CodeUnderTest     map[string]any    `json:"code_under_test"`
	Notes             map[string]string `json:"notes,omitempty"`
}

// Options say which containers to describe and where the Bifrost tree is.
type Options struct {
	Containers []string
	// ContainerImages supplies image names when the docker CLI is not available
	// (for example when the harness itself runs inside a container).
	ContainerImages map[string]string
	BifrostDir      string
	Notes           map[string]string
}

// Collect gathers the environment.
func Collect(opts Options) Env {
	step, source := clock.Resolution()
	env := Env{
		CapturedAt:        time.Now().UTC().Format(time.RFC3339),
		ClientLocation:    clientLocation(),
		LogicalCPUs:       runtime.NumCPU(),
		GoVersion:         runtime.Version(),
		GOOS:              runtime.GOOS,
		GOARCH:            runtime.GOARCH,
		ClockSource:       source,
		ClockResolutionNs: float64(step.Nanoseconds()),
		Notes:             opts.Notes,
	}
	switch runtime.GOOS {
	case "windows":
		env.CPUModel = powershell(`(Get-CimInstance Win32_Processor | Select-Object -First 1).Name`)
		env.OS = powershell(`$o = Get-CimInstance Win32_OperatingSystem; "$($o.Caption) $($o.Version) build $($o.BuildNumber)"`)
		if mem, err := strconv.ParseUint(powershell(`(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory`), 10, 64); err == nil {
			env.MemoryBytes = mem
		}
	case "linux":
		env.CPUModel = procField("/proc/cpuinfo", "model name")
		env.OS = osRelease()
		env.Kernel = strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))
		if kb, err := strconv.ParseUint(strings.Fields(procField("/proc/meminfo", "MemTotal") + " 0")[0], 10, 64); err == nil {
			env.MemoryBytes = kb * 1024
		}
	case "darwin":
		env.CPUModel = run("sysctl", "-n", "machdep.cpu.brand_string")
		env.OS = "macOS " + run("sw_vers", "-productVersion")
	}
	if env.CPUModel == "" {
		env.CPUModel = "unavailable"
	}
	env.Docker = dockerInfo(opts)
	env.CodeUnderTest = codeUnderTest(opts.BifrostDir)
	return env
}

func clientLocation() string {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			return "docker container (linux, on the Docker Desktop VM)"
		}
	}
	return runtime.GOOS + " host"
}

func dockerInfo(opts Options) map[string]any {
	out := map[string]any{}
	if _, err := exec.LookPath("docker"); err != nil {
		out["cli"] = "unavailable (no docker CLI where the harness ran)"
		images := map[string]string{}
		for name, image := range opts.ContainerImages {
			images[name] = image + " (supplied with --container-image)"
		}
		out["containers"] = images
		return out
	}
	out["server_version"] = run("docker", "version", "--format", "{{.Server.Version}}")
	out["engine"] = run("docker", "info", "--format", "os={{.OperatingSystem}} kernel={{.KernelVersion}} ncpu={{.NCPU}} mem_bytes={{.MemTotal}}")
	containers := map[string]string{}
	for _, name := range opts.Containers {
		if v := run("docker", "inspect", "--format", "{{.Config.Image}} image_id={{.Image}} limits: nano_cpus={{.HostConfig.NanoCpus}} memory={{.HostConfig.Memory}}", name); v != "" {
			containers[name] = v
		} else {
			containers[name] = "not found"
		}
	}
	out["containers"] = containers
	return out
}

// BuildFingerprint is stamped in by scripts/build.ps1 (-ldflags -X): the
// SHA-256 prefixes of the vectorstore sources the binary was compiled from,
// as "file=hash,file=hash". Empty for `go run` builds.
var BuildFingerprint string

var fingerprintFiles = []string{"store.go", "pgvector.go", "qdrant.go", "redis.go", "utils.go", "errors.go"}

// codeUnderTest fingerprints the vectorstore sources. The build-time
// fingerprint (when the binary was built by scripts/build.ps1) says what was
// compiled; the run-time hashes say what is on disk now. If they differ,
// someone edited the files after the build and the run-time hashes do not
// describe the code that ran.
func codeUnderTest(dir string) map[string]any {
	out := map[string]any{}
	built := map[string]string{}
	if BuildFingerprint != "" {
		for _, part := range strings.Split(BuildFingerprint, ",") {
			if name, hash, ok := strings.Cut(part, "="); ok {
				built[name] = hash
			}
		}
		out["build_fingerprint"] = built
	} else {
		out["build_fingerprint"] = "none (not built by scripts/build.ps1; go run compiles the tree as it is at start-up)"
	}
	if dir == "" {
		out["status"] = "no --bifrost-dir given"
		return out
	}
	out["bifrost_dir"] = dir
	if commit := gitCommit(dir); commit != "" {
		out["git_head"] = commit
	}
	files := map[string]string{}
	matches := len(built) > 0
	for _, name := range fingerprintFiles {
		path := filepath.Join(dir, "framework", "vectorstore", name)
		data, err := os.ReadFile(path)
		if err != nil {
			files[name] = "unreadable: " + err.Error()
			matches = false
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:8])
		stat, _ := os.Stat(path)
		files[name] = "sha256:" + hash + " modified:" + stat.ModTime().UTC().Format(time.RFC3339)
		if built[name] != hash {
			matches = false
		}
	}
	out["framework/vectorstore"] = files
	if len(built) > 0 {
		out["build_matches_disk"] = matches
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var deps []string
		for _, dep := range info.Deps {
			if strings.HasPrefix(dep.Path, "github.com/maximhq/bifrost/") {
				entry := dep.Path + " " + dep.Version
				if dep.Replace != nil {
					entry += " => " + dep.Replace.Path
				}
				deps = append(deps, entry)
			}
		}
		out["build_info_bifrost_modules"] = deps
	}
	if status := run("git", "-C", dir, "status", "--porcelain", "--", "framework/vectorstore"); status != "" {
		out["uncommitted_vectorstore_changes"] = strings.Split(status, "\n")
	}
	return out
}

// gitCommit reads HEAD without the git binary, so it also works in a container.
func gitCommit(dir string) string {
	head := strings.TrimSpace(readFile(filepath.Join(dir, ".git", "HEAD")))
	ref, ok := strings.CutPrefix(head, "ref: ")
	if !ok {
		return head
	}
	if commit := strings.TrimSpace(readFile(filepath.Join(dir, ".git", filepath.FromSlash(ref)))); commit != "" {
		return commit + " (" + ref + ")"
	}
	scanner := bufio.NewScanner(strings.NewReader(readFile(filepath.Join(dir, ".git", "packed-refs"))))
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 && fields[1] == ref {
			return fields[0] + " (" + ref + ")"
		}
	}
	return ""
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func procField(path, key string) string {
	scanner := bufio.NewScanner(strings.NewReader(readFile(path)))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "kB"))
		}
	}
	return ""
}

func osRelease() string {
	scanner := bufio.NewScanner(strings.NewReader(readFile("/etc/os-release")))
	for scanner.Scan() {
		if v, ok := strings.CutPrefix(scanner.Text(), "PRETTY_NAME="); ok {
			return "linux " + strings.Trim(v, `"`)
		}
	}
	return "linux"
}

func powershell(command string) string {
	return run("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

func run(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
