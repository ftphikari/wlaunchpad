package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/gtk"
	"github.com/joshuarubin/go-sway"
)

func wayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

// If filename is a lock file, returns the PID of the process locking it
func getLockFilePid(filename string) (pid int, err error) {
	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}

	pid, err = strconv.Atoi(string(contents))
	return
}

// createLockFile tries to create a file with given name and acquire an
// exclusive lock on it. If the file already exists AND is still locked, it will
// fail.
func createLockFile(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		file.Close()
		return nil, err
	}

	// Write PID to lock file
	contents := strconv.Itoa(os.Getpid())
	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.WriteString(contents); err != nil {
		file.Close()
		return nil, err
	}

	return file, nil
}

func createPixbuf(icon string, size int) (*gdk.Pixbuf, error) {
	if strings.Contains(icon, "/") {
		pixbuf, err := gdk.PixbufNewFromFileAtSize(icon, size, size)
		if err != nil {
			log.Printf("%s", err)
			return nil, err
		}
		return pixbuf, nil
	} else if strings.HasSuffix(icon, ".svg") || strings.HasSuffix(icon, ".png") || strings.HasSuffix(icon, ".xpm") {
		// for entries like "Icon=netflix-desktop.svg"
		icon = strings.Split(icon, ".")[0]
	}

	pixbuf, err := iconTheme.LoadIcon(icon, size, gtk.ICON_LOOKUP_FORCE_SIZE)
	if err != nil {
		if strings.HasPrefix(icon, "/") {
			pixbuf, err := gdk.PixbufNewFromFileAtSize(icon, size, size)
			if err != nil {
				return nil, err
			}
			return pixbuf, nil
		}

		pixbuf, err := iconTheme.LoadIcon(icon, size, gtk.ICON_LOOKUP_FORCE_SIZE)
		if err != nil {
			return nil, err
		}
		return pixbuf, nil
	}
	return pixbuf, nil
}

func tempDir() string {
	if os.Getenv("TMPDIR") != "" {
		return os.Getenv("TMPDIR")
	} else if os.Getenv("TEMP") != "" {
		return os.Getenv("TEMP")
	} else if os.Getenv("TMP") != "" {
		return os.Getenv("TMP")
	}
	return "/tmp"
}

func getAppDirs() []string {
	var dirs []string
	xdgDataDirs := ""

	home := os.Getenv("HOME")
	xdgDataHome := os.Getenv("XDG_DATA_HOME")
	if os.Getenv("XDG_DATA_DIRS") != "" {
		xdgDataDirs = os.Getenv("XDG_DATA_DIRS")
	} else {
		xdgDataDirs = "/usr/local/share/:/usr/share/"
	}
	if xdgDataHome != "" {
		dirs = append(dirs, filepath.Join(xdgDataHome, "applications"))
	} else if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local/share/applications"))
	}
	for _, d := range strings.Split(xdgDataDirs, ":") {
		dirs = append(dirs, filepath.Join(d, "applications"))
	}
	flatpakDirs := []string{filepath.Join(home, ".local/share/flatpak/exports/share/applications"),
		"/var/lib/flatpak/exports/share/applications"}

	for _, d := range flatpakDirs {
		if !contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func listFiles(dir string) ([]fs.FileInfo, error) {
	files, err := ioutil.ReadDir(dir)
	if err == nil {
		return files, nil
	}
	return nil, err
}

func listDesktopFiles() []string {
	appDirs := getAppDirs()
	var paths []string
	for _, dir := range appDirs {
		dirs, err := listFiles(dir)
		if err == nil {
			for _, file := range dirs {
				parts := strings.Split(file.Name(), ".")
				if parts[len(parts)-1] == "desktop" {
					paths = append(paths, filepath.Join(dir, file.Name()))
				}
			}
		}
	}
	return paths
}

func parseDesktopFiles() string {
	desktopFiles := listDesktopFiles()
	desktopEntries = []desktopEntry{}
	id2entry := make(map[string]desktopEntry)
	skipped := 0
	hidden := 0
	for _, file := range desktopFiles {
		id := filepath.Base(file)
		if _, ok := id2entry[id]; ok {
			skipped++
			continue
		}

		entry, err := parseDesktopEntryFile(id, file)
		if err != nil {
			continue
		}

		if entry.NoDisplay {
			hidden++
			// We still need hidden entries, so `continue` is disallowed here
			// Fixes introduced in #19
		}

		id2entry[entry.DesktopID] = entry
		desktopEntries = append(desktopEntries, entry)
	}
	sort.Slice(desktopEntries, func(i, j int) bool {
		return desktopEntries[i].NameLoc < desktopEntries[j].NameLoc
	})
	summary := fmt.Sprintf("%v entries (+%v hidden)", len(desktopEntries)-hidden, hidden)
	log.Printf("Found %v desktop files\n", len(desktopEntries))
	log.Printf("Skipped %v duplicates; %v .desktop entries hidden by \"NoDisplay=true\"", skipped, hidden)
	return summary
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

func launch(command string, terminal bool) {
	// trim % and everything afterwards
	if strings.Contains(command, "%") {
		cutAt := strings.Index(command, "%")
		if cutAt != -1 {
			command = command[:cutAt-1]
		}
	}

	elements := strings.Split(command, " ")

	// find prepended env variables, if any
	envVarsNum := strings.Count(command, "=")
	var envVars []string

	cmdIdx := -1

	if envVarsNum > 0 {
		for idx, item := range elements {
			if strings.Contains(item, "=") {
				envVars = append(envVars, item)
			} else if !strings.HasPrefix(item, "-") && cmdIdx == -1 {
				cmdIdx = idx
			}
		}
	}
	if cmdIdx == -1 {
		cmdIdx = 0
	}

	cmd := exec.Command(elements[cmdIdx], elements[1+cmdIdx:]...)

	if terminal {
		args := []string{elements[cmdIdx]}
		cmd = exec.Command(*term, args...)
	}

	// set env variables
	if len(envVars) > 0 {
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, envVars...)
	}

	msg := fmt.Sprintf("env vars: %s; command: '%s'; args: %s\n", envVars, elements[cmdIdx], elements[1+cmdIdx:])
	log.Println(msg)

	cmd.Start()
	if *daemon {
		win.Hide()
	} else {
		gtk.MainQuit()
	}
}

// Returns map output name -> gdk.Monitor
func mapOutputs() (map[string]*gdk.Monitor, error) {
	result := make(map[string]*gdk.Monitor)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	client, err := sway.New(ctx)
	if err != nil {
		return nil, err
	}

	outputs, err := client.GetOutputs(ctx)
	if err != nil {
		return nil, err
	}

	display, err := gdk.DisplayGetDefault()
	if err != nil {
		return nil, err
	}

	num := display.GetNMonitors()
	for i := 0; i < num; i++ {
		monitor, _ := display.GetMonitor(i)
		geometry := monitor.GetGeometry()
		// assign output to monitor on the basis of the same x, y coordinates
		for _, output := range outputs {
			if int(output.Rect.X) == geometry.GetX() && int(output.Rect.Y) == geometry.GetY() {
				result[output.Name] = monitor
			}
		}
	}
	return result, nil
}

func parseDesktopEntryFile(id string, path string) (e desktopEntry, err error) {
	o, err := os.Open(path)
	if err != nil {
		return e, err
	}
	defer o.Close()

	return parseDesktopEntry(id, o)
}

func parseDesktopEntry(id string, in io.Reader) (entry desktopEntry, err error) {
	cleanexec := strings.NewReplacer("\"", "", "'", "")
	entry.DesktopID = id
	lang := strings.Split(os.Getenv("LANG"), ".")[0]
	localizedName := fmt.Sprintf("Name[%s]", strings.Split(lang, "_")[0])
	localizedComment := fmt.Sprintf("Comment[%s]", strings.Split(lang, "_")[0])
	scanner := bufio.NewScanner(in)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		l := scanner.Text()
		if strings.HasPrefix(l, "[") && l != "[Desktop Entry]" {
			break
		}

		name, value := parseKeypair(l)
		if value == "" {
			continue
		}

		switch name {
		case "Name":
			entry.Name = value
		case localizedName:
			entry.NameLoc = value
		case "Comment":
			entry.Comment = value
		case localizedComment:
			entry.CommentLoc = value
		case "Icon":
			entry.Icon = value
		case "Categories":
			entry.Category = value
		case "Terminal":
			entry.Terminal, _ = strconv.ParseBool(value)
		case "NoDisplay":
			entry.NoDisplay, _ = strconv.ParseBool(value)
		case "Exec":
			entry.Exec = cleanexec.Replace(value)
		}
	}

	// if name[ln] not found, let's try to find name[ln_LN]
	if entry.NameLoc == "" {
		entry.NameLoc = entry.Name
	}
	if entry.CommentLoc == "" {
		entry.CommentLoc = entry.Comment
	}
	return entry, err
}

func parseKeypair(s string) (string, string) {
	if idx := strings.IndexRune(s, '='); idx > 0 {
		return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
	}
	return s, ""
}
