package main

import (
	"bytes"
	"container/list"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/gdamore/tcell/v2"
)

const (
	focusEditor = iota
	focusConsole
)

type App struct {
	data    *Model
	tab     View
	editor  []*View
	status  View
	console View
	cmdCh   chan string
	focus   int
	done    chan struct{}
}

// Model holds the application state.
type Model struct {
	tabs    []string
	tabIdx  int
	lines   *list.List
	status  string
	console *lineBuf

	scroll int // Scroll position for the editor
}

func (m *Model) addTab(s string) {
	i := slices.Index(m.tabs, s)
	if i >= 0 {
		m.tabIdx = i
		return
	}
	m.tabs = append(m.tabs, s)
	m.tabIdx = len(m.tabs) - 1
}

func (m *Model) deleteTab(s string) {
	for i, tab := range m.tabs {
		if tab == s {
			m.tabs = slices.Delete(m.tabs, i, i+1)
			if m.tabIdx >= len(m.tabs) {
				m.tabIdx = len(m.tabs) - 1
			}
			return
		}
	}
}

type lineBuf struct {
	line   []rune
	cursor int
}

func LineBuf(s string) *lineBuf {
	return &lineBuf{
		line:   []rune(s),
		cursor: len(s),
	}
}

func (l *lineBuf) set(s string) {
	l.line = []rune(s)
	l.cursor = len(l.line)
}

func (l *lineBuf) insert(r rune) {
	if l.cursor >= len(l.line) {
		l.line = append(l.line, r)
	} else {
		l.line = slices.Insert(l.line, l.cursor, r)
	}
	l.cursor++
}

func (l *lineBuf) delete() {
	if l.cursor > 0 {
		l.cursor--
		l.line = slices.Delete(l.line, l.cursor, l.cursor+1)
	}
}

// Move the cursor to the right, if possible.
func (l *lineBuf) right() {
	if l.cursor < len(l.line) {
		l.cursor++
	}
}

// Move the cursor to the left, if possible.
func (l *lineBuf) left() {
	if l.cursor > 0 {
		l.cursor--
	}
}

type View struct {
	x, y, w, h int
	style      tcell.Style
}

func (v *View) draw(line ...string) {
	for row := 0; row < v.h; row++ {
		for col := 0; col < v.w; col++ {
			if row < len(line) && col < len(line[row]) {
				screen.SetContent(v.x+col, v.y+row, rune(line[row][col]), nil, v.style)
			} else {
				screen.SetContent(v.x+col, v.y+row, ' ', nil, v.style)
			}
		}
	}
}

func (v *View) contains(x, y int) bool {
	return x >= v.x && x < v.x+v.w && y >= v.y && y < v.y+v.h
}

func (a *App) resize(width, height int) {
	a.tab = View{0, 0, width, 1, tcell.StyleDefault.Reverse(true)}
	a.editor = make([]*View, height-3)
	for i := range a.editor {
		a.editor[i] = &View{0, i + a.tab.h, width, 1, tcell.StyleDefault}
	}
	a.status = View{0, height - 2, width, 1, tcell.StyleDefault.Reverse(true)}
	a.console = View{0, height - 1, width, 1, tcell.StyleDefault}
}

const tabCloser = " x|"

func (a *App) draw() {
	a.drawTabs()
	a.drawEditor()
	a.status.draw(a.data.status)
	a.console.draw("> " + string(a.data.console.line))
}

func (a *App) drawTabs() {
	if len(a.data.tabs) == 0 {
		a.tab.draw("New Tab")
		return
	}

	var col int
	for i, tab := range a.data.tabs {
		style := a.tab.style.Background(tcell.ColorDarkGray)
		if i == a.data.tabIdx {
			style = a.tab.style.Bold(true)
			// paper-like yellow color
			// style = style.Foreground(tcell.NewRGBColor(255, 249, 202))
		}
		for _, c := range tab + tabCloser {
			screen.SetContent(a.tab.x+col, a.tab.y, c, nil, style)
			col++
		}
	}

	// clear remaining space
	for i := col; i < a.tab.w; i++ {
		screen.SetContent(a.tab.x+i, a.tab.y, ' ', nil, a.tab.style)
	}
}

func (a *App) drawEditor() {
	line := a.data.lines.Front()
	for i, lineView := range a.editor {
		if i < a.data.lines.Len() {
			lineView.draw(line.Value.(string))
			line = line.Next()
		} else {
			lineView.draw("")
		}
	}
}

var screen tcell.Screen

func main() {
	output := os.Getenv("SEYI_LOG_FILE")
	if output != "" {
		f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file %s: %v", output, err)
		}
		defer f.Close()
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Initialize screen
	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("%+v", err)
	}
	if err := s.Init(); err != nil {
		log.Fatalf("%+v", err)
	}
	defStyle := tcell.StyleDefault.Background(tcell.ColorReset).Foreground(tcell.ColorReset)
	s.SetStyle(defStyle)
	s.EnableMouse()
	s.EnablePaste()
	s.Clear()
	screen = s

	quit := func() {
		// You have to catch panics in a defer, clean up, and
		// re-raise them - otherwise your application can
		// die without leaving any diagnostic trace.
		maybePanic := recover()
		s.Fini()
		if maybePanic != nil {
			panic(maybePanic)
		}
	}
	defer quit()

	app := &App{
		data: &Model{
			// tabs:    []string{"Tab1", "Tab2", "Tab3"},
			lines:   list.New(),
			status:  "Ready",
			console: LineBuf("Type a command here..."),
		},
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
	app.data.lines.PushBack("This is app simple text editor.")
	app.data.lines.PushBack("You can add more lines here.")
	go app.handleCommand()

	// Event loop
	for {
		// Update screen
		s.Show()
		// Poll event
		ev := s.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventResize: // arrive when the app start
			w, h := s.Size()
			app.resize(w, h)
			app.draw()
			s.Sync()
		case *tcell.EventKey:
			log.Printf("Key pressed: %s %c", tcell.KeyNames[ev.Key()], ev.Rune())
			if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyCtrlC {
				close(app.done)
				s.Fini()
				return
			}
			if ev.Key() == tcell.KeyCtrlP {
				app.focus = focusConsole
				app.setConsole("")
				app.syncCursor()
				continue
			}
			if app.focus == focusConsole {
				app.consoleInput(ev)
				app.syncCursor()
				continue
			}
		case *tcell.EventMouse:
			x, y := ev.Position()
			switch ev.Buttons() {
			case tcell.Button1: // left click
				app.handleClick(x, y)
			case tcell.ButtonNone: // drag
			}
		}
	}
}

func (a *App) handleClick(x, y int) {
	if a.tab.contains(x, y) {
		var width int
		for _, tab := range a.data.tabs {
			if x < a.tab.x+width+len(tab) {
				a.cmdCh <- "open " + tab
				return
			} else if x < a.tab.x+width+len(tab)+len(tabCloser) {
				a.cmdCh <- "close " + tab
				return
			}
			width += len(tab) + len(tabCloser)
		}
	} else if a.console.contains(x, y) {
		a.focus = focusConsole
		a.setConsole("")
		a.syncCursor()
	} else {
		a.focus = focusEditor
	}
}

// setStatus updates the status view with the given string.
func (a *App) setStatus(s string) {
	a.data.status = s
	a.status.draw(s)
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string) {
	a.data.console.set(s)
	a.console.draw("> " + s)
}

func (a *App) consoleInput(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEnter:
		if len(a.data.console.line) > 0 {
			a.cmdCh <- string(a.data.console.line)
			a.data.console.set("")
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(a.data.console.line) > 0 {
			a.data.console.delete()
		}
	case tcell.KeyLeft:
		a.data.console.left()
	case tcell.KeyRight:
		a.data.console.right()
	default:
		if ev.Rune() != 0 {
			a.data.console.insert(ev.Rune())
		}
	}
	a.console.draw("> " + string(a.data.console.line))
}

func (a *App) handleCommand() {
	for {
		select {
		case cmd := <-a.cmdCh:
			log.Println("Command received:", cmd)
			c := strings.Split(strings.TrimSpace(cmd), " ")
			switch c[0] {
			case "open":
				if len(c) == 1 {
					a.setStatus("Usage: open <filename>")
					continue
				}
				filename := c[1]
				bs, err := os.ReadFile(filename)
				if err != nil {
					log.Println(err)
					continue
				}
				a.data.addTab(filename)
				a.data.lines.Init()
				for _, line := range bytes.Split(bs, []byte{'\n'}) {
					a.data.lines.PushBack(string(line))
				}
				a.focus = focusEditor
				a.draw()
				a.syncCursor()
				screen.Show()
			case "close":
				if len(c) == 1 {
					a.setStatus("Usage: close <filename>")
					continue
				}
				filename := c[1]
				currentTab := a.data.tabs[a.data.tabIdx]
				a.data.deleteTab(filename)
				if filename != currentTab {
					// closed other tab, just redraw
					a.drawTabs()
					continue
				}
				if len(a.data.tabs) == 0 {
					a.data.lines.Init()
					a.draw()
					screen.Show()
					continue
				}
				bs, err := os.ReadFile(a.data.tabs[a.data.tabIdx])
				if err != nil {
					log.Println(err)
					continue
				}
				a.data.lines.Init()
				for _, line := range bytes.Split(bs, []byte{'\n'}) {
					a.data.lines.PushBack(string(line))
				}
				a.draw()
				screen.Show()
			}
		case <-a.done:
			return
		}
	}
}

func (a *App) syncCursor() {
	if a.focus == focusConsole {
		screen.ShowCursor(a.console.x+2+a.data.console.cursor, a.console.y)
	} else {
		screen.HideCursor()
	}
}
