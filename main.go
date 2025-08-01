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
	s       *State
	tab     View
	editor  []*View // viewport
	status  View
	console View
	cmdCh   chan string
	done    chan struct{}
}

type State struct {
	tabs          []string
	tabIdx        int
	lines         *list.List
	row           int    // Current row position (starts from 0)
	col           int    // Current column position (starts from 0)
	scroll        int    // Scroll position for the editor (starts from 0)
	upDownCol     int    // Column to maintain while navigating up/down
	status        string // Status message displayed in the status bar
	console       string // Console input text
	consoleCursor int    // Cursor position in the console
	focus         int    // Current focus (editor or console)
}

// line return the lineBuf in list
func (st *State) line(i int) *list.Element {
	if st.lines.Len() == 0 || i > st.lines.Len()-1 {
		return nil
	}

	l := st.lines.Front()
	for range i {
		l = l.Next()
	}
	return l
}

func (st *State) openTab(s string) error {
	bs, err := os.ReadFile(s)
	if err != nil {
		return err
	}

	i := slices.Index(st.tabs, s)
	if i < 0 {
		st.tabs = append(st.tabs, s)
		st.tabIdx = len(st.tabs) - 1
	} else {
		st.tabIdx = i
	}

	st.lines.Init()
	for _, line := range bytes.Split(bs, []byte{'\n'}) {
		st.lines.PushBack(string(line))
	}
	st.scroll = 0
	st.row = 0
	st.col = 0
	return nil
}

func (st *State) deleteTab(s string) {
	for i, tab := range st.tabs {
		if tab == s {
			st.tabs = slices.Delete(st.tabs, i, i+1)
			if st.tabIdx >= len(st.tabs) {
				st.tabIdx = len(st.tabs) - 1
			}
			return
		}
	}
}

// adjustIndex ensures the index not over the line end
func adjustIndex(line string, i int) int {
	if i < 0 {
		return 0
	} else if i >= len(line) {
		return len(line)
	}
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
	a.status.draw(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.console.draw(a.s.console)
	a.syncCursor()
}

func (a *App) drawTabs() {
	if len(a.s.tabs) == 0 {
		a.tab.draw("New Tab")
		return
	}

	var col int
	for i, tab := range a.s.tabs {
		style := a.tab.style.Background(tcell.ColorDarkGray)
		if i == a.s.tabIdx {
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
	line := a.s.lines.Front()
	for range a.s.scroll {
		line = line.Next()
	}
	remainlines := a.s.lines.Len() - a.s.scroll
	for i, lineView := range a.editor {
		if i < remainlines {
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
		s: &State{
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
				if len(app.s.tabs) == 0 {
					close(app.done)
					s.Fini()
					return
				}
				app.cmdCh <- ">close " + app.s.tabs[app.s.tabIdx]
				continue
			}
			// open file
			if ev.Key() == tcell.KeyCtrlP {
				app.s.focus = focusConsole
				app.setStatus("open file by name (append : to go to line or > to execute command)")
				app.setConsole("")
				app.syncCursor()
				continue
			}

			if app.s.focus == focusConsole {
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
				app.s.scroll -= int(float32(y) * scrollFactor)
				if app.s.scroll < 0 {
					app.s.scroll = 0
				}
				app.drawEditor()
				app.syncCursor()
			case tcell.WheelDown:
				app.s.scroll += int(float32(y) * scrollFactor)
				// keep it viewport
				if app.s.scroll > app.s.lines.Len()-len(app.editor) {
					app.s.scroll = app.s.lines.Len() - len(app.editor)
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
		for i, tab := range a.s.tabs {
			if x < a.tab.x+width+len(tab) {
				if i != a.s.tabIdx {
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
		a.s.focus = focusConsole
		a.setConsole("")
		a.syncCursor()
		return
	}

	a.s.focus = focusEditor
	if a.s.lines.Len() == 0 {
		a.s.row = 0
		a.s.col = 0
	} else {
		a.s.row = min(y-a.editor[0].y+a.s.scroll, a.s.lines.Len()-1)
		line := a.s.line(a.s.row).Value.(string)
		col := x - a.editor[0].x
		a.s.col = adjustIndex(line, col)
	}
	a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.syncCursor()
	a.s.upDownCol = -1
	// debug
	if line := a.s.line(a.s.row); line != nil {
		log.Printf("Clicked line %d, column %d, text: %q", a.s.row+1, a.s.col+1,
			line.Value.(string))
	}
}

// setStatus updates the status view with the given string.
func (a *App) setStatus(s string) {
	a.s.status = s
	a.status.draw(s)
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string) {
	a.s.console = s
	a.s.consoleCursor = len(s)
	a.console.draw(s)
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		a.s.console = ""
		a.s.consoleCursor = 0
		a.s.focus = focusEditor
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	case tcell.KeyEnter:
		if a.s.console != "" {
			a.cmdCh <- strings.TrimSpace(a.s.console)
			a.s.console = ""
			a.s.consoleCursor = 0
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.s.console == "" {
			return
		}
		a.s.console = a.s.console[:len(a.s.console)-1]
		a.s.consoleCursor--
	case tcell.KeyLeft:
		if a.s.console == "" {
			return
		}
		a.s.consoleCursor--
	case tcell.KeyRight:
		if a.s.console == "" || a.s.consoleCursor >= len(a.s.console) {
			return
		}
		a.s.consoleCursor++
	case tcell.KeyRune:
		if a.s.consoleCursor >= len(a.s.console) {
			a.s.console += string(ev.Rune())
		} else {
			a.s.console = a.s.console[:a.s.consoleCursor] + string(ev.Rune()) + a.s.console[a.s.consoleCursor:]
		}
		a.s.consoleCursor++
	default:
		return
	}
	a.console.draw(a.s.console)
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
			currentTab := a.s.tabs[a.s.tabIdx]
			a.s.deleteTab(filename)
			// no tab left
			if len(a.s.tabs) == 0 {
				a.s.lines.Init()
				a.s.row = 0
				a.s.col = 0
				a.draw()
				return
			}
			// closed other tab
			if filename != currentTab {
				a.drawTabs()
				return
			}
			// switch to next tab
			err := a.s.openTab(a.s.tabs[a.s.tabIdx])
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
		if line < 1 || line > a.s.lines.Len() {
			a.setStatus("Line number out of range")
			return
		}
		a.s.row = line - 1
		a.s.col = 0
		a.s.scroll = line - 1
		a.s.focus = focusEditor
		a.draw()
	case '@':
	default:
		filename := cmd
		err := a.s.openTab(filename)
		if err != nil {
			a.setStatus(err.Error())
			screen.Show()
			return
		}

		a.s.focus = focusEditor
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
	switch a.s.focus {
	case focusEditor:
		if a.s.row < a.s.scroll || a.s.row > (a.s.scroll+len(a.editor)-1) {
			// out of viewport
			screen.HideCursor()
			return
		}
		screen.ShowCursor(a.editor[0].x+a.s.col, a.editor[0].y+a.s.row-a.s.scroll)
	case focusConsole:
		screen.ShowCursor(a.console.x+a.s.consoleCursor, a.console.y)
	default:
		screen.HideCursor()
	}
}

func (a *App) editorEvent(ev *tcell.EventKey) {
	defer func() {
		a.syncCursor()
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
			a.s.upDownCol = -1
		}
	}()
	switch ev.Key() {
	case tcell.KeyRune:
		var line string
		if e := a.s.line(a.s.row); e == nil {
			line = string(ev.Rune())
			a.s.lines.PushBack(line)
		} else {
			line = e.Value.(string)
			line = line[:a.s.col] + string(ev.Rune()) + line[a.s.col:]
			e.Value = line
		}
		a.s.col++
		a.editor[a.s.row-a.s.scroll].draw(line)
	case tcell.KeyEnter:
		if e := a.s.line(a.s.row); e == nil {
			// file end
			a.s.lines.PushBack("")
		} else {
			// insert a new line after the current line
			line := e.Value.(string)
			a.s.lines.InsertAfter(line[a.s.col:], e)
			e.Value = line[:a.s.col]
		}
		a.s.col = 0
		a.s.row++
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
		}
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// backspace at line start, delete current line and move up
		if a.s.col == 0 {
			if a.s.row == 0 {
				return // no line to delete
			}

			line := a.s.line(a.s.row)
			prevLine := line.Prev()
			a.s.col = len(prevLine.Value.(string))
			prevLine.Value = prevLine.Value.(string) + line.Value.(string)
			a.s.lines.Remove(line)
			a.s.row--
			a.drawEditor()
			return
		}

		// line := a.s.line(a.s.row).Value.(*lineBuf)
		// if line == nil {
		// 	return
		// }
		// line.buf = slices.Delete(line.buf, a.s.col-1, a.s.col)
		line := a.s.line(a.s.row)
		text := line.Value.(string)
		text = text[:a.s.col-1] + text[a.s.col:]
		line.Value = text
		a.s.col--
		a.editor[a.s.row-a.s.scroll].draw(text)
	case tcell.KeyLeft:
		// file start
		if a.s.row == 0 && a.s.col == 0 {
			return
		}
		if a.s.col == 0 {
			// move to previous line
			a.s.row--
			line := a.s.line(a.s.row).Value.(string)
			a.s.col = len(line)
			if a.s.row < a.s.scroll {
				a.s.scroll--
				a.drawEditor()
			}
			return
		}
		a.s.col--
	case tcell.KeyRight:
		lineItem := a.s.line(a.s.row)
		if lineItem == nil {
			return
		}

		line := lineItem.Value.(string)
		// middle of the line
		if a.s.col < len(line) {
			a.s.col++
			return
		}
		// file end
		if lineItem.Next() == nil {
			return
		}
		// line end, move to next line
		a.s.row++
		a.s.col = 0
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
			a.drawEditor()
		}
	case tcell.KeyUp:
		if a.s.row == 0 {
			return // already at the top
		}

		a.s.row--
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = adjustIndex(line, a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = adjustIndex(line, a.s.col)
		}
		if a.s.row < a.s.scroll {
			a.s.scroll--
			a.drawEditor()
			return
		}
	case tcell.KeyDown:
		if a.s.row == a.s.lines.Len()-1 {
			return // already at the bottom
		}

		a.s.row++
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = adjustIndex(line, a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = adjustIndex(line, a.s.col)
		}
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
			a.drawEditor()
			return
		}
	case tcell.KeyCtrlA:
		a.s.col = 0
	case tcell.KeyCtrlE:
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		a.s.col = len(e.Value.(string))
	}
}
