//go:build windows

package watchdog

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

//go:embed ui/settings.ps1
var settingsPowerShell string

// openLegacySettings keeps the embedded settings.ps1 window reachable when no
// desktop client is installed next to the watchdog. It is deleted together
// with ui/settings.ps1 under T8.
func (a *trayApp) openLegacySettings() {
	a.settingsMu.Lock()
	if a.settingsOpen {
		a.settingsMu.Unlock()
		return
	}
	a.settingsOpen = true
	a.settingsMu.Unlock()
	scriptPath, err := ensureSettingsScript(a.dataDir)
	if err != nil {
		a.logger.Printf("settings open failed category=settings_script")
		a.settingsFinished()
		return
	}
	config, err := loadOrCreateConfig(filepath.Join(a.dataDir, "config.json"))
	if err != nil {
		a.logger.Printf("settings open failed category=settings_config")
		a.settingsFinished()
		return
	}
	powerShell, err := resolvePowerShellExecutable(config.PowerShellExecutable)
	if err != nil {
		a.logger.Printf("settings open failed category=settings_shell")
		a.settingsFinished()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		a.logger.Printf("settings open failed category=settings_executable")
		a.settingsFinished()
		return
	}
	arguments := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-STA", "-File", scriptPath, "-DataDir", a.dataDir, "-Executable", executable}
	if os.Getenv("CODEX_AUTO_RETRY_SETTINGS_SMOKE") == "1" {
		arguments = append(arguments, "-SmokeTest")
	}
	command := exec.Command(powerShell, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := command.Start(); err != nil {
		a.logger.Printf("settings open failed category=settings_start")
		a.settingsFinished()
		return
	}
	go func() {
		if err := command.Wait(); err != nil {
			a.logger.Printf("settings process stopped category=settings_process")
		}
		a.settingsFinished()
	}()
}

func (a *trayApp) settingsFinished() {
	a.settingsMu.Lock()
	a.settingsOpen = false
	a.settingsMu.Unlock()
}

func ensureSettingsScript(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "settings.ps1")
	content := ensureUTF8BOM([]byte(settingsPowerShell))
	current, _ := os.ReadFile(path)
	if string(current) == string(content) {
		return path, nil
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func ensureUTF8BOM(content []byte) []byte {
	bom := []byte{0xef, 0xbb, 0xbf}
	if bytes.HasPrefix(content, bom) {
		return content
	}
	return append(bom, content...)
}
