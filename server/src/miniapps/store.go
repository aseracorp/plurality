package miniapps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
	"github.com/google/uuid"
)

const (
	systemAuthor = "system"
	pinsFile     = "pins.json"
	presetsDir   = "presets"
)

var (
	storeMu  sync.RWMutex
	builtins map[string]utils.MiniApp // id -> app
)

func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

func userDataRoot() string {
	if p := os.Getenv("USER_DATA_STORAGE"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "users-data")
}

func builtinsDir() string { return filepath.Join(dataDir(), presetsDir) }
func userDir(u string) string {
	return filepath.Join(userDataRoot(), u, presetsDir)
}

// LoadBuiltins reads data/presets/*.json into memory. Safe to re-run.
func LoadBuiltins() {
	storeMu.Lock()
	defer storeMu.Unlock()
	builtins = map[string]utils.MiniApp{}

	root := builtinsDir()
	if err := os.MkdirAll(root, 0755); err != nil {
		utils.Error("[MiniApps] failed to create presets dir", err)
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		utils.Error("[MiniApps] failed to read presets dir", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		app, err := readApp(filepath.Join(root, e.Name()))
		if err != nil {
			utils.Error("[MiniApps] failed to read builtin "+e.Name(), err)
			continue
		}
		app.ID = id
		if app.Author == "" {
			app.Author = systemAuthor
		}
		builtins[id] = app
	}
	utils.Log("[MiniApps] %d builtin preset(s) loaded", len(builtins))
}

// wireMiniApp is the on-disk representation: same as utils.MiniApp but with
// the prompt map exposed so it round-trips through JSON. utils.MiniApp itself
// keeps `json:"-"` on Prompt so it never leaks via the HTTP API.
type wireMiniApp struct {
	utils.MiniApp
	Prompt map[string]string `json:"prompt,omitempty"`
}

func readApp(path string) (utils.MiniApp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return utils.MiniApp{}, err
	}
	var w wireMiniApp
	if err := json.Unmarshal(data, &w); err != nil {
		return utils.MiniApp{}, err
	}
	w.MiniApp.Prompt = w.Prompt
	return w.MiniApp, nil
}

func writeAppToFile(path string, app utils.MiniApp) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(wireMiniApp{MiniApp: app, Prompt: app.Prompt}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validateID(id string) error {
	if id == "" {
		return errors.New("empty id")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return errors.New("invalid id")
	}
	return nil
}

func userAppPath(username, id string) string {
	return filepath.Join(userDir(username), id+".json")
}

// listUserOverrides returns all user-owned mini apps for `username`, keyed by id.
func listUserOverrides(username string) map[string]utils.MiniApp {
	out := map[string]utils.MiniApp{}
	dir := userDir(username)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		app, err := readApp(filepath.Join(dir, e.Name()))
		if err != nil {
			utils.Error("[MiniApps] failed to read user preset "+e.Name(), err)
			continue
		}
		app.ID = id
		out[id] = app
	}
	return out
}

// ListForUser returns the merged builtin + user-override view, sorted by name.
func ListForUser(username string) []utils.MiniApp {
	storeMu.RLock()
	merged := make(map[string]utils.MiniApp, len(builtins))
	for id, app := range builtins {
		merged[id] = app
	}
	storeMu.RUnlock()

	for id, app := range listUserOverrides(username) {
		merged[id] = app
	}

	out := make([]utils.MiniApp, 0, len(merged))
	for _, app := range merged {
		out = append(out, app)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Get returns a single mini app by id (user override wins).
func Get(username, id string) (*utils.MiniApp, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	if app, err := readApp(userAppPath(username, id)); err == nil {
		app.ID = id
		return &app, nil
	}
	storeMu.RLock()
	defer storeMu.RUnlock()
	if app, ok := builtins[id]; ok {
		return &app, nil
	}
	return nil, errors.New("mini-app not found")
}

// Create writes a new user-owned mini app. Generates a UUID id if absent.
func Create(username string, app utils.MiniApp) (*utils.MiniApp, error) {
	if app.ID == "" {
		app.ID = uuid.NewString()
	}
	if err := validateID(app.ID); err != nil {
		return nil, err
	}
	app.Author = username

	path := userAppPath(username, app.ID)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("mini-app %q already exists", app.ID)
	}
	if err := writeAppToFile(path, app); err != nil {
		return nil, err
	}
	return &app, nil
}

// Update writes a user override. Editing a builtin creates a user override
// with the same id (which then shadows the builtin on next list).
func Update(username, id string, app utils.MiniApp) (*utils.MiniApp, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	app.ID = id
	if app.Author == "" {
		app.Author = username
	}
	path := userAppPath(username, id)
	if err := writeAppToFile(path, app); err != nil {
		return nil, err
	}
	return &app, nil
}

// Delete removes a user override. Builtins cannot be deleted (returns 409-style error).
func Delete(username, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	path := userAppPath(username, id)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		storeMu.RLock()
		_, isBuiltin := builtins[id]
		storeMu.RUnlock()
		if isBuiltin {
			return errors.New("cannot delete builtin mini-app; edit data/presets/ to remove it")
		}
		return errors.New("mini-app not found")
	}
	return os.Remove(path)
}

// --- Pins ---

func pinsPath(username string) string {
	return filepath.Join(userDataRoot(), username, pinsFile)
}

func loadPins(username string) []string {
	data, err := os.ReadFile(pinsPath(username))
	if err != nil {
		return []string{}
	}
	var pins []string
	if err := json.Unmarshal(data, &pins); err != nil {
		return []string{}
	}
	return pins
}

func savePins(username string, pins []string) error {
	if err := os.MkdirAll(filepath.Dir(pinsPath(username)), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(pins)
	if err != nil {
		return err
	}
	return os.WriteFile(pinsPath(username), data, 0644)
}

// GetPinned returns the user's pinned mini apps in the order they were pinned.
func GetPinned(username string) []utils.MiniApp {
	pins := loadPins(username)
	if len(pins) == 0 {
		return []utils.MiniApp{}
	}
	out := make([]utils.MiniApp, 0, len(pins))
	for _, id := range pins {
		app, err := Get(username, id)
		if err != nil {
			continue
		}
		out = append(out, *app)
	}
	return out
}

// Pin adds an id to the user's pin list (idempotent).
func Pin(username, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if _, err := Get(username, id); err != nil {
		return err
	}
	pins := loadPins(username)
	for _, p := range pins {
		if p == id {
			return nil
		}
	}
	pins = append(pins, id)
	return savePins(username, pins)
}

// Unpin removes an id from the user's pin list (idempotent).
func Unpin(username, id string) error {
	pins := loadPins(username)
	out := pins[:0]
	for _, p := range pins {
		if p == id {
			continue
		}
		out = append(out, p)
	}
	return savePins(username, out)
}
