package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"path/filepath"
	"runtime"

	"github.com/dlasky/gotk3-layershell/layershell"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
)

type desktopEntry struct {
	DesktopID  string
	Name       string
	NameLoc    string
	Comment    string
	CommentLoc string
	Icon       string
	Exec       string
	Category   string
	Terminal   bool
	NoDisplay  bool
}

// UI elements
var (
	win                    *gtk.Window
	resultWindow           *gtk.ScrolledWindow
	searchEntry            *gtk.SearchEntry
	phrase                 string
	iconTheme              *gtk.IconTheme
	appFlowBox             *gtk.FlowBox
	appSearchResultWrapper *gtk.Box
	statusLabel            *gtk.Label
	status                 string
	desktopEntries         []desktopEntry
	iconCache              = make(map[string]*gdk.Pixbuf)
)

func defaultStringIfBlank(s, fallback string) string {
	s = strings.TrimSpace(s)
	// os.Getenv("TERM") returns "linux" instead of empty string, if program has been started
	// from a key binding defined in the config file. See #23.
	if s == "" || s == "linux" {
		return fallback
	}
	return s
}

func setUpAppsFlowBox(searchPhrase string) {
	// this reduces RAM usage significantly for daemon mode
	// it also MIGHT crash, but did not happen in my testing
	glib.IdleAdd(func() bool {
		runtime.GC()
		return false
	})

	if appFlowBox != nil {
		appFlowBox.GetChildren().Foreach(func(item interface{}) {
			item.(*gtk.Widget).Destroy()
		})
	} else {
		appFlowBox, _ = gtk.FlowBoxNew()
		appFlowBox.SetMinChildrenPerLine(*columnsNumber)
		appFlowBox.SetMaxChildrenPerLine(*columnsNumber)
		appFlowBox.SetColumnSpacing(*itemSpacing)
		appFlowBox.SetRowSpacing(*itemSpacing)
		appFlowBox.SetHomogeneous(true)
		appFlowBox.SetSelectionMode(gtk.SELECTION_NONE)
	}

	for _, entry := range desktopEntries {
		if !(searchPhrase == "" || !entry.NoDisplay && (strings.Contains(strings.ToLower(entry.NameLoc), strings.ToLower(searchPhrase)) ||
			strings.Contains(strings.ToLower(entry.CommentLoc), strings.ToLower(searchPhrase)) ||
			strings.Contains(strings.ToLower(entry.Comment), strings.ToLower(searchPhrase)) ||
			strings.Contains(strings.ToLower(entry.Exec), strings.ToLower(searchPhrase)))) {
			continue
		}
		if !entry.NoDisplay {
			button, _ := gtk.ButtonNew()
			button.SetAlwaysShowImage(true)

			pixbuf, ok := iconCache[entry.Icon]
			if !ok {
				var err error
				if entry.Icon != "" {
					pixbuf, err = createPixbuf(entry.Icon, *iconSize)
					if err != nil {
						log.Print(err)
						pixbuf, err = createPixbuf("image-missing", *iconSize)
					}
				}
				if err != nil {
					log.Print(err)
					pixbuf, _ = createPixbuf("unknown", *iconSize)
				}
				iconCache[entry.Icon] = pixbuf
			}

			img, _ := gtk.ImageNewFromPixbuf(pixbuf)
			button.SetImage(img)
			button.SetImagePosition(gtk.POS_TOP)
			name := entry.NameLoc
			if len(name) > 20 {
				r := []rune(name)
				name = string(r[:17])
				name = fmt.Sprintf("%s…", name)
			}
			button.SetLabel(name)

			exec := entry.Exec
			terminal := entry.Terminal
			desc := entry.CommentLoc
			button.Connect("button-release-event", func(btn *gtk.Button, e *gdk.Event) bool {
				btnEvent := gdk.EventButtonNewFromEvent(e)
				if btnEvent.Button() == 1 {
					launch(exec, terminal)
					return true
				} else if btnEvent.Button() == 3 {
					return true
				}
				return false
			})
			button.Connect("activate", func() {
				launch(exec, terminal)
			})
			button.Connect("enter-notify-event", func() {
				statusLabel.SetText(desc)
			})
			appFlowBox.Add(button)
		}
	}
	// While moving focus with arrow keys we want buttons to get focus directly
	appFlowBox.GetChildren().Foreach(func(item interface{}) {
		item.(*gtk.Widget).SetCanFocus(false)
	})
	resultWindow.ShowAll()
}

func showWindow() {
	parseDesktopFiles()
	searchEntry.SetText("")
	setUpAppsFlowBox("")
	resultWindow.GetVAdjustment().SetValue(0)
	focusFirstItem()
	win.ShowAll()
}

func focusFirstItem() {
	if appFlowBox == nil {
		return
	}

	b := appFlowBox.GetChildAtIndex(0)
	if b != nil {
		button, err := b.GetChild()
		if err == nil {
			button.ToWidget().GrabFocus()
		}
	}
}

// Flags
var (
	debug         = flag.Bool("debug", false, "display debug information")
	daemon        = flag.Bool("d", false, "launch in daemon mode")
	noshow        = flag.Bool("n", false, "don't show the window on first launch (only if daemon mode is on)")
	styleFile     = flag.String("style", "", "css style file name")
	targetOutput  = flag.String("o", "", "name of the output to display the launchpad on (sway only)")
	iconSize      = flag.Int("i", 64, "icon size")
	columnsNumber = flag.Uint("c", 6, "number of columns")
	itemSpacing   = flag.Uint("s", 20, "icon spacing")
	term          = flag.String("t", defaultStringIfBlank(os.Getenv("TERM"), "foot"), "terminal emulator")
)

func main() {
	timeStart := time.Now()
	flag.Parse()

	if !*debug {
		log.SetOutput(io.Discard)
	}

	// Gentle SIGTERM handler thanks to reiki4040 https://gist.github.com/reiki4040/be3705f307d3cd136e85
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGUSR1)
	go func() {
		for {
			s := <-signalChan
			if s == syscall.SIGTERM || (s == syscall.SIGUSR1 && !*daemon) {
				log.Println("SIGTERM or SIGUSR1 received, exiting..")
				gtk.MainQuit()
			} else if s == syscall.SIGUSR1 {
				log.Println("SIGUSR1 received, toggling..", )
				glib.IdleAdd(func() bool {
					if win.GetVisible() {
						win.Hide()
					} else {
						showWindow()
					}
					return false
				})
			}
		}
	}()

	// We want the same key/mouse binding to turn the dock off: kill the running instance and exit.
	lockFilePath := filepath.Join(tempDir(), "wlaunchpad.lock")
	lockFile, err := createLockFile(lockFilePath)
	if err != nil {
		pid, err := getLockFilePid(lockFilePath)
		if err == nil {
			log.Println("Running instance found, sending SIGUSR1 and exiting…")
			syscall.Kill(pid, syscall.SIGUSR1)
		}
		os.Exit(0)
	}
	defer lockFile.Close()

	// USER INTERFACE
	gtk.Init(nil)

	cssProvider, _ := gtk.CssProviderNew()
	if *styleFile != "" {
		err = cssProvider.LoadFromPath(*styleFile)
		if err != nil {
			log.Printf("ERROR: %s css file not found or erroneous. Using GTK styling.\n", *styleFile)
			log.Printf("%s\n", err)
		} else {
			log.Printf("Using style from %s\n", *styleFile)
			screen, _ := gdk.ScreenGetDefault()
			gtk.AddProviderForScreen(screen, cssProvider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
		}
	}

	win, err = gtk.WindowNew(gtk.WINDOW_TOPLEVEL)
	if err != nil {
		log.Fatal("Unable to create window:", err)
	}

	if wayland() {
		layershell.InitForWindow(win)

		var output2mon map[string]*gdk.Monitor
		if *targetOutput != "" {
			// We want to assign layershell to a monitor, but we only know the output name!
			output2mon, err = mapOutputs()
			if err == nil {
				monitor := output2mon[*targetOutput]
				layershell.SetMonitor(win, monitor)

			} else {
				log.Printf("%s", err)
			}
		}

		layershell.SetAnchor(win, layershell.LAYER_SHELL_EDGE_BOTTOM, true)
		layershell.SetAnchor(win, layershell.LAYER_SHELL_EDGE_TOP, true)
		layershell.SetAnchor(win, layershell.LAYER_SHELL_EDGE_LEFT, true)
		layershell.SetAnchor(win, layershell.LAYER_SHELL_EDGE_RIGHT, true)
		layershell.SetLayer(win, layershell.LAYER_SHELL_LAYER_OVERLAY)
		layershell.SetExclusiveZone(win, -1)
		layershell.SetKeyboardMode(win, layershell.LAYER_SHELL_KEYBOARD_MODE_EXCLUSIVE)
	}

	win.Connect("destroy", func() {
		if *daemon {
			win.Hide()
		} else {
			gtk.MainQuit()
		}
	})

	win.Connect("key-press-event", func(window *gtk.Window, event *gdk.Event) bool {
		key := &gdk.EventKey{Event: event}
		switch key.KeyVal() {
		case gdk.KEY_Escape:
			s, _ := searchEntry.GetText()
			if s != "" {
				searchEntry.GrabFocus()
				searchEntry.SetText("")
			} else {
				if *daemon {
					win.Hide()
				} else {
					gtk.MainQuit()
				}
			}
			return false
		case gdk.KEY_downarrow, gdk.KEY_Up, gdk.KEY_Down, gdk.KEY_Left, gdk.KEY_Right, gdk.KEY_Tab,
			gdk.KEY_Return, gdk.KEY_Page_Up, gdk.KEY_Page_Down, gdk.KEY_Home, gdk.KEY_End:
			return false

		default:
			if !searchEntry.IsFocus() {
				searchEntry.GrabFocusWithoutSelecting()
			}
			return false
		}
	})

	/*
		In case someone REALLY needed to use X11 - for some stupid Zoom meeting or something, this allows
		the drawer to behave properly on Openbox, and possibly somewhere else. For sure not on i3.
		This feature is not really supported and will stay undocumented.
	*/
	if !wayland() {
		log.Println("Not Wayland, oh really?")
		win.SetDecorated(false)
		win.Maximize()
	}
	// Set up UI
	iconTheme, err = gtk.IconThemeGetDefault()
	if err != nil {
		log.Fatal("Couldn't get default theme: ", err)
	}

	outerVBox, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	win.Add(outerVBox)

	searchBoxWrapper, _ := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, 0)
	outerVBox.PackStart(searchBoxWrapper, false, false, 10)

	searchEntry, _ = gtk.SearchEntryNew()
	searchEntry.SetPlaceholderText("Type to search")
	searchEntry.Connect("search-changed", func() {
		phrase, _ = searchEntry.GetText()
		if len(phrase) > 0 {
			setUpAppsFlowBox(phrase)
		} else {
			setUpAppsFlowBox("")
		}
		focusFirstItem()
	})
	searchEntry.SetMaxWidthChars(30)
	searchBoxWrapper.PackStart(searchEntry, true, false, 0)

	resultWindow, _ = gtk.ScrolledWindowNew(nil, nil)
	resultWindow.SetEvents(int(gdk.ALL_EVENTS_MASK))
	resultWindow.SetPolicy(gtk.POLICY_AUTOMATIC, gtk.POLICY_AUTOMATIC)
	outerVBox.PackStart(resultWindow, true, true, 10)

	resultsWrapper, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	resultWindow.Add(resultsWrapper)

	appSearchResultWrapper, _ = gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	resultsWrapper.PackStart(appSearchResultWrapper, false, false, 0)

	status = parseDesktopFiles()
	setUpAppsFlowBox("")

	hWrapper, _ := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, 0)
	appSearchResultWrapper.PackStart(hWrapper, false, false, 0)
	hWrapper.PackStart(appFlowBox, true, false, 0)

	placeholder, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	resultsWrapper.PackStart(placeholder, true, true, 0)
	placeholder.SetSizeRequest(20, 20)

	statusLineWrapper, _ := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, 0)
	outerVBox.PackStart(statusLineWrapper, false, false, 10)
	statusLabel, _ = gtk.LabelNew(status)
	statusLineWrapper.PackStart(statusLabel, true, false, 0)

	if !*daemon || !*noshow {
		focusFirstItem()
		win.ShowAll()
	}

	t := time.Now()
	log.Printf("UI created in %v ms. Thank you for your patience.\n", t.Sub(timeStart).Milliseconds())
	gtk.Main()
}
