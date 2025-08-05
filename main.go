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
	row           int               // Current row position (starts from 0)
	col           int               // Current column position (starts from 0)
	scroll        int               // Scroll position for the editor (starts from 0)
	upDownCol     int               // Column to maintain while navigating up/down
	status        string            // Status message displayed in the status bar
	console       string            // Console input text
	consoleCursor int               // Cursor position in the console
	focus         int               // Current focus (editor or console)
	symbols       map[string]Symbol // symbol name to list of symbols
	matchSymbols  []Symbol
	matchIdx      int
	completion    string

	selecting         bool
	selectionStartRow int
	selectionStartCol int
	selectionEndRow   int
	selectionEndCol   int
	// lastClickPos
	// lastClickTime     time.Time // Time of the last mouse click
	// clickCount        int       // Number of consecutive clicks
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

func (st *State) switchTab(i int) {
	if i < 0 || i > len(st.tabs)-1 {
		return
	}

	st.tabIdx = i
	st.lines.Init()
	st.row = 0
	st.col = 0
	st.scroll = 0
	st.symbols = nil

	file := st.tabs[st.tabIdx]
	if file == "" {
		return
	}
	bs, err := os.ReadFile(file)
	if err != nil {
		log.Printf("Failed to open file %s: %v", file, err)
		return
	}
	for _, line := range bytes.Split(bs, []byte{'\n'}) {
		st.lines.PushBack(string(line))
	}
	symbols, err := ParseSymbol(file)
	if err != nil {
		log.Print(err)
	}
	st.symbols = symbols
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

// draw a line with a optional style
func (v *View) draw(line string, style ...tcell.Style) {
	s := v.style
	if len(style) > 0 {
		s = style[0]
	}
	for row := range v.h {
		for col := range v.w {
			if col < len(line) {
				screen.SetContent(v.x+col, v.y+row, rune(line[col]), nil, s)
			} else {
				screen.SetContent(v.x+col, v.y+row, ' ', nil, s)
			}
		}
	}
}

type textStyle struct {
	text  string
	style tcell.Style
}

// draw inline texts with different styles
func (v *View) drawText(texts ...textStyle) {
	var col int
	for _, ts := range texts {
		style := ts.style
		if style == tcell.StyleDefault {
			style = v.style
		}
		for _, c := range ts.text {
			screen.SetContent(v.x+col, v.y, c, nil, style)
			col++
		}
	}
	// clear remaining space
	for i := col; i < v.w; i++ {
		screen.SetContent(v.x+i, v.y, ' ', nil, v.style)
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
	var ts []textStyle
	var col int
	for i, tab := range a.s.tabs {
		if tab == "" {
			tab = "untitled"
		}
		if i == a.s.tabIdx {
			ts = append(ts, textStyle{text: tab, style: a.tab.style.Bold(true).Underline(true).Italic(true)})
			ts = append(ts, textStyle{text: tabCloser, style: a.tab.style.Bold(true)})
		} else {
			ts = append(ts, textStyle{text: tab})
			ts = append(ts, textStyle{text: tabCloser})
		}
		col += len(tab) + len(tabCloser)
	}
	save := " Save "
	quit := " Quit "
	padding := max(0, a.tab.w-col-len(save)-len(quit))
	ts = append(ts, textStyle{text: strings.Repeat(" ", padding)})
	ts = append(ts, textStyle{text: save})
	ts = append(ts, textStyle{text: quit})
	a.tab.drawText(ts...)
}

func (a *App) drawEditor() {
	line := a.s.lines.Front()
	for range a.s.scroll {
		line = line.Next()
	}
	remainlines := a.s.lines.Len() - a.s.scroll
	for i, lineView := range a.editor {
		if i < remainlines {
			text := line.Value.(string)
			if a.s.selecting && a.s.selectionStartRow <= a.s.scroll+i && a.s.selectionEndRow >= a.s.scroll+i {
				startCol := 0
				endCol := len(text)
				if a.s.selectionStartRow == a.s.scroll+i {
					startCol = a.s.selectionStartCol
				}
				if a.s.selectionEndRow == a.s.scroll+i {
					endCol = a.s.selectionEndCol
				}
				lineView.drawText(
					textStyle{text: text[:startCol]},
					textStyle{text: text[startCol:endCol], style: highlightStyle},
					textStyle{text: text[endCol:]},
				)
			} else {
				lineView.draw(text)
			}
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
	s.SetCursorStyle(tcell.CursorStyleBlinkingBlock)
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
		s:     &State{lines: list.New(), tabs: []string{""}},
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
	go app.commandLoop()

	if len(os.Args) >= 2 {
		bs, err := os.ReadFile(os.Args[1])
		if err != nil {
			log.Print(err)
		} else {
			app.s.tabs = []string{os.Args[1]}
			for _, line := range bytes.Split(bs, []byte{'\n'}) {
				app.s.lines.PushBack(string(line))
			}
		}
	}

	eventCh := make(chan tcell.Event, 10)
	go s.ChannelEvents(eventCh, app.done)

	for {
		// Update screen
		s.Show()
		select {
		case <-app.done:
			return
		case ev := <-eventCh:
			switch ev := ev.(type) {
			case *tcell.EventResize: // arrive when the app start
				w, h := s.Size()
				app.resize(w, h)
				app.draw()
				s.Sync()
			case *tcell.EventKey:
				log.Printf("Key pressed: %s %c", tcell.KeyNames[ev.Key()], ev.Rune())
				if ev.Key() == tcell.KeyCtrlQ {
					close(app.done)
					return
				}
				// redraw the screen, sometimes iTerm2 resize but doesn't trigger a resize event
				if ev.Key() == tcell.KeyCtrlL {
					s.Sync()
					continue
				}
				if ev.Key() == tcell.KeyCtrlW {
					app.cmdCh <- ">close " + strconv.Itoa(app.s.tabIdx)
					if len(app.s.tabs) == 0 {
						close(app.done)
						return
					}
					continue
				}
				if ev.Key() == tcell.KeyCtrlO {
					app.s.focus = focusConsole
					app.setStatus("open file (:<line> or @symbol)")
					app.setConsole("")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlG {
					app.s.focus = focusConsole
					app.setStatus("go to line number")
					app.setConsole(":")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlR {
					app.s.focus = focusConsole
					app.setStatus("go to symbol")
					app.setConsole("@")
					app.syncCursor()

					if len(app.s.tabs) == 0 {
						app.setStatus("Not a Go file, cannot parse symbols")
						continue
					}
					if !strings.HasSuffix(app.s.tabs[app.s.tabIdx], ".go") {
						app.setStatus("Not a Go file, cannot parse symbols")
						continue
					}
					symbols, err := ParseSymbol(app.s.tabs[app.s.tabIdx])
					if err != nil {
						app.setStatus("Failed to parse symbols: " + err.Error())
						continue
					}
					app.s.symbols = symbols
					app.s.matchSymbols = nil
					app.s.matchIdx = 0
					continue
				}
				if ev.Key() == tcell.KeyCtrlF {
					app.s.focus = focusConsole
					app.setStatus("search text")
					app.setConsole("#")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlP {
					app.s.focus = focusConsole
					app.setStatus("command:")
					app.setConsole(">")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlS {
					app.cmdCh <- ">save " + app.s.tabs[app.s.tabIdx]
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
				case tcell.Button1:
					// will receive this event many times
					// when pressing left button and moving the mouse
					log.Print("Mouse press at: ", x, y)
					app.handleClick(x, y)
				case tcell.ButtonNone:
					// will receive this event when mouse is released
					// or when mouse is moved without pressing any button
					log.Print("Mouse release at: ", x, y)
					if !app.s.selecting {
						continue
					}
					app.s.selecting = false
				case tcell.WheelUp:
					app.s.scroll -= int(float32(y) * scrollFactor)
					if app.s.scroll < 0 {
						app.s.scroll = 0
					}
					app.drawEditor()
					app.syncCursor()
				case tcell.WheelDown:
					// keep in viewport
					if app.s.lines.Len() < len(app.editor) {
						app.s.scroll = 0
						continue
					}
					app.s.scroll += int(float32(y) * scrollFactor)
					if app.s.scroll > app.s.lines.Len()-len(app.editor) {
						app.s.scroll = app.s.lines.Len() - len(app.editor)
						continue
					}
					app.drawEditor()
					app.syncCursor()
				}
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
			if tab == "" {
				tab = "untitled"
			}
			if x < a.tab.x+width+len(tab) {
				if i != a.s.tabIdx {
					a.s.switchTab(i)
					a.s.focus = focusEditor
					a.draw()
				}
				return
			} else if x < a.tab.x+width+len(tab)+len(tabCloser) {
				a.s.closeTab(i)
				if len(a.s.tabs) == 0 {
					close(a.done)
					return
				}
				a.s.focus = focusEditor
				a.draw()
				return
			}
			width += len(tab) + len(tabCloser)
		}

		saveLabelWidth := len(" Save ")
		quitLabelWidth := len(" Quit ")
		if x >= a.tab.x+a.tab.w-saveLabelWidth-quitLabelWidth && x < a.tab.x+a.tab.w-quitLabelWidth {
			a.cmdCh <- ">save " + a.s.tabs[a.s.tabIdx]
			return
		}
		if x >= a.tab.x+a.tab.w-quitLabelWidth {
			close(a.done)
			return
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
	if !a.s.selecting {
		a.s.selectionStartRow = a.s.row
		a.s.selectionStartCol = a.s.col
		a.s.selectionEndRow = a.s.row
		a.s.selectionEndCol = a.s.col
		a.s.selecting = true
	} else {
		a.s.selectionEndRow = a.s.row
		a.s.selectionEndCol = a.s.col
	}

	a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.syncCursor()
	a.drawEditor()
	a.s.upDownCol = -1 // reset up/down column tracking
	// debug
	// if line := a.s.line(a.s.row); line != nil {
	// 	log.Printf("Clicked line %d, column %d, text: %q", a.s.row+1, a.s.col+1,
	// 		line.Value.(string))
	// }
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

// jump moves the cursor to the specified line and column,
// adjusting the scroll position to keep the line in view.
// Note: it does not render the editor
func (a *App) jump(row, col int) {
	a.s.row = row
	a.s.col = col
	a.s.scroll = max(0, a.s.row-(len(a.editor)/2)) // viewport center
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		a.s.console = ""
		a.s.consoleCursor = 0
		a.s.focus = focusEditor
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		// reset matched text
		if line := a.s.line(a.s.row); line != nil {
			a.editor[a.s.row-a.s.scroll].draw(line.Value.(string))
		}
	case tcell.KeyEnter:
		if a.s.console == "" {
			return
		}
		// go to symbol
		if a.s.console[0] == '@' && len(a.s.matchSymbols) > 0 {
			matched := a.s.matchSymbols[a.s.matchIdx]
			a.jump(matched.Line-1, matched.Column-1)
			a.s.focus = focusEditor
			a.s.console = ""
			a.draw()
			return
		}
		a.cmdCh <- strings.TrimSpace(a.s.console)
		a.s.console = ""
		a.s.consoleCursor = 0
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.s.console == "" {
			return
		}
		a.s.console = a.s.console[:a.s.consoleCursor-1] + a.s.console[a.s.consoleCursor:]
		a.s.consoleCursor--
		if a.s.console != "" && a.s.console[0] == '@' {
			a.cmdCh <- a.s.console
		}
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
		// incremental command handling
		if len(a.s.console) >= 2 && a.s.console[0] == '@' {
			a.cmdCh <- a.s.console
		}
	case tcell.KeyTAB, tcell.KeyBacktab:
		if len(a.s.matchSymbols) == 0 {
			return
		}
		// select symbol in the match list
		if ev.Key() == tcell.KeyTAB {
			a.s.matchIdx = (a.s.matchIdx + 1) % len(a.s.matchSymbols)
		} else {
			a.s.matchIdx = (a.s.matchIdx - 1 + len(a.s.matchSymbols)) % len(a.s.matchSymbols)
		}
		ts := make([]textStyle, len(a.s.matchSymbols))
		for i, sym := range a.s.matchSymbols {
			if i == a.s.matchIdx {
				ts[i] = textStyle{
					text:  sym.Name + " ",
					style: highlightStyle,
				}
			} else {
				ts[i] = textStyle{text: sym.Name + " "}
			}
		}
		a.status.drawText(ts...)
	default:
		return
	}
	a.console.draw(a.s.console)
}

func (st *State) closeTab(index int) {
	if index < 0 || index >= len(st.tabs) {
		return
	}
	st.tabs = slices.Delete(st.tabs, index, index+1)
	if len(st.tabs) == 0 {
		return
	}
	if index < st.tabIdx {
		st.tabIdx--
	} else if index == st.tabIdx {
		if st.tabIdx >= len(st.tabs) {
			st.tabIdx = len(st.tabs) - 1
		}
		st.switchTab(st.tabIdx)
	}
}

// handleCommand processes a command string and performs actions based on its prefix.
// Commands:
// - <filename> open file
// - :<line_number> go to line
// - @<symbol> go to symbol
// - #<text> search text
func (a *App) handleCommand(cmd string) {
	// this function is called outside the main goroutine,
	// so ensure to call screen.Show() after making changes to reflect updates.
	defer screen.Show()
	cmd = strings.TrimSpace(cmd)
	switch cmd[0] {
	case '>':
		c := strings.Split(cmd[1:], " ")
		switch c[0] {
		case "save":
			if len(c) == 1 || len(c[1]) == 0 {
				a.setStatus("Usage: >save <filename>")
				a.setConsole(">save ")
				a.s.focus = focusConsole
				a.syncCursor()
				return
			}
			filename := c[1]
			var content []string
			for e := a.s.lines.Front(); e != nil; e = e.Next() {
				content = append(content, e.Value.(string))
			}
			err := os.WriteFile(filename, []byte(strings.Join(content, "\n")), 0644)
			if err != nil {
				log.Printf("Failed to save file %s: %v", filename, err)
				a.setStatus("Failed to save file: " + err.Error())
			} else {
				a.setStatus("File saved as: " + filename)
				a.s.tabs[a.s.tabIdx] = filename // update current tab
				a.s.focus = focusEditor
			}
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
		a.jump(line-1, 0)
		a.s.focus = focusEditor
		a.draw()
	case '@':
		symbolStr := cmd[1:]
		if symbolStr == "" {
			a.s.matchSymbols = nil
			a.setStatus("Usage: @symbol")
			return
		}
		var matched []Symbol
		for k, v := range a.s.symbols {
			if strings.Contains(strings.ToLower(k), strings.ToLower(symbolStr)) {
				matched = append(matched, v)
			}
		}
		slices.SortFunc(matched, func(a, b Symbol) int {
			return strings.Compare(a.Name, b.Name)
		})
		a.s.matchSymbols = matched
		a.s.matchIdx = 0
		if len(matched) == 0 {
			a.setStatus("no matching symbols")
			return
		}
		ts := make([]textStyle, len(matched))
		for i, sym := range matched {
			if i == a.s.matchIdx {
				ts[i] = textStyle{
					text:  sym.Name + " ",
					style: highlightStyle,
				}
			} else {
				ts[i] = textStyle{text: sym.Name + " "}
			}
		}
		a.status.drawText(ts...)
	case '#':
		text := cmd[1:]
		if text == "" {
			a.setStatus("Usage: #text")
			return
		}
		row := a.s.row
		col := a.s.col
		var reverse bool
		start := a.s.line(row)
		for e := start; ; e = e.Next() {
			if e == nil {
				// reverse search
				e = a.s.lines.Front()
				row = 0
				col = 0
				reverse = true
			}
			if e == start && reverse {
				// reached the start again, no match found
				a.setConsole(cmd)
				a.syncCursor()
				return
			}
			line := e.Value.(string)
			if i := strings.Index(strings.ToLower(line[col:]), strings.ToLower(text)); i > -1 {
				a.s.row = row
				a.s.col = col + i + len(text)
				a.s.scroll = max(0, a.s.row-(len(a.editor)/2)) // viewport center
				// incremental search
				a.setConsole(cmd)
				a.draw()
				lineView := a.editor[a.s.row-a.s.scroll]
				// highlight the found text
				lineView.drawText(
					textStyle{text: line[:col+i]},
					textStyle{text: line[col+i : a.s.col], style: highlightStyle},
					textStyle{text: line[a.s.col:]},
				)
				return
			}
			row++
			col = 0
		}
	default:
		// open file
		filename := cmd
		i := slices.Index(a.s.tabs, filename)
		if i < 0 {
			a.s.tabs = append(a.s.tabs, filename)
			i = len(a.s.tabs) - 1
		}
		a.s.switchTab(i)
		a.s.focus = focusEditor
		a.draw()
	}
}

var highlightStyle = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(paperColor)

func (a *App) commandLoop() {
	for {
		select {
		case cmd := <-a.cmdCh:
			log.Printf("Command received: %q", cmd)
			a.handleCommand(cmd)
		case <-a.done:
			return
		}
	}
}

// paper-like yellow color
var paperColor = tcell.NewRGBColor(255, 249, 202)

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

		// completion
		if a.s.col == len(line) && a.s.symbols != nil {
			var keyword string
			for i := len(line) - 1; i >= 0; i-- {
				if line[i] == ' ' || line[i] == '\t' || line[i] == '.' {
					keyword = line[i+1:]
					break
				}
				if i == 0 {
					keyword = line
					break
				}
			}
			if keyword == "" {
				a.editor[a.s.row-a.s.scroll].draw(line)
				return
			}
			var completion string
			for k := range a.s.symbols {
				// TODO: smart case
				if strings.HasPrefix(k, keyword) {
					completion = k[len(keyword):]
					// this is inline completion, so one symbol is enough
					break
				}
			}
			if completion != "" {
				a.s.completion = completion
				a.editor[a.s.row-a.s.scroll].drawText(
					textStyle{text: line},
					textStyle{text: completion, style: tcell.StyleDefault.Foreground(tcell.ColorGray)},
				)
				return
			}
		}

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
	case tcell.KeyTAB:
		s := "\t"
		if a.s.completion != "" {
			s = a.s.completion
		}
		line := a.s.line(a.s.row)
		if line == nil {
			line = a.s.lines.PushBack(s)
		} else {
			line.Value = line.Value.(string) + s
		}
		a.s.col += len(s)
		a.editor[a.s.row-a.s.scroll].draw(line.Value.(string))
	}
}
