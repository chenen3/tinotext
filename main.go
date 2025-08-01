package main

import (
	"bytes"
	"container/list"
	"fmt"
	"io"
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
	editor  []*View // viewport
	status  View
	console View
	cmdCh   chan string
	focus   int
	done    chan struct{}
}

// Model holds the application state.
type Model struct {
	tabs   []string
	tabIdx int

	lines  *list.List
	row    int // start from 0
	col    int // start from 0
	scroll int // Scroll position for the editor

	status  string
	console *lineBuf
}

// line return the lineBuf in list
func (m *Model) line(i int) *lineBuf {
	if m.lines.Len() == 0 || i > m.lines.Len()-1 {
		return nil
	}

	l := m.lines.Front()
	for range i {
		l = l.Next()
	}
	return l.Value.(*lineBuf)
}

func (m *Model) openTab(s string) error {
	bs, err := os.ReadFile(s)
	if err != nil {
		return err
	}

	i := slices.Index(m.tabs, s)
	if i < 0 {
		m.tabs = append(m.tabs, s)
		m.tabIdx = len(m.tabs) - 1
	} else {
		m.tabIdx = i
	}

	m.lines.Init()
	for _, line := range bytes.Split(bs, []byte{'\n'}) {
		m.lines.PushBack(LineBuf(string(line)))
	}
	m.scroll = 0
	m.row = 0
	m.col = 0
	return nil
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

func (l *lineBuf) String() string {
	return string(l.line)
}

func (l *lineBuf) set(s string) {
	l.line = []rune(s)
	l.cursor = len(l.line)
}

// insert at inner cursor
func (l *lineBuf) insert(r rune) {
	if l.cursor >= len(l.line) {
		l.line = append(l.line, r)
	} else {
		l.line = slices.Insert(l.line, l.cursor, r)
	}
	l.cursor++
}

// backspace delete backwards
func (l *lineBuf) backspace() {
	if l.cursor == 0 {
		return
	}
	l.line = slices.Delete(l.line, l.cursor-1, l.cursor)
	l.cursor--
}

// Move the cursor to the right, if possible.
func (l *lineBuf) right() {
	if l.cursor < len(l.line) {
		l.cursor++
	}
}

// Move the cursor to the left, if possible.
func (l *lineBuf) left() {
	if l.cursor == 0 {
		return
	}
	l.cursor--
}

// seek move the cursor and return it
func (l *lineBuf) seek(i int) int {
	if i < 0 {
		i = 0
	} else if i > len(l.line)-1 {
		// line end
		i = len(l.line)
	}
	l.cursor = i
	return i
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
	for range a.data.scroll {
		line = line.Next()
	}
	remainlines := a.data.lines.Len() - a.data.scroll
	for i, lineView := range a.editor {
		if i < remainlines {
			lineView.draw(line.Value.(*lineBuf).String())
			line = line.Next()
		} else {
			lineView.draw("")
		}
	}
}

var screen tcell.Screen

func main() {
	output := os.Getenv("SEYI_LOG_FILE")
	if output == "" {
		log.SetOutput(io.Discard)
	} else {
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
			lines:   list.New(),
			status:  "Ready",
			console: LineBuf("Type a command here..."),
		},
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
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
				app.consoleEvent(ev)
				app.syncCursor()
				continue
			}
			app.editorEvent(ev)
		case *tcell.EventMouse:
			x, y := ev.Position()
			switch ev.Buttons() {
			case tcell.Button1: // left click
				app.handleClick(x, y)
			case tcell.ButtonNone: // drag
			case tcell.WheelUp:
				app.data.scroll -= int(float32(y) * scrollFactor)
				if app.data.scroll < 0 {
					app.data.scroll = 0
				}
				app.drawEditor()
				app.syncCursor()
			case tcell.WheelDown:
				app.data.scroll += int(float32(y) * scrollFactor)
				// keep it viewport
				if app.data.scroll > app.data.lines.Len()-len(app.editor) {
					app.data.scroll = app.data.lines.Len() - len(app.editor)
				}
				app.drawEditor()
				app.syncCursor()
			}
		}
	}
}

// A multiplier to be used on scrolling
const scrollFactor = 0.125

func (a *App) handleClick(x, y int) {
	if a.tab.contains(x, y) {
		var width int
		for i, tab := range a.data.tabs {
			if x < a.tab.x+width+len(tab) {
				if i != a.data.tabIdx {
					a.cmdCh <- "open " + tab
				}
				return
			} else if x < a.tab.x+width+len(tab)+len(tabCloser) {
				a.cmdCh <- "close " + tab
				return
			}
			width += len(tab) + len(tabCloser)
		}
		return
	}

	if a.console.contains(x, y) {
		a.focus = focusConsole
		a.setConsole("")
		a.syncCursor()
		return
	}

	a.focus = focusEditor
	if a.data.lines.Len() == 0 {
		a.data.row = 0
		a.data.col = 0
	} else {
		row := y - a.editor[0].y + a.data.scroll
		if row > a.data.lines.Len()-1 {
			row = a.data.lines.Len() - 1
		}
		a.data.row = row
		line := a.data.line(a.data.row)
		col := x - a.editor[0].x
		a.data.col = line.seek(col)
	}
	a.setStatus(fmt.Sprintf("Row %d, Column %d", a.data.row+1, a.data.col+1))
	a.syncCursor()
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

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEnter:
		if len(a.data.console.line) > 0 {
			a.cmdCh <- string(a.data.console.line)
			a.data.console.set("")
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(a.data.console.line) > 0 {
			a.data.console.backspace()
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

/*
>open file
>close tab
>find word
:line
@symbol
*/
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
				err := a.data.openTab(filename)
				if err != nil {
					log.Println(err)
					a.setStatus(err.Error())
					screen.Show()
					continue
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
					a.data.lines.PushBack(LineBuf(string(line)))
				}
				a.draw()
				screen.Show()
			default:
				a.setStatus("unknown command: " + cmd)
				screen.Show()
			}
		case <-a.done:
			return
		}
	}
}

func (a *App) syncCursor() {
	switch a.focus {
	case focusEditor:
		if a.data.row < a.data.scroll || a.data.row > (a.data.scroll+len(a.editor)-1) {
			// out of viewport
			screen.HideCursor()
			return
		}
		screen.ShowCursor(a.editor[0].x+a.data.col, a.editor[0].y+a.data.row-a.data.scroll)
	case focusConsole:
		screen.ShowCursor(a.console.x+2+a.data.console.cursor, a.console.y)
	default:
		screen.HideCursor()
	}
}

// TODO
var prevKey tcell.Key
var prevCol int

func (a *App) editorEvent(ev *tcell.EventKey) {
	defer func() {
		a.syncCursor()
		a.setStatus(fmt.Sprintf("Row %d, Column %d", a.data.row+1, a.data.col+1))
		prevKey = ev.Key()
	}()
	switch ev.Key() {
	case tcell.KeyRune:
		line := a.data.line(a.data.row)
		if line == nil {
			line = LineBuf("")
			a.data.lines.PushBack(line)
		}
		line.insert(ev.Rune())
		a.data.col = line.seek(a.data.col + 1)
		a.editor[a.data.row-a.data.scroll].draw(line.String())
	case tcell.KeyEnter:
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// delete current line and move up
		if a.data.col == 0 {
			if a.data.row == 0 {
				return // no line to delete
			}
			l := a.data.lines.Front()
			for range a.data.row {
				l = l.Next()
			}
			currentLine := l
			prevLine := currentLine.Prev().Value.(*lineBuf)
			a.data.lines.Remove(currentLine)
			prevLine.line = append(prevLine.line,
				currentLine.Value.(*lineBuf).line...)
			a.data.col = prevLine.seek(len(prevLine.line))
			a.data.row--
			a.drawEditor()
			return
		}

		line := a.data.line(a.data.row)
		if line == nil {
			return
		}
		line.backspace()
		a.data.col = line.seek(a.data.col - 1)
		a.editor[a.data.row-a.data.scroll].draw(line.String())
	case tcell.KeyLeft:
		line := a.data.line(a.data.row)
		if line == nil {
			return
		}
		a.data.col = line.seek(a.data.col - 1)
	case tcell.KeyRight:
		line := a.data.line(a.data.row)
		if line == nil {
			return
		}
		a.data.col = line.seek(a.data.col + 1)
	case tcell.KeyUp:
		if a.data.row == 0 {
			return // already at the top
		}

		a.data.row--
		line := a.data.line(a.data.row)
		if line == nil {
			a.data.col = 0
		} else if prevKey == tcell.KeyUp {
			// if the previous key was also up, keep previous column
			a.data.col = line.seek(prevCol)
		} else {
			// keep current column
			a.data.col = line.seek(a.data.col)
		}
		if a.data.row < a.data.scroll {
			a.data.scroll--
			a.drawEditor()
			return
		}
	case tcell.KeyDown:
	}
}
