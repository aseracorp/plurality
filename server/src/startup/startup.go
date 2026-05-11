package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// Run launches every *.sh file in ~/.plurality/startup/ with nohup, detached,
// piping stdout+stderr to <script>.sh.log next to the script. Each script is
// fully detached: if the server exits, the scripts keep running.
//
// Missing directory is not an error — it's the first-boot default.
func Run() {
	home, err := os.UserHomeDir()
	if err != nil {
		utils.Error("[startup] cannot resolve user home directory", err)
		return
	}

	dir := filepath.Join(home, ".plurality", "startup")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		utils.Error("[startup] cannot read directory", err, dir)
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".sh") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		scriptPath := filepath.Join(dir, name)
		logPath := scriptPath + ".log"
		launch(scriptPath, logPath)
	}
}

func launch(scriptPath, logPath string) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		utils.Error("[startup] cannot open log file", err, logPath)
		return
	}
	defer logFile.Close()

	cmd := exec.Command("nohup", "bash", scriptPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		utils.Error("[startup] failed to launch", err, scriptPath)
		return
	}

	if err := cmd.Process.Release(); err != nil {
		utils.Error("[startup] failed to detach child", err, scriptPath)
	}

	utils.Log("[startup] launched %s -> %s", scriptPath, logPath)
}
