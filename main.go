package main

import (
	"bytes"
	"container/list"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
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
	done    chan struct{}
}

// Model holds the application state.
type Model struct {
	tabs   []string
	tabIdx int

	lines     *list.List
	row       int // start from 0
	col       int // start from 0
	scroll    int // Scroll position for the editor
	upDownCol int // ideal column when moving up/down

	status string

	console       string
	consoleCursor int

	focus int
}

// line return the lineBuf in list
func (m *Model) line(i int) *list.Element {
	if m.lines.Len() == 0 || i > m.lines.Len()-1 {
		return nil
	}

	l := m.lines.Front()
	for range i {
		l = l.Next()
	}
	return l
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
	buf    []rune
	cursor int
}

func LineBuf(s string) *lineBuf {
	return &lineBuf{
		buf:    []rune(s),
		cursor: len(s),
	}
}

func (l *lineBuf) String() string {
	return string(l.buf)
}

func (l *lineBuf) set(s string) {
	l.buf = []rune(s)
	l.cursor = len(l.buf)
}

// insert at inner cursor
func (l *lineBuf) insert(r rune) {
	if l.cursor >= len(l.buf) {
		l.buf = append(l.buf, r)
	} else {
		l.buf = slices.Insert(l.buf, l.cursor, r)
	}
	l.cursor++
}

// backspace delete backwards
func (l *lineBuf) backspace() {
	if l.cursor == 0 {
		return
	}
	l.buf = slices.Delete(l.buf, l.cursor-1, l.cursor)
	l.cursor--
}

// Move the cursor to the right, if possible.
func (l *lineBuf) right() {
	if l.cursor < len(l.buf) {
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
	} else if i > len(l.buf)-1 {
		// line end
		i = len(l.buf)
	}
	l.cursor = i
	return i
}

type View struct {
	x, y, w, h int
	style      tcell.Style
}

func (v *View) draw(line ...string) {
	for row := range v.h {
		for col := range v.w {
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

// draw the whole layout and cursor
func (a *App) draw() {
	a.drawTabs()
	a.drawEditor()
	a.status.draw(fmt.Sprintf("Line %d, Column %d", a.data.row+1, a.data.col+1))
	a.console.draw(a.data.console)
	a.syncCursor()
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
			lines:  list.New(),
			status: "Ready",
		},
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
	go app.commandLoop()

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
			if ev.Key() == tcell.KeyCtrlC {
				close(app.done)
				s.Fini()
				return
			}
			// redraw the screen, sometimes iTerm2 resize but doesn't trigger a resize event
			if ev.Key() == tcell.KeyCtrlL {
				s.Sync()
				continue
			}
			// close current tab
			if ev.Key() == tcell.KeyCtrlW {
				if len(app.data.tabs) == 0 {
					close(app.done)
					s.Fini()
					return
				}
				app.cmdCh <- ">close " + app.data.tabs[app.data.tabIdx]
				continue
			}
			// open file
			if ev.Key() == tcell.KeyCtrlP {
				app.data.focus = focusConsole
				app.setStatus("open file by name (append : to go to line or > to execute command)")
				app.setConsole("")
				app.syncCursor()
				continue
			}

			if app.data.focus == focusConsole {
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
const scrollFactor = 0.1

func (a *App) handleClick(x, y int) {
	if a.tab.contains(x, y) {
		var width int
		for i, tab := range a.data.tabs {
			if x < a.tab.x+width+len(tab) {
				if i != a.data.tabIdx {
					a.cmdCh <- tab
				}
				return
			} else if x < a.tab.x+width+len(tab)+len(tabCloser) {
				a.cmdCh <- ">close " + tab
				return
			}
			width += len(tab) + len(tabCloser)
		}
		return
	}

	if a.console.contains(x, y) {
		a.data.focus = focusConsole
		a.setConsole("")
		a.syncCursor()
		return
	}

	a.data.focus = focusEditor
	if a.data.lines.Len() == 0 {
		a.data.row = 0
		a.data.col = 0
	} else {
		a.data.row = min(y-a.editor[0].y+a.data.scroll, a.data.lines.Len()-1)
		line := a.data.line(a.data.row).Value.(*lineBuf)
		col := x - a.editor[0].x
		a.data.col = line.seek(col)
	}
	a.setStatus(fmt.Sprintf("Line %d, Column %d", a.data.row+1, a.data.col+1))
	a.syncCursor()
	a.data.upDownCol = -1
	// debug
	if line := a.data.line(a.data.row); line != nil {
		log.Printf("Clicked line %d, column %d, text: %q", a.data.row+1, a.data.col+1,
			line.Value.(*lineBuf))
	}
}

// setStatus updates the status view with the given string.
func (a *App) setStatus(s string) {
	a.data.status = s
	a.status.draw(s)
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string) {
	a.data.console = s
	a.data.consoleCursor = len(s)
	a.console.draw(s)
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		a.data.console = ""
		a.data.consoleCursor = 0
		a.data.focus = focusEditor
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.data.row+1, a.data.col+1))
	case tcell.KeyEnter:
		if a.data.console != "" {
			a.cmdCh <- strings.TrimSpace(a.data.console)
			a.data.console = ""
			a.data.consoleCursor = 0
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.data.console == "" {
			return
		}
		a.data.console = a.data.console[:len(a.data.console)-1]
		a.data.consoleCursor--
	case tcell.KeyLeft:
		if a.data.console == "" {
			return
		}
		a.data.consoleCursor--
	case tcell.KeyRight:
		if a.data.console == "" || a.data.consoleCursor >= len(a.data.console) {
			return
		}
		a.data.consoleCursor++
	case tcell.KeyRune:
		if a.data.consoleCursor >= len(a.data.console) {
			a.data.console += string(ev.Rune())
		} else {
			a.data.console = a.data.console[:a.data.consoleCursor] + string(ev.Rune()) + a.data.console[a.data.consoleCursor:]
		}
		a.data.consoleCursor++
	default:
		return
	}
	a.console.draw(a.data.console)
}

// form of command:
// filename
// :line
// >close tab
//
// TODO:
// >find word
// @symbol
func (a *App) handleCommand(cmd string) {
	// this function is called out of the main goroutine,
	// content changes here may not be visible in time,
	// so make sure to call screen.Show() after making changes
	defer screen.Show()
	switch cmd[0] {
	case '>':
		c := strings.Split(cmd[1:], " ")
		switch c[0] {
		case "close":
			if len(c) == 1 {
				a.setStatus("Usage: close <filename>")
				return
			}
			filename := c[1]
			currentTab := a.data.tabs[a.data.tabIdx]
			a.data.deleteTab(filename)
			// no tab left
			if len(a.data.tabs) == 0 {
				a.data.lines.Init()
				a.data.row = 0
				a.data.col = 0
				a.draw()
				return
			}
			// closed other tab
			if filename != currentTab {
				a.drawTabs()
				return
			}
			// switch to next tab
			err := a.data.openTab(a.data.tabs[a.data.tabIdx])
			if err != nil {
				a.setStatus(err.Error())
				return
			}
			a.draw()
		default:
			a.setStatus("unknown command: " + cmd)
		}
	case ':':
		line, err := strconv.Atoi(cmd[1:])
		if err != nil {
			a.setStatus("Invalid line number")
			return
		}
		if line < 1 || line > a.data.lines.Len() {
			a.setStatus("Line number out of range")
			return
		}
		a.data.row = line - 1
		a.data.col = 0
		a.data.scroll = line - 1
		a.data.focus = focusEditor
		a.draw()
	case '@':
	default:
		filename := cmd
		err := a.data.openTab(filename)
		if err != nil {
			a.setStatus(err.Error())
			screen.Show()
			return
		}

		a.data.focus = focusEditor
		a.draw()
	}
}

func (a *App) commandLoop() {
	for {
		select {
		case cmd := <-a.cmdCh:
			log.Println("Command received:", cmd)
			a.handleCommand(cmd)
		case <-a.done:
			return
		}
	}
}

func (a *App) syncCursor() {
	switch a.data.focus {
	case focusEditor:
		if a.data.row < a.data.scroll || a.data.row > (a.data.scroll+len(a.editor)-1) {
			// out of viewport
			screen.HideCursor()
			return
		}
		screen.ShowCursor(a.editor[0].x+a.data.col, a.editor[0].y+a.data.row-a.data.scroll)
	case focusConsole:
		screen.ShowCursor(a.console.x+a.data.consoleCursor, a.console.y)
	default:
		screen.HideCursor()
	}
}

func (a *App) editorEvent(ev *tcell.EventKey) {
	defer func() {
		a.syncCursor()
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.data.row+1, a.data.col+1))
		if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
			a.data.upDownCol = -1
		}
	}()
	switch ev.Key() {
	case tcell.KeyRune:
		var line *lineBuf
		if e := a.data.line(a.data.row); e == nil {
			line = LineBuf("")
			a.data.lines.PushBack(line)
		} else {
			line = e.Value.(*lineBuf)
		}
		line.insert(ev.Rune())
		a.data.col = line.seek(a.data.col + 1)
		a.editor[a.data.row-a.data.scroll].draw(line.String())
	case tcell.KeyEnter:
		if e := a.data.line(a.data.row); e == nil {
			// file end
			line := LineBuf("")
			a.data.lines.PushBack(line)
		} else {
			line := e.Value.(*lineBuf)
			buf := make([]rune, len(line.buf)-a.data.col)
			copy(buf, line.buf[a.data.col:])
			a.data.lines.InsertAfter(&lineBuf{buf: buf}, e)
			line.buf = line.buf[:a.data.col]
		}
		a.data.col = 0
		a.data.row++
		if a.data.row >= a.data.scroll+len(a.editor) {
			a.data.scroll++
		}
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// backspace at line start, delete current line and move up
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
			a.data.col = prevLine.seek(len(prevLine.buf))
			a.data.lines.Remove(currentLine)
			prevLine.buf = append(prevLine.buf,
				currentLine.Value.(*lineBuf).buf...)
			a.data.row--
			a.drawEditor()
			return
		}

		line := a.data.line(a.data.row).Value.(*lineBuf)
		if line == nil {
			return
		}
		line.backspace()
		a.data.col = line.seek(a.data.col - 1)
		a.editor[a.data.row-a.data.scroll].draw(line.String())
	case tcell.KeyLeft:
		// file start
		if a.data.row == 0 && a.data.col == 0 {
			return
		}
		if a.data.col == 0 {
			// move to previous line
			a.data.row--
			line := a.data.line(a.data.row).Value.(*lineBuf)
			a.data.col = line.seek(len(line.buf))
			if a.data.row < a.data.scroll {
				a.data.scroll--
				a.drawEditor()
			}
			return
		}
		line := a.data.line(a.data.row).Value.(*lineBuf)
		a.data.col = line.seek(a.data.col - 1)
	case tcell.KeyRight:
		lineItem := a.data.line(a.data.row)
		if lineItem == nil {
			return
		}

		line := lineItem.Value.(*lineBuf)
		// middle of the line
		if a.data.col < len(line.buf) {
			a.data.col = line.seek(a.data.col + 1)
			return
		}
		// file end
		if lineItem.Next() == nil {
			return
		}
		// line end, move to next line
		a.data.row++
		a.data.col = 0
		if a.data.row >= a.data.scroll+len(a.editor) {
			a.data.scroll++
			a.drawEditor()
		}
	case tcell.KeyUp:
		if a.data.row == 0 {
			return // already at the top
		}

		a.data.row--
		line := a.data.line(a.data.row).Value.(*lineBuf)
		if line == nil {
			a.data.col = 0
		} else if a.data.upDownCol > 0 {
			// moving up/down, keep previous column
			a.data.col = line.seek(a.data.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.data.upDownCol = a.data.col
			a.data.col = line.seek(a.data.col)
		}
		if a.data.row < a.data.scroll {
			a.data.scroll--
			a.drawEditor()
			return
		}
	case tcell.KeyDown:
		if a.data.row == a.data.lines.Len()-1 {
			return // already at the bottom
		}

		a.data.row++
		line := a.data.line(a.data.row).Value.(*lineBuf)
		if line == nil {
			a.data.col = 0
		} else if a.data.upDownCol > 0 {
			// moving up/down, keep previous column
			a.data.col = line.seek(a.data.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.data.upDownCol = a.data.col
			a.data.col = line.seek(a.data.col)
		}
		if a.data.row >= a.data.scroll+len(a.editor) {
			a.data.scroll++
			a.drawEditor()
			return
		}
	case tcell.KeyCtrlA:
		a.data.col = 0
	case tcell.KeyCtrlE:
		e := a.data.line(a.data.row)
		if e == nil {
			return
		}
		line := e.Value.(*lineBuf)
		a.data.col = line.seek(len(line.buf))
	}
}
