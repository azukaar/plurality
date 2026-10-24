package ai

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// LiteLLMBaseURL is the base URL for the LiteLLM proxy.
// Set via LITELLM_URL env var (Docker) or defaults to http://127.0.0.1:4000.
var LiteLLMBaseURL = "http://127.0.0.1:4000"

var litellmProcess *exec.Cmd
var litellmMu sync.Mutex
var litellmReady atomic.Bool

// shuttingDown is set by ShutdownLiteLLM so a watchProcess goroutine woken by
// the kill knows not to spawn a replacement (which would orphan a fresh proxy
// when the server exits).
var shuttingDown bool

// InitLiteLLM initializes the LiteLLM proxy connection.
// If LITELLM_URL is set (Docker/external), it just uses that URL.
// Otherwise, it starts a local Python process.
func InitLiteLLM() error {
	if url := os.Getenv("LITELLM_URL"); url != "" {
		LiteLLMBaseURL = url
		utils.Log("[LiteLLM] Using external proxy at %s", LiteLLMBaseURL)
		if err := waitForHealth(30 * time.Second); err != nil {
			return err
		}
		litellmReady.Store(true)
		return nil
	}

	utils.Log("[LiteLLM] Starting local proxy on port 4000...")

	// If a proxy is already answering on the port — e.g. because a previous
	// server process died without cleaning up its child (observed: rc=1 exit
	// leaves uvicorn orphaned on :4000) — adopt it instead of spawning a
	// second uvicorn that will crash-loop on EADDRINUSE. `litellmReady` stays
	// false until it passes the health check below, so callers never see a
	// half-initialized proxy.
	if probeHealth(2*time.Second) == nil {
		utils.Log("[LiteLLM] Adopting existing healthy proxy at %s", LiteLLMBaseURL)
		if err := waitForHealth(10 * time.Second); err != nil {
			return fmt.Errorf("LiteLLM proxy on %s stopped responding: %w", LiteLLMBaseURL, err)
		}
		litellmReady.Store(true)
		utils.Log("[LiteLLM] Proxy is ready at %s", LiteLLMBaseURL)
		return nil
	}

	if err := startLocalProcess(); err != nil {
		return fmt.Errorf("failed to start LiteLLM: %w", err)
	}

	if err := waitForHealth(30 * time.Second); err != nil {
		ShutdownLiteLLM()
		return fmt.Errorf("LiteLLM failed health check: %w", err)
	}

	litellmReady.Store(true)
	utils.Log("[LiteLLM] Proxy is ready at %s", LiteLLMBaseURL)

	// Watch for crashes and auto-restart
	go watchProcess()

	return nil
}

// ShutdownLiteLLM stops the local LiteLLM process if one was started.
func ShutdownLiteLLM() {
	litellmMu.Lock()
	defer litellmMu.Unlock()

	litellmReady.Store(false)
	shuttingDown = true

	if litellmProcess != nil && litellmProcess.Process != nil {
		utils.Log("[LiteLLM] Shutting down proxy...")
		// The proxy runs in its own process group (Setpgid in
		// startLocalProcess), so this reaches uvicorn even if it has forked
		// children/pgid changes. Without it a server exit can orphan the
		// proxy on :4000 and every later boot crash-loops on EADDRINUSE
		// (observed: 3750 "address already in use" restarts over 24h).
		syscall.Kill(-litellmProcess.Process.Pid, syscall.SIGTERM)
		litellmProcess.Wait()
		litellmProcess = nil
	}
}

// LiteLLMReady returns whether the proxy has been initialized and is running.
func LiteLLMReady() bool {
	return litellmReady.Load()
}

func startLocalProcess() error {
	litellmMu.Lock()
	defer litellmMu.Unlock()

	// Refuse to double-start. There must be exactly one proxy; spawning a
	// second uvicorn while the first holds :4000 produces the EADDRINUSE
	// crash-loop. If a live process is already being supervised, this is a
	// no-op (watchProcess can call us from the restart loop after a genuine
	// crash replaced litellmProcess — but never while the old one is alive).
	if litellmProcess != nil && litellmProcess.Process != nil {
		utils.Log("[LiteLLM] Proxy process %d still running — not starting a duplicate", litellmProcess.Process.Pid)
		return nil
	}

	configPath := findConfigPath()
	if configPath == "" {
		return fmt.Errorf("litellm_config.yaml not found")
	}

	proxyScript := findProxyScript()
	if proxyScript == "" {
		return fmt.Errorf("litellm_proxy.py not found")
	}

	pythonBin := findPythonBin()
	cmd := exec.Command(pythonBin, proxyScript, "--config", configPath, "--port", "4000", "--host", "127.0.0.1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group so ShutdownLiteLLM can SIGTERM the whole group and
	// never orphan a uvicorn that keeps :4000 bound after a fast exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start LiteLLM process: %w", err)
	}

	litellmProcess = cmd
	return nil
}

// probeHealth performs a single health request against the proxy. Returns nil
// if a healthy response was received, or a non-nil error otherwise.
func probeHealth(timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(LiteLLMBaseURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}

func waitForHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if probeHealth(2 * time.Second) == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("LiteLLM proxy did not become healthy within %s", timeout)
}

func watchProcess() {
	backoff := time.Second

	for {
		litellmMu.Lock()
		if shuttingDown {
			litellmMu.Unlock()
			return
		}
		cmd := litellmProcess
		litellmMu.Unlock()

		if cmd == nil {
			return
		}

		err := cmd.Wait()
		// The watched process is gone either way — clear the reference under
		// the lock so a later restart can spawn a fresh one instead of
		// "adopting" a dead process.
		litellmMu.Lock()
		if litellmProcess == cmd {
			litellmProcess = nil
		}
		wasShuttingDown := shuttingDown
		litellmMu.Unlock()

		if err == nil {
			utils.Log("[LiteLLM] Proxy exited cleanly")
			litellmReady.Store(false)
			return
		}

		litellmReady.Store(false)
		if wasShuttingDown {
			utils.Log("[LiteLLM] Proxy terminated during shutdown — not restarting")
			return
		}

		utils.Error("[LiteLLM] Proxy crashed, restarting in %s...", err, backoff)
		time.Sleep(backoff)

		if err := startLocalProcess(); err != nil {
			utils.Error("[LiteLLM] Failed to restart proxy", err)
		}

		if waitForHealth(15*time.Second) == nil {
			litellmReady.Store(true)
			utils.Log("[LiteLLM] Proxy restarted successfully")
			backoff = time.Second
		} else {
			// The replacement never came up — likely a port conflict or a
			// repeatedly-failing process. Back off so we don't spawn-storm
			// (observed: 3750 EADDRINUSE restarts in 24h pinning CPU and
			// killing conversations). A later health failure or a manual
			// restart will retry.
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			utils.Error("[LiteLLM] Proxy did not become healthy; next restart attempt in %s", err, backoff)
		}
	}
}

// findConfigPath resolves litellm_config.yaml under data/, matching the
// dataDir() convention used by auth/skills/miniapps. The DATA_DIR env var
// takes precedence; otherwise we look in <exeDir>/data.
func findConfigPath() string {
	data := os.Getenv("DATA_DIR")
	if data == "" {
		exeDir := filepath.Dir(os.Args[0])
		data = filepath.Join(exeDir, "data")
	}
	p := filepath.Join(data, "litellm_config.yaml")
	if _, err := os.Stat(p); err == nil {
		abs, _ := filepath.Abs(p)
		return abs
	}
	return ""
}

func findProxyScript() string {
	exeDir := filepath.Dir(os.Args[0])
	candidates := []string{
		"litellm_proxy.py",
		"litellm/litellm_proxy.py",
		"../litellm_proxy.py",
		filepath.Join(exeDir, "litellm_proxy.py"),
		filepath.Join(exeDir, "litellm", "litellm_proxy.py"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func findPythonBin() string {
	// Check for venv first
	exeDir := filepath.Dir(os.Args[0])
	venvPaths := []string{
		"litellm_venv/bin/python",
		"litellm_venv/Scripts/python.exe",
		"litellm/litellm_venv/bin/python",
		"litellm/litellm_venv/Scripts/python.exe",
		"../litellm_venv/bin/python",
		"../litellm_venv/Scripts/python.exe",
		filepath.Join(exeDir, "litellm", "litellm_venv", "bin", "python"),
		filepath.Join(exeDir, "litellm", "litellm_venv", "Scripts", "python.exe"),
	}
	for _, p := range venvPaths {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}

	// Fall back to system python
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	return "python"
}
