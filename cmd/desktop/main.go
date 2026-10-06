// Command desktop is the AIO Agent desktop app: a menu-bar / system-tray app
// that embeds the daemon and gives it a settings, status and log GUI.
//
// It runs the same daemon as cmd/daemon (which stays available for headless
// use), configured from a per-user config file edited through the GUI.
package main

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"runtime"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/shellenv"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// version is overridable at build time: -ldflags "-X main.version=1.2.3".
var version = "0.1.0"

//go:embed frontend
var frontend embed.FS

//go:embed assets/tray.png
var trayIcon []byte

//go:embed assets/tray-template.png
var trayTemplateOn []byte

//go:embed assets/tray-template-off.png
var trayTemplateOff []byte

func main() {
	// Finder/login-item launches get launchd's bare PATH; agent CLIs live in
	// Homebrew/npm dirs, so import the login shell's PATH first.
	shellenv.ImportPATH()

	cfgPath := os.Getenv("AIO_CONFIG")
	if cfgPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			log.Fatal(err)
		}
		cfgPath = p
	}

	ctl, err := newApp(version, cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	assets, _ := fs.Sub(frontend, "frontend")

	var window *application.WebviewWindow
	showWindow := func() {
		window.Show()
		window.Focus()
	}

	app := application.New(application.Options{
		Name:        "AIO Agent",
		Description: "Control your AI coding agents from your phone",
		Services:    []application.Service{application.NewService(ctl)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac: application.MacOptions{
			// Menu-bar app: no Dock icon, keep running with no windows open.
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
		Windows: application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Linux:   application.LinuxOptions{DisableQuitOnLastWindowClosed: true},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "ai.aioagent.desktop",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				application.InvokeAsync(showWindow)
			},
		},
	})

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "main",
		Title:     "AIO Agent",
		Width:     480,
		Height:    640,
		MinWidth:  400,
		MinHeight: 480,
		URL:       "/",
		Hidden:    ctl.Configured(), // first run: open straight to Settings
	})
	// Closing the window hides it; the app lives on in the tray.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		window.Hide()
		e.Cancel()
	})

	tray := app.SystemTray.New()
	tray.SetTooltip("AIO Agent")
	setTrayIcon := func(connected bool) {
		if runtime.GOOS == "darwin" {
			if connected {
				tray.SetTemplateIcon(trayTemplateOn)
			} else {
				tray.SetTemplateIcon(trayTemplateOff)
			}
			return
		}
		tray.SetIcon(trayIcon)
	}
	setTrayIcon(false)

	menu := app.NewMenu()
	statusItem := menu.Add("Disconnected").SetEnabled(false)
	menu.AddSeparator()
	menu.Add("Open AIO Agent…").OnClick(func(*application.Context) { showWindow() })
	toggleItem := menu.Add("Connect").OnClick(func(*application.Context) {
		if st := ctl.Status().State; st == StateConnected || st == StateConnecting {
			go ctl.Disconnect()
			return
		}
		if err := ctl.Connect(); err != nil {
			showWindow()
		}
	})
	menu.AddSeparator()
	menu.Add("Quit AIO Agent").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)

	ctl.onStatus = func(st Status) {
		application.InvokeAsync(func() {
			label := map[string]string{
				StateUnconfigured: "Not set up",
				StateStopped:      "Disconnected",
				StateConnecting:   "Connecting…",
				StateConnected:    "Connected",
			}[st.State]
			if st.State == StateConnected && st.Running > 0 {
				label += " · running"
			}
			statusItem.SetLabel(label)
			if st.State == StateConnected || st.State == StateConnecting {
				toggleItem.SetLabel("Disconnect")
			} else {
				toggleItem.SetLabel("Connect")
			}
			toggleItem.SetEnabled(st.State != StateUnconfigured)
			tray.SetTooltip("AIO Agent — " + label)
			setTrayIcon(st.State == StateConnected)
		})
	}

	// Auto-connect on launch once configured.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		ctl.publishStatus()
		if ctl.Configured() {
			if err := ctl.Connect(); err != nil {
				ctl.logger.Error("auto-connect failed", "err", err)
			}
		}
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
