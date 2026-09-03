package tray

import (
	"log"

	"github.com/godbus/dbus/v5"
)

// checkTrayHost warns when nothing on the session bus can show a tray icon.
// The widget keeps running: systray registers itself as soon as a host
// appears. Without the warning it would simply look as if nothing happened.
func checkTrayHost() {
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Printf("no D-Bus session bus (%v); the tray icon cannot be shown", err)
		return
	}
	var owned bool
	call := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher")
	if call.Err != nil || call.Store(&owned) != nil || !owned {
		log.Printf("no system tray host found (org.kde.StatusNotifierWatcher is not on the session bus); " +
			"waiting for one. GNOME needs the AppIndicator extension, XFCE the \"Status Tray Items\" panel item")
	}
}
