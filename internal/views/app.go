package views

import (
	"context"
	"fmt"
	"time"

	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/k8s"
	"github.com/derailed/k9s/internal/watch"
	"github.com/derailed/tview"
	"github.com/fsnotify/fsnotify"
	"github.com/gdamore/tcell"
	"github.com/rs/zerolog/log"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

const (
	splashTime = 1
	devMode    = "dev"
)

type (
	focusHandler func(tview.Primitive)

	igniter interface {
		tview.Primitive

		getTitle() string
		init(ctx context.Context, ns string)
	}

	keyHandler interface {
		keyboard(evt *tcell.EventKey) *tcell.EventKey
	}

	actionsFn func(keyActions)

	resourceViewer interface {
		igniter

		setEnterFn(enterFn)
		setColorerFn(colorerFn)
		setDecorateFn(decorateFn)
		setExtraActionsFn(actionsFn)
	}

	appView struct {
		*tview.Application

		config          *config.Config
		styles          *config.Styles
		version         string
		flags           *genericclioptions.ConfigFlags
		pages           *tview.Pages
		content         *tview.Pages
		flashView       *flashView
		crumbsView      *crumbsView
		menuView        *menuView
		clusterInfoView *clusterInfoView
		command         *command
		focusGroup      []tview.Primitive
		focusCurrent    int
		focusChanged    focusHandler
		cancel          context.CancelFunc
		cmdBuff         *cmdBuff
		cmdView         *cmdView
		actions         keyActions
		stopCh          chan struct{}
		informer        *watch.Meta
	}
)

// NewApp returns a K9s app instance.
func NewApp(cfg *config.Config) *appView {
	v := appView{Application: tview.NewApplication(), config: cfg}
	{
		v.refreshStyles()
		v.pages = tview.NewPages()
		v.actions = make(keyActions)
		v.menuView = newMenuView(&v)
		v.content = tview.NewPages()
		v.cmdBuff = newCmdBuff(':')
		v.cmdView = newCmdView(&v, '🐶')
		v.command = newCommand(&v)
		v.flashView = newFlashView(&v, "Initializing...")
		v.crumbsView = newCrumbsView(&v)
		v.clusterInfoView = newClusterInfoView(&v, k8s.NewMetricsServer(cfg.GetConnection()))
		v.focusChanged = v.changedFocus
		v.SetInputCapture(v.keyboard)
	}

	v.actions[KeyColon] = newKeyAction("Cmd", v.activateCmd, false)
	v.actions[tcell.KeyCtrlR] = newKeyAction("Redraw", v.redrawCmd, false)
	v.actions[tcell.KeyCtrlC] = newKeyAction("Quit", v.quitCmd, false)
	v.actions[KeyHelp] = newKeyAction("Help", v.helpCmd, false)
	v.actions[tcell.KeyCtrlA] = newKeyAction("Aliases", v.aliasCmd, true)
	v.actions[tcell.KeyEscape] = newKeyAction("Exit Cmd", v.deactivateCmd, false)
	v.actions[tcell.KeyEnter] = newKeyAction("Goto", v.gotoCmd, false)
	v.actions[tcell.KeyBackspace2] = newKeyAction("Erase", v.eraseCmd, false)
	v.actions[tcell.KeyBackspace] = newKeyAction("Erase", v.eraseCmd, false)
	v.actions[tcell.KeyDelete] = newKeyAction("Erase", v.eraseCmd, false)

	return &v
}

func (a *appView) Init(v string, rate int, flags *genericclioptions.ConfigFlags) {
	a.version = v
	a.flags = flags
	a.startInformer()
	a.clusterInfoView.init()
	a.cmdBuff.addListener(a.cmdView)

	header := tview.NewFlex()
	{
		header.SetDirection(tview.FlexColumn)
		header.AddItem(a.clusterInfoView, 35, 1, false)
		header.AddItem(a.menuView, 0, 1, false)
		header.AddItem(a.logoView(), 26, 1, false)
	}

	main := tview.NewFlex()
	{
		main.SetDirection(tview.FlexRow)
		main.AddItem(header, 7, 1, false)
		main.AddItem(a.cmdView, 3, 1, false)
		main.AddItem(a.content, 0, 10, true)
		main.AddItem(a.crumbsView, 2, 1, false)
		main.AddItem(a.flashView, 1, 1, false)
	}

	a.pages.AddPage("main", main, true, false)
	a.pages.AddPage("splash", newSplash(a), true, true)
	a.SetRoot(a.pages, true)
}

func (a *appView) startInformer() {
	if a.stopCh != nil {
		close(a.stopCh)
	}

	a.stopCh = make(chan struct{})
	ns := ""
	if a.flags.Namespace != nil {
		ns = *a.flags.Namespace
	}
	a.informer = watch.NewMeta(a.conn(), ns)
	log.Debug().Msgf(">> Starting Watcher")
	a.informer.Run(a.stopCh)
}

func (a *appView) bailOut() {
	if a.stopCh != nil {
		log.Debug().Msg("<<<< Stopping Watcher")
		close(a.stopCh)
	}
	a.Stop()
}

func (a *appView) conn() k8s.Connection {
	return a.config.GetConnection()
}

func (a *appView) stylesUpdater() (*fsnotify.Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return w, err
	}

	go func() {
		for {
			select {
			case evt := <-w.Events:
				_ = evt
				a.QueueUpdateDraw(func() {
					a.refreshStyles()
				})
			case err := <-w.Errors:
				log.Info().Err(err).Msg("Skin watcher failed")
			}
		}
	}()

	if err := w.Add(config.K9sStylesFile); err != nil {
		return w, err
	}

	return w, nil
}

// Run starts the application loop
func (a *appView) Run() {
	// Only enable updater while in dev mode.
	if a.version == devMode {
		w, err := a.stylesUpdater()
		defer func() {
			if err != nil {
				w.Close()
			}
		}()
	}

	go func() {
		<-time.After(splashTime * time.Second)
		a.QueueUpdateDraw(func() {
			a.showPage("main")
		})
	}()

	a.command.defaultCmd()
	if err := a.Application.Run(); err != nil {
		panic(err)
	}
}

func (a *appView) keyboard(evt *tcell.EventKey) *tcell.EventKey {
	key := evt.Key()
	if key == tcell.KeyRune {
		if a.cmdBuff.isActive() {
			a.cmdBuff.add(evt.Rune())
			return nil
		}
		key = tcell.Key(evt.Rune())
	}

	if a, ok := a.actions[key]; ok {
		log.Debug().Msgf(">> AppView handled key: %s", tcell.KeyNames[key])
		return a.action(evt)
	}

	return evt
}

func (a *appView) rbacCmd(evt *tcell.EventKey) *tcell.EventKey {
	a.inject(newRBACView(a, "", "aa_k9s", clusterRole))
	return evt
}

func (a *appView) redrawCmd(evt *tcell.EventKey) *tcell.EventKey {
	a.Draw()
	return evt
}

func (a *appView) focusCmd(evt *tcell.EventKey) *tcell.EventKey {
	a.nextFocus()
	return evt
}

func (a *appView) eraseCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdBuff.isActive() {
		a.cmdBuff.del()
		return nil
	}
	return evt
}

func (a *appView) deactivateCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdBuff.isActive() {
		a.cmdBuff.reset()
	}
	return evt
}

func (a *appView) prevCmd(evt *tcell.EventKey) *tcell.EventKey {
	if top, ok := a.command.previousCmd(); ok {
		log.Debug().Msgf("Previous command %s", top)
		a.gotoResource(top, false)
		return nil
	}
	return evt
}

func (a *appView) gotoCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdBuff.isActive() && !a.cmdBuff.empty() {
		a.gotoResource(a.cmdBuff.String(), true)
		a.cmdBuff.reset()
		return nil
	}
	a.cmdBuff.setActive(false)
	return evt
}

func (a *appView) activateCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdView.inCmdMode() {
		return evt
	}
	a.flash(flashInfo, "Command mode activated.")
	log.Debug().Msg("Entering command mode...")
	a.cmdBuff.setActive(true)
	a.cmdBuff.clear()
	return nil
}

func (a *appView) quitCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdMode() {
		return evt
	}
	a.Stop()
	return nil
}

func (a *appView) helpCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdView.inCmdMode() {
		return evt
	}
	a.inject(newHelpView(a))
	return nil
}

func (a *appView) aliasCmd(evt *tcell.EventKey) *tcell.EventKey {
	if a.cmdView.inCmdMode() {
		return evt
	}
	a.inject(newAliasView(a))
	return nil
}

func (a *appView) noopCmd(*tcell.EventKey) *tcell.EventKey {
	return nil
}

func (a *appView) puntCmd(evt *tcell.EventKey) *tcell.EventKey {
	return evt
}

func (a *appView) gotoResource(res string, record bool) bool {
	valid := a.command.run(res)
	if valid && record {
		a.command.pushCmd(res)
	}
	return valid
}

func (a *appView) showPage(p string) {
	a.pages.SwitchToPage(p)
}

func (a *appView) inject(i igniter) {
	if a.cancel != nil {
		a.cancel()
	}
	a.content.RemovePage("main")

	var ctx context.Context
	{
		ctx, a.cancel = context.WithCancel(context.Background())
		i.init(ctx, a.config.ActiveNamespace())
	}
	a.content.AddPage("main", i, true, true)

	a.focusGroup = append([]tview.Primitive{}, i)
	a.focusCurrent = 0
	a.fireFocusChanged(i)
	a.SetFocus(i)
}

func (a *appView) cmdMode() bool {
	return a.cmdView.inCmdMode()
}

func (a *appView) flash(level flashLevel, m ...string) {
	a.flashView.setMessage(level, m...)
}

func (a *appView) setHints(h hints) {
	a.menuView.populateMenu(h)
}

func (a *appView) logoView() tview.Primitive {
	v := tview.NewTextView()
	{
		v.SetWordWrap(false)
		v.SetWrap(false)
		v.SetDynamicColors(true)
		for i, s := range LogoSmall {
			fmt.Fprintf(v, "[%s::b]%s", a.styles.Style.LogoColor, s)
			if i+1 < len(LogoSmall) {
				fmt.Fprintf(v, "\n")
			}
		}
	}
	return v
}

func (a *appView) fireFocusChanged(p tview.Primitive) {
	if a.focusChanged != nil {
		a.focusChanged(p)
	}
}

func (a *appView) changedFocus(p tview.Primitive) {
	switch p.(type) {
	case hinter:
		a.setHints(p.(hinter).hints())
	}
}

func (a *appView) nextFocus() {
	for i := range a.focusGroup {
		if i == a.focusCurrent {
			a.focusCurrent = 0
			victim := a.focusGroup[a.focusCurrent]
			if i+1 < len(a.focusGroup) {
				a.focusCurrent++
				victim = a.focusGroup[a.focusCurrent]
			}
			a.fireFocusChanged(victim)
			a.SetFocus(victim)
			return
		}
	}
	return
}

func (a *appView) refreshStyles() {
	var err error
	if a.styles, err = config.NewStyles(); err != nil {
		log.Warn().Err(err).Msg("No skin file found. Loading defaults.")
	}
	a.styles.Update()

	stdColor = config.AsColor(a.styles.Style.Status.NewColor)
	addColor = config.AsColor(a.styles.Style.Status.AddColor)
	modColor = config.AsColor(a.styles.Style.Status.ModifyColor)
	errColor = config.AsColor(a.styles.Style.Status.ErrorColor)
	highlightColor = config.AsColor(a.styles.Style.Status.HighlightColor)
	completedColor = config.AsColor(a.styles.Style.Status.CompletedColor)
}
