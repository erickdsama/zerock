package tray

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/cliutil"
	"github.com/erickdsama/zerock/internal/version"
)

const usage = `zerock-tray - manage zerock tunnels from the menu bar / system tray.

Usage:
  zerock-tray [--profile name]

The widget reads the same config as the CLI (zerock login), lists saved tunnels
so they can be started and stopped with a click, shows the token's other live
tunnels, and copies URLs to the clipboard.

Flags:
  --profile name   saved profile to use (default: the configured default)
  --version        print the version and exit
`

// Slot counts bound how many entries the menu can show. Tray menus cannot
// reorder items after creation, so the menu is built once with hidden slots
// that are filled in as tunnels come and go.
const (
	savedSlots  = 12
	remoteSlots = 12
	profSlots   = 8
)

// Main runs the widget and returns a process exit code.
func Main(args []string) int {
	fs := cliutil.NewFlagSet("zerock-tray", usage)
	fs.Usage = func() {} // printed below, once, on stdout
	profile := fs.String("profile", "", "saved profile to use")
	showVersion := fs.Bool("version", false, "print the version")
	if err := cliutil.ParseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usage)
			return 0
		}
		fmt.Fprintf(os.Stderr, "zerock-tray: %v\n", err)
		return 2
	}
	if *showVersion {
		fmt.Printf("zerock-tray %s\n", version.String())
		return 0
	}

	log.SetFlags(log.Ltime)
	log.SetPrefix("zerock-tray: ")

	checkTrayHost()
	u := newUI(*profile)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		u.quit()
	}()

	// systray.Run blocks on the platform's event loop until Quit is called.
	systray.Run(u.onReady, func() {})
	return 0
}

// ui owns the menu and the state it renders. All mutable state sits behind mu;
// the menu is only ever touched from render, which serialises through the
// refresh loop.
type ui struct {
	profileFlag string
	mgr         *Manager
	refresh     chan struct{}
	quitOnce    sync.Once
	formMu      sync.Mutex
	formOpen    bool

	mu          sync.Mutex
	cfg         *client.Config
	profileName string
	profile     client.Profile
	profileErr  error
	remote      []client.Tunnel
	remoteErr   error

	header       *systray.MenuItem
	saved        []*slot
	savedEmpty   *systray.MenuItem
	remoteHeader *systray.MenuItem
	remotes      []*slot
	newItem      *systray.MenuItem
	profileItem  *systray.MenuItem
	profiles     []*systray.MenuItem
	profileNames []string
	editItem     *systray.MenuItem
	quitItem     *systray.MenuItem
}

// slot is one reusable tunnel entry with its submenu.
type slot struct {
	item   *systray.MenuItem
	copy   *systray.MenuItem
	open   *systray.MenuItem
	action *systray.MenuItem
	forget *systray.MenuItem

	mu     sync.Mutex
	key    string // saved name, or remote tunnel id
	target string // public URL or host:port, "" when not yet known
	isHTTP bool
}

func (s *slot) bind(key, target string, isHTTP bool) {
	s.mu.Lock()
	s.key, s.target, s.isHTTP = key, target, isHTTP
	s.mu.Unlock()
}

func (s *slot) binding() (key, target string, isHTTP bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.target, s.isHTTP
}

func newUI(profileFlag string) *ui {
	u := &ui{
		profileFlag: profileFlag,
		refresh:     make(chan struct{}, 1),
		cfg:         &client.Config{Profiles: map[string]client.Profile{}, Tunnels: map[string]client.SavedTunnel{}},
	}
	u.mgr = NewManager(u.requestRefresh)
	return u
}

// requestRefresh asks for a redraw without blocking; bursts collapse into one.
func (u *ui) requestRefresh() {
	select {
	case u.refresh <- struct{}{}:
	default:
	}
}

func (u *ui) onReady() {
	systray.SetTemplateIcon(trayIcons(StateStopped))
	systray.SetTooltip("zerock")
	u.buildMenu()
	u.reload()
	u.render()

	log.Printf("%s · config %s", version.String(), client.ConfigPath())
	u.mu.Lock()
	if u.profileErr != nil {
		log.Printf("no profile: %v", u.profileErr)
	} else {
		log.Printf("profile %s (%s)", u.profileName, u.profile.Host)
	}
	autostart := u.cfg.TunnelNames()
	cfg := u.cfg
	u.mu.Unlock()

	for _, name := range autostart {
		if cfg.Tunnels[name].Autostart {
			u.start(name)
		}
	}

	go u.loop()
}

// buildMenu creates every item once. Entries that vary are slots, shown or
// hidden by render.
func (u *ui) buildMenu() {
	u.header = systray.AddMenuItem("zerock", "Open the dashboard")
	u.onClick(u.header, u.openDashboard)
	systray.AddSeparator()

	u.savedEmpty = systray.AddMenuItem("No saved tunnels yet", "Use \"New tunnel…\" below")
	u.savedEmpty.Disable()
	for i := 0; i < savedSlots; i++ {
		s := newSlot("Stop", "Forget")
		u.saved = append(u.saved, s)
		u.onClick(s.copy, func() { u.copySlot(s) })
		u.onClick(s.open, func() { u.openSlot(s) })
		u.onClick(s.action, func() { u.toggleSaved(s) })
		u.onClick(s.forget, func() { u.forgetSaved(s) })
	}
	systray.AddSeparator()

	u.remoteHeader = systray.AddMenuItem("Other tunnels on this token", "Tunnels opened elsewhere with the same token")
	u.remoteHeader.Disable()
	for i := 0; i < remoteSlots; i++ {
		s := newSlot("Close tunnel", "")
		u.remotes = append(u.remotes, s)
		u.onClick(s.copy, func() { u.copySlot(s) })
		u.onClick(s.open, func() { u.openSlot(s) })
		u.onClick(s.action, func() { u.closeRemote(s) })
	}
	systray.AddSeparator()

	u.newItem = systray.AddMenuItem("New tunnel…", "Save and open a tunnel")
	u.onClick(u.newItem, u.newTunnel)
	u.profileItem = systray.AddMenuItem("Profile", "Which server and token to use")
	for i := 0; i < profSlots; i++ {
		item := u.profileItem.AddSubMenuItemCheckbox("", "", false)
		item.Hide()
		u.profiles = append(u.profiles, item)
		idx := i
		u.onClick(item, func() { u.selectProfile(idx) })
	}
	u.editItem = systray.AddMenuItem("Edit config…", "Open "+client.ConfigPath())
	u.onClick(u.editItem, func() {
		if err := openTarget(client.ConfigPath()); err != nil {
			u.fail("Could not open the config", err)
		}
	})
	systray.AddSeparator()

	u.quitItem = systray.AddMenuItem("Quit zerock", "Stop every tunnel started here and exit")
	u.onClick(u.quitItem, u.quit)
}

func newSlot(actionTitle, forgetTitle string) *slot {
	s := &slot{}
	s.item = systray.AddMenuItem("", "")
	s.item.Hide()
	s.copy = s.item.AddSubMenuItem("Copy URL", "Copy the public address")
	s.open = s.item.AddSubMenuItem("Open in browser", "")
	s.action = s.item.AddSubMenuItem(actionTitle, "")
	if forgetTitle != "" {
		s.item.AddSeparator()
		s.forget = s.item.AddSubMenuItem(forgetTitle, "Remove from the saved tunnels")
	}
	return s
}

// onClick runs fn each time the item is clicked. Items live for the whole
// process, so one goroutine per item is fine.
func (u *ui) onClick(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			fn()
		}
	}()
}

// loop redraws on demand and polls the server for the token's other tunnels.
func (u *ui) loop() {
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	u.pollRemote()
	u.render()

	for {
		select {
		case <-u.refresh:
			// Coalesce the burst a busy tunnel produces into one redraw.
			time.Sleep(150 * time.Millisecond)
			for drained := false; !drained; {
				select {
				case <-u.refresh:
				default:
					drained = true
				}
			}
			u.render()
		case <-poll.C:
			u.reload()
			u.pollRemote()
			u.render()
		}
	}
}

// reload re-reads the config so logins and edits made from the CLI show up
// without restarting the widget.
func (u *ui) reload() {
	cfg, err := client.LoadConfig()
	if err != nil {
		log.Printf("config: %v", err)
		return
	}
	name, prof, perr := cfg.Resolve(u.profileFlag)

	u.mu.Lock()
	u.cfg = cfg
	u.profileName, u.profile, u.profileErr = name, prof, perr
	u.mu.Unlock()
}

// pollRemote fetches the token's live tunnels from the server.
func (u *ui) pollRemote() {
	u.mu.Lock()
	prof, perr := u.profile, u.profileErr
	u.mu.Unlock()
	if perr != nil {
		u.mu.Lock()
		u.remote, u.remoteErr = nil, nil
		u.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tunnels, err := client.NewAPI(prof).ListTunnels(ctx, true)

	u.mu.Lock()
	u.remote, u.remoteErr = tunnels, err
	u.mu.Unlock()
}

// render pushes the current state into the menu and the icon.
func (u *ui) render() {
	u.mu.Lock()
	cfg, profName, prof, perr := u.cfg, u.profileName, u.profile, u.profileErr
	remote, rerr := u.remote, u.remoteErr
	u.mu.Unlock()
	statuses := u.mgr.Snapshot()

	// Header: who we are.
	if perr != nil {
		u.header.SetTitle("zerock · not logged in")
		u.header.SetTooltip("Run: zerock login --server <host> --token zk_…")
	} else {
		u.header.SetTitle("zerock · " + profName)
		u.header.SetTooltip("Open the dashboard at " + prof.APIURL(""))
	}

	// Saved tunnels.
	names := cfg.TunnelNames()
	if len(names) == 0 {
		u.savedEmpty.Show()
	} else {
		u.savedEmpty.Hide()
	}
	for i, s := range u.saved {
		if i >= len(names) {
			s.item.Hide()
			s.bind("", "", false)
			continue
		}
		name := names[i]
		spec := cfg.Tunnels[name]
		st, running := statuses[name]
		title, tooltip := savedTitle(name, spec, st, running)
		s.item.SetTitle(title)
		s.item.SetTooltip(tooltip)
		s.bind(name, st.Target, spec.Type == "http")
		if st.Target != "" && st.State == StateUp {
			s.copy.Enable()
			if spec.Type == "http" {
				s.open.Enable()
			} else {
				s.open.Disable()
			}
		} else {
			s.copy.Disable()
			s.open.Disable()
		}
		if running && st.Active() {
			s.action.SetTitle("Stop")
		} else {
			s.action.SetTitle("Start")
		}
		s.item.Show()
	}
	if len(names) > savedSlots {
		log.Printf("%d saved tunnels, showing the first %d", len(names), savedSlots)
	}

	// The token's other tunnels, minus the ones running here.
	mine := u.mgr.TunnelIDs()
	var others []client.Tunnel
	for _, t := range remote {
		if !mine[t.ID] {
			others = append(others, t)
		}
	}
	switch {
	case perr != nil:
		u.remoteHeader.SetTitle("Other tunnels: log in first")
	case rerr != nil:
		u.remoteHeader.SetTitle("Other tunnels: " + shorten(rerr.Error(), 60))
	case len(others) == 0:
		u.remoteHeader.SetTitle("No other tunnels on this token")
	default:
		u.remoteHeader.SetTitle(fmt.Sprintf("Other tunnels on this token (%d)", len(others)))
	}
	for i, s := range u.remotes {
		if i >= len(others) {
			s.item.Hide()
			s.bind("", "", false)
			continue
		}
		t := others[i]
		s.item.SetTitle(fmt.Sprintf("%s  ·  %s  ·  up %s", t.PublicLabel(), orUnknown(t.AgentHost), t.Uptime))
		s.item.SetTooltip(fmt.Sprintf("%s → %s on %s · %d requests", t.PublicLabel(), t.LocalAddr,
			orUnknown(t.AgentHost), t.Stats.Requests))
		s.bind(t.ID, t.URL, t.Type == "http")
		s.copy.Enable()
		if t.Type == "http" {
			s.open.Enable()
		} else {
			s.open.Disable()
		}
		s.item.Show()
	}

	// Profiles.
	profNames := cfg.Names()
	u.mu.Lock()
	u.profileNames = profNames
	u.mu.Unlock()
	if len(profNames) == 0 {
		u.profileItem.SetTitle("Profile: none · run zerock login")
		u.profileItem.Disable()
	} else {
		u.profileItem.SetTitle("Profile: " + orUnknown(profName))
		u.profileItem.Enable()
	}
	for i, item := range u.profiles {
		if i >= len(profNames) {
			item.Hide()
			continue
		}
		p := cfg.Profiles[profNames[i]]
		item.SetTitle(profNames[i])
		item.SetTooltip(p.ControlAddr())
		if profNames[i] == profName {
			item.Check()
		} else {
			item.Uncheck()
		}
		item.Show()
	}

	// Icon and tooltip.
	state, up := Summary(statuses)
	systray.SetTemplateIcon(trayIcons(state))
	switch {
	case perr != nil:
		systray.SetTooltip("zerock · not logged in")
	case up == 0:
		systray.SetTooltip("zerock · " + profName + " · no tunnels running here")
	default:
		systray.SetTooltip(fmt.Sprintf("zerock · %s · %d tunnel%s up", profName, up, plural(up)))
	}
	if runtime.GOOS == "darwin" {
		// A count next to the icon is normal on the macOS menu bar; Linux
		// panels ignore or misplace the title, so it stays empty there.
		if up > 0 {
			systray.SetTitle(strconv.Itoa(up))
		} else {
			systray.SetTitle("")
		}
	}
}

// savedTitle renders one saved tunnel's menu line and tooltip.
func savedTitle(name string, spec client.SavedTunnel, st Status, running bool) (title, tooltip string) {
	local := ":" + strconv.Itoa(spec.Port)
	if spec.Host != "" {
		local = spec.LocalAddr()
	}
	if !running || st.State == StateStopped {
		return fmt.Sprintf("○ %s  ·  %s", name, spec.Args()), "Stopped · open the submenu to start"
	}
	switch st.State {
	case StateConnecting:
		return fmt.Sprintf("◌ %s  ·  connecting…", name), spec.Args()
	case StateUp:
		reqs := fmt.Sprintf("%d req", st.Requests)
		if st.Requests == 1 {
			reqs = "1 req"
		}
		return fmt.Sprintf("● %s → %s  ·  %s", strings.TrimPrefix(strings.TrimPrefix(st.Target, "https://"), "http://"), local, reqs),
			fmt.Sprintf("%s · up %s · tunnel %s", st.Target, time.Since(st.Since).Truncate(time.Second), st.TunnelID)
	case StateReconnecting:
		return fmt.Sprintf("◌ %s  ·  reconnecting: %s", name, shorten(st.Err, 50)), st.Err
	case StateFailed:
		return fmt.Sprintf("✕ %s  ·  %s", name, shorten(st.Err, 60)), st.Err + " · open the submenu to retry"
	}
	return name, ""
}

// start opens a saved tunnel under the profile it names, or the current one.
func (u *ui) start(name string) {
	u.mu.Lock()
	cfg := u.cfg
	spec, ok := cfg.Tunnels[name]
	profileName := u.profileFlag
	u.mu.Unlock()
	if !ok {
		return
	}
	if spec.Profile != "" {
		profileName = spec.Profile
	}
	_, prof, err := cfg.Resolve(profileName)
	if err != nil {
		u.fail("Cannot start "+name, err)
		return
	}
	log.Printf("starting %s (%s) via %s", name, spec.Args(), prof.Host)
	u.mgr.Start(name, spec, prof)
}

func (u *ui) toggleSaved(s *slot) {
	name, _, _ := s.binding()
	if name == "" {
		return
	}
	if st, ok := u.mgr.Snapshot()[name]; ok && st.Active() {
		log.Printf("stopping %s", name)
		u.mgr.Stop(name)
		u.requestRefresh()
		go func() { u.pollRemote(); u.requestRefresh() }()
		return
	}
	u.start(name)
}

func (u *ui) forgetSaved(s *slot) {
	name, _, _ := s.binding()
	if name == "" {
		return
	}
	u.mgr.Forget(name)
	u.mu.Lock()
	delete(u.cfg.Tunnels, name)
	err := u.cfg.Save()
	u.mu.Unlock()
	if err != nil {
		u.fail("Could not save the config", err)
	}
	log.Printf("forgot %s", name)
	u.requestRefresh()
}

func (u *ui) closeRemote(s *slot) {
	id, _, _ := s.binding()
	if id == "" {
		return
	}
	u.mu.Lock()
	prof, perr := u.profile, u.profileErr
	u.mu.Unlock()
	if perr != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := client.NewAPI(prof).CloseTunnel(ctx, id); err != nil {
		u.fail("Could not close tunnel "+id, err)
		return
	}
	log.Printf("closed tunnel %s", id)
	u.pollRemote()
	u.requestRefresh()
}

func (u *ui) copySlot(s *slot) {
	_, target, _ := s.binding()
	if target == "" {
		return
	}
	if err := copyText(target); err != nil {
		u.fail("Could not copy", err)
	}
}

func (u *ui) openSlot(s *slot) {
	_, target, isHTTP := s.binding()
	if target == "" || !isHTTP {
		return
	}
	if err := openTarget(target); err != nil {
		u.fail("Could not open "+target, err)
	}
}

func (u *ui) openDashboard() {
	u.mu.Lock()
	prof, perr := u.profile, u.profileErr
	u.mu.Unlock()
	if perr != nil {
		alert("Not logged in", "Run in a terminal:\n\nzerock login --server <host> --token zk_…")
		return
	}
	if err := openTarget(prof.APIURL("")); err != nil {
		u.fail("Could not open the dashboard", err)
	}
}

// newTunnel shows the form, then saves and starts what comes back. Only one
// form is open at a time.
func (u *ui) newTunnel() {
	u.formMu.Lock()
	if u.formOpen {
		u.formMu.Unlock()
		return
	}
	u.formOpen = true
	u.formMu.Unlock()
	defer func() {
		u.formMu.Lock()
		u.formOpen = false
		u.formMu.Unlock()
	}()

	name, spec, ok, err := askForm()
	if err != nil {
		u.fail("Could not show the new-tunnel form", err)
		return
	}
	if !ok {
		return
	}

	u.mu.Lock()
	name = uniqueName(name, u.cfg.Tunnels)
	u.cfg.Tunnels[name] = spec
	err = u.cfg.Save()
	u.mu.Unlock()
	if err != nil {
		u.fail("Could not save the config", err)
		return
	}
	log.Printf("saved %s (%s)", name, spec.Args())
	u.start(name)
}

// selectProfile makes a profile the default, for the CLI as well.
func (u *ui) selectProfile(idx int) {
	u.mu.Lock()
	if idx >= len(u.profileNames) {
		u.mu.Unlock()
		return
	}
	name := u.profileNames[idx]
	u.cfg.Default = name
	err := u.cfg.Save()
	u.mu.Unlock()
	if err != nil {
		u.fail("Could not save the config", err)
		return
	}
	if u.profileFlag != "" {
		// --profile pins the widget; the default still changed for the CLI.
		log.Printf("default profile is now %s (this widget stays on --profile %s)", name, u.profileFlag)
	} else {
		log.Printf("profile %s", name)
	}
	u.reload()
	u.pollRemote()
	u.requestRefresh()
}

// fail logs an error and shows it, since there is no terminal to read.
func (u *ui) fail(title string, err error) {
	log.Printf("%s: %v", title, err)
	alert(title, err.Error())
}

func (u *ui) quit() {
	u.quitOnce.Do(func() {
		log.Printf("stopping tunnels")
		u.mgr.StopAll()
		systray.Quit()
	})
}

func shorten(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
