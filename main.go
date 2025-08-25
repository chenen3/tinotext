package main

import (
	"bufio"
	"container/list"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

const (
	focusEditor = iota
	focusConsole
)

type App struct {
	s       *State
	tab     View
	editor  []*View
	status  View
	console View
	cmdCh   chan string
	done    chan struct{}
}

type State struct {
	*Tab          // active tab
	tabs          []*Tab
	tabIdx        int    // index of active tab
	status        string // Status message displayed in the status bar
	console       []rune // Console input text
	consoleCursor int    // Cursor position in the console
	focus         int    // Current focus (editor or console)
	lineNumber    bool   // Whether to show line numbers in the editor
	clipboard     string
	files         []string // top level file names
	options       []string // options listed in the status bar
	optionIdx     int      // current option index
}

type Tab struct {
	filename  string
	lines     *list.List          // element is rune slice
	row       int                 // Current row position (starts from 0)
	col       int                 // Current column position (starts from 0)
	top       int                 // vertical scroll  (starts from 0)
	left      int                 // horizontal scroll  (starts from 0)
	upDownCol int                 // Column to maintain while navigating up/down
	symbols   map[string][]Symbol // symbol name to list of symbols
	// completion   string
	selecting    bool
	selection    *Selection
	undoStack    []Edit
	redoStack    []Edit
	lastEdit     *Edit
	backStack    []int
	forwardStack []int
}

type Selection struct {
	startRow int
	startCol int
	endRow   int
	endCol   int
}

// line returns the list element at the specified line index, or nil if out of bounds.
func (t *Tab) line(i int) *list.Element {
	if t.lines.Len() == 0 || i > t.lines.Len()-1 {
		return nil
	}

	if i == 0 {
		return t.lines.Front()
	}
	if i == t.lines.Len()-1 {
		return t.lines.Back()
	}

	e := t.lines.Front()
	for range i {
		e = e.Next()
	}
	return e
}

// switchTab clears the editor and switch to the specified tab.
func (st *State) switchTab(i int) {
	if i < 0 || i > len(st.tabs)-1 {
		return
	}

	st.tabIdx = i
	st.Tab = st.tabs[i]
}

type View struct {
	x, y, w, h int
	style      tcell.Style
}

// draw draws a line and clears the remaining space
func (v *View) draw(line []rune) {
	col := 0
	for _, c := range line {
		if col >= v.w {
			return
		}
		screen.SetContent(v.x+col, v.y, c, nil, v.style)
		col += runewidth.RuneWidth(c)
	}
	// Clear remaining space
	for i := col; i < v.w; i++ {
		screen.SetContent(v.x+i, v.y, ' ', nil, v.style)
	}
}

type textStyle struct {
	text  []rune
	style tcell.Style
}

// drawText draw inline texts with multiple styles.
// Note that it does not handle tab expansion.
func (v *View) drawText(texts ...textStyle) {
	col := 0
	for _, ts := range texts {
		style := ts.style
		if style == tcell.StyleDefault {
			style = v.style
		}
		for _, c := range ts.text {
			if col >= v.w {
				break
			}
			screen.SetContent(v.x+col, v.y, c, nil, style)
			col += runewidth.RuneWidth(c)
		}
	}
	// Clear remaining space
	for i := col; i < v.w; i++ {
		screen.SetContent(v.x+i, v.y, ' ', nil, v.style)
	}
}

func (v *View) contains(x, y int) bool {
	return x >= v.x && x < v.x+v.w && y >= v.y && y < v.y+v.h
}

func (a *App) resize() {
	w, h := screen.Size()
	a.tab = View{0, 0, w, 1, tcell.StyleDefault.Reverse(true)}
	a.editor = make([]*View, h-3)
	for i := range a.editor {
		a.editor[i] = &View{0, i + a.tab.h, w, 1, tcell.StyleDefault}
	}
	a.status = View{0, h - 2, w, 1, tcell.StyleDefault.Reverse(true)}
	a.console = View{0, h - 1, w, 1, tcell.StyleDefault}
}

const tabSize = 4

// expandTabs converts all tabs in a line to spaces for display
func expandTabs(line []rune) []rune {
	newline := make([]rune, 0, len(line))
	col := 0
	for _, char := range line {
		if char == '\t' {
			// Add spaces to reach the next tab stop
			spaces := tabSize - (col % tabSize)
			for range spaces {
				newline = append(newline, ' ')
			}
			col += spaces
		} else {
			newline = append(newline, char)
			col += runewidth.RuneWidth(char)
		}
	}
	return newline
}

// columnToVisual converts a column index in the line to column index in screen line
func columnToVisual(line []rune, col int) int {
	if col > len(line) {
		col = len(line)
	}
	visualCol := 0
	for i := range col {
		if line[i] == '\t' {
			visualCol += tabSize - (visualCol % tabSize)
		} else {
			visualCol++
		}
	}
	return visualCol
}

// columnToScreenWidth converts a column index in the line to its screen width,
// accounting for tabs and Unicode character widths (e.g., double-width for East Asian characters).
func columnToScreenWidth(line []rune, col int) int {
	if col > len(line) {
		col = len(line)
	}
	screenCol := 0
	for i, char := range line {
		if i >= col {
			break
		}
		if char == '\t' {
			spaces := tabSize - (screenCol % tabSize)
			screenCol += spaces
		} else {
			screenCol += runewidth.RuneWidth(char)
		}
	}
	return screenCol
}

// columnFromScreenWidth converts screen width to column index in the line.
// Use this to get the line column index from screen width
func columnFromScreenWidth(line []rune, screenCol int) int {
	if screenCol <= 0 {
		return 0
	}
	width := 0
	for i, char := range line {
		if char == '\t' {
			spaces := tabSize - (width % tabSize)
			width += spaces
		} else {
			width += runewidth.RuneWidth(char)
		}
		if screenCol < width {
			return i
		}
	}
	return len(line)
}

// draw the whole layout and cursor
func (a *App) draw() {
	a.drawTabs()
	a.drawEditor()
	a.drawStatus()
	a.console.draw(a.s.console)
	a.syncCursor()
}

const (
	labelClose = "x|"
	labelNew   = "New"
	labelOpen  = "Open"
	labelSave  = "Save"
	labelQuit  = "Quit"
)

var menu = []string{labelNew, labelOpen, labelSave, labelQuit}

func (a *App) drawTabs() {
	var ts []textStyle
	var totalTabWidth int
	for i, tab := range a.s.tabs {
		var name string
		if tab.filename == "" {
			name = "untitled"
		} else {
			name = filepath.Base(tab.filename)
		}
		if i == a.s.tabIdx {
			highlight := a.tab.style.Bold(true).Underline(true).Italic(true)
			ts = append(ts, textStyle{text: []rune(name), style: highlight})
		} else {
			ts = append(ts, textStyle{text: []rune(name)})
		}
		ts = append(ts, textStyle{text: []rune{' '}})
		ts = append(ts, textStyle{text: []rune(labelClose)})
		ts = append(ts, textStyle{text: []rune{' '}})
		for _, c := range name {
			totalTabWidth += runewidth.RuneWidth(c)
		}
		totalTabWidth += len(labelClose) + 2
	}

	menuS := strings.Join(menu, " ")
	padding := a.tab.w - totalTabWidth - len(menuS)
	if padding > 0 {
		ts = append(ts, textStyle{text: []rune(strings.Repeat(" ", padding))})
	}
	ts = append(ts, textStyle{text: []rune(menuS)})
	a.tab.drawText(ts...)
}

// drawStatus show filename, line and column number by default.
// If msg is provided, show the message instead.
func (a *App) drawStatus(msg ...string) {
	if len(msg) == 0 {
		lineCol := fmt.Sprintf("Line %d, Column %d ", a.s.row+1, a.s.col+1)
		// padding := max(0, a.status.w-len(a.s.filename)-len(lineCol))
		// a.status.drawText(
		// 	textStyle{text: a.s.filename},
		// 	textStyle{text: strings.Repeat(" ", padding)},
		// 	textStyle{text: lineCol},
		// )
		a.status.draw([]rune(lineCol))
		return
	}

	a.status.draw([]rune(msg[0]))
}

func (st *State) newLineNum(row int) string {
	var n int
	for i := st.lines.Len(); i > 0; i = i / 10 {
		n++
	}
	lineNumer := row + 1
	var m int
	for i := lineNumer; i > 0; i = i / 10 {
		m++
	}
	// at least 1 space before the line number, 1 space after
	return strings.Repeat(" ", n-m+1) + strconv.Itoa(lineNumer) + " "
}

func (st *State) lineNumLen() int {
	if !st.lineNumber {
		return 0
	}

	n := st.lines.Len()
	if n == 0 {
		return 0
	}

	length := 0
	for n > 0 {
		n /= 10
		length++
	}
	return length + 2 // padding
}

// drawEditorLine draws the line with automatic tab expansion and syntax highlight
func (a *App) drawEditorLine(row int, line []rune) {
	var lineNumber []rune
	if a.s.lineNumber {
		lineNumber = []rune(a.s.newLineNum(row))
	}

	screenLine := expandTabs(line)
	if a.s.left > 0 {
		// Adjust for horizontal scroll, accounting for rune widths
		screenCol := 0
		for i, r := range screenLine {
			if screenCol >= a.s.left {
				screenLine = screenLine[i:]
				break
			}
			screenCol += runewidth.RuneWidth(r)
		}
		if screenCol < a.s.left {
			screenLine = nil
			a.editor[row-a.s.top].draw(nil)
		}
	}

	// selection highlight
	if sel := a.s.selected(); sel != nil && sel.startRow <= row && row <= sel.endRow {
		start, end := 0, len(screenLine)
		if sel.startRow == row {
			start = columnToVisual(line, sel.startCol)
		}
		if sel.endRow == row {
			end = columnToVisual(line, sel.endCol)
		}
		a.editor[row-a.s.top].drawText(
			textStyle{text: lineNumber, style: styleComment},
			textStyle{text: screenLine[:start]},
			textStyle{text: screenLine[start:end], style: styleHighlight},
			textStyle{text: screenLine[end:]},
		)
		return
	}

	if a.s.filename == "" || !strings.HasSuffix(a.s.filename, ".go") {
		a.editor[row-a.s.top].drawText(
			textStyle{text: lineNumber, style: styleComment},
			textStyle{text: screenLine, style: styleBase},
		)
		return
	}

	// syntax highlight
	parts := highlightGoLine(screenLine)
	s := make([]textStyle, 0, len(parts)+1)
	s = append(s, textStyle{text: lineNumber, style: styleComment})
	s = append(s, parts...)
	a.editor[row-a.s.top].drawText(s...)
}

func (a *App) drawEditor() {
	e := a.s.lines.Front()
	for range a.s.top {
		e = e.Next()
	}
	remainLines := a.s.lines.Len() - a.s.top

	for i, lineView := range a.editor {
		if i >= remainLines {
			lineView.draw(nil)
			continue
		}
		line := e.Value.([]rune)
		a.drawEditorLine(a.s.top+i, line)
		e = e.Next()
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
	s.SetStyle(styleBase)
	s.SetCursorStyle(tcell.CursorStyleBlinkingBlock, cursorColor)
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
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
	go app.commandLoop()

	app.s = &State{
		tabs: []*Tab{{
			filename: "",
			lines:    list.New(),
		}},
	}
	app.s.Tab = app.s.tabs[0]
	if len(os.Args) >= 2 {
		filename := os.Args[1]
		app.s.tabs[0].filename = filename
		f, err := os.Open(filename)
		if err != nil {
			log.Print(err)

		} else {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				app.s.lines.PushBack([]rune(scanner.Text()))
			}
			if err := scanner.Err(); err != nil {
				log.Print(err)
			}
			f.Close()
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
				app.resize()
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
					app.s.closeTab(app.s.tabIdx)
					if len(app.s.tabs) == 0 {
						close(app.done)
						return
					}
					app.draw()
					continue
				}
				// quickly open file in current folder
				if ev.Key() == tcell.KeyCtrlO {
					// keep it simple, don't read the folder recursively
					entries, err := os.ReadDir(".")
					if err != nil {
						log.Print(err)
						continue
					}
					app.s.files = nil
					for _, entry := range entries {
						if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
							app.s.files = append(app.s.files, entry.Name())
						}
					}
					app.s.options = app.s.files
					app.s.optionIdx = -1 // no selected option by default
					ts := make([]textStyle, 0, len(app.s.options))
					for _, option := range app.s.options {
						ts = append(ts, textStyle{text: []rune(option + " ")})
					}
					app.status.drawText(ts...)

					app.s.focus = focusConsole
					app.setConsole("", "file name")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlG {
					app.s.focus = focusConsole
					app.setConsole(":", "line number")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlR {
					app.s.focus = focusConsole
					app.setConsole("@", "symbol")
					app.syncCursor()

					if len(app.s.tabs) == 0 {
						continue
					}
					if !strings.HasSuffix(app.s.filename, ".go") {
						// Go symbols only
						continue
					}
					var src strings.Builder
					for e := app.s.lines.Front(); e != nil; e = e.Next() {
						src.WriteString(string(e.Value.([]rune)))
						if e != app.s.lines.Back() {
							src.WriteString("\n")
						}
					}
					symbols, err := ParseSymbol(app.s.filename, src.String())
					if err != nil {
						app.drawStatus("Failed to parse symbols: " + err.Error())
						continue
					}
					app.s.symbols = symbols
					app.s.options = nil
					app.s.optionIdx = -1
					continue
				}
				if ev.Key() == tcell.KeyCtrlF {
					var selected string
					if sel := app.s.selected(); sel != nil && sel.startRow == sel.endRow {
						e := app.s.line(sel.startRow)
						if e != nil {
							line := e.Value.([]rune)
							selected = string(line[sel.startCol:sel.endCol])
						}
					}
					if len(selected) > 0 {
						app.setConsole("#" + selected)
					} else {
						app.setConsole("#", "find")
					}
					app.s.focus = focusConsole
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlP {
					app.s.focus = focusConsole
					app.setConsole(">", "command")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlS {
					app.cmdCh <- ">save " + app.s.filename
					continue
				}
				if ev.Key() == tcell.KeyCtrlT {
					// new tab
					app.s.tabs = append(app.s.tabs, &Tab{
						filename: "",
						lines:    list.New(),
					})
					app.s.switchTab(len(app.s.tabs) - 1)
					app.s.focus = focusEditor
					app.draw()
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
					// log.Print("Mouse left button clicked")
					app.handleClick(x, y)
				case tcell.ButtonNone:
					// will receive this event when mouse button is released
					// or when mouse is moved without pressing any button
					// log.Print("Mouse button released")
					if !app.s.selecting {
						continue
					}
					app.s.selecting = false
					if app.s.selection != nil && app.s.selection.startRow == app.s.selection.endRow &&
						app.s.selection.startCol == app.s.selection.endCol {
						// no selection, reset
						app.s.selection = nil
					}
				case tcell.WheelUp:
					app.s.top -= int(float32(y) * scrollFactor)
					if app.s.top < 0 {
						app.s.top = 0
					}
					app.drawEditor()
					app.syncCursor()
				case tcell.WheelDown:
					// keep in viewport
					if app.s.lines.Len() < len(app.editor) {
						app.s.top = 0
						continue
					}
					app.s.top += int(float32(y) * scrollFactor)
					if app.s.top > app.s.lines.Len()-len(app.editor) {
						app.s.top = app.s.lines.Len() - len(app.editor)
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
		var totalTabWidth int
		for _, tab := range a.s.tabs {
			tabName := tab.filename
			if tabName == "" {
				tabName = "untitled"
			}
			totalTabWidth += len(tabName) + len(labelClose) + 2
		}
		sep := " "
		menuS := strings.Join(menu, sep)
		padding := max(0, a.tab.w-totalTabWidth-len(menuS))
		// click menu
		menuStart := a.tab.x + totalTabWidth + padding
		if x >= menuStart {
			start := menuStart
			end := 0
			for _, label := range menu {
				end = start + len(label)
				if x < start || x >= end {
					start = end + len(sep) // separator
					continue
				}
				switch label {
				case labelNew:
					a.s.tabs = slices.Insert(a.s.tabs, a.s.tabIdx+1, &Tab{filename: "", lines: list.New()})
					a.s.switchTab(a.s.tabIdx + 1)
					a.draw()
					return
				case labelOpen:
					a.s.focus = focusConsole
					a.setConsole(">open ")
					a.syncCursor()
					return
				case labelSave:
					if len(a.s.tabs) > 0 && a.s.tabIdx < len(a.s.tabs) {
						a.cmdCh <- ">save " + a.s.filename
					}
					return
				case labelQuit:
					close(a.done)
					return
				}
			}
			return
		}

		// click tabs
		if x < a.tab.x+totalTabWidth {
			nameStart := a.tab.x
			for i, tab := range a.s.tabs {
				tabName := tab.filename
				if tabName == "" {
					tabName = "untitled"
				}
				nameEnd := nameStart
				for _, char := range tabName {
					nameEnd += runewidth.RuneWidth(char)
				}
				// A separator following a tab name is considered part of the name.
				nameEnd += 1
				closerEnd := nameEnd + len(labelClose)
				if x < nameEnd {
					// switch tab
					if i != a.s.tabIdx {
						a.s.switchTab(i)
						a.s.focus = focusEditor
						a.draw()
					}
					return
				} else if x < closerEnd {
					// close tab
					a.s.closeTab(i)
					if len(a.s.tabs) == 0 {
						close(a.done)
						return
					}
					a.s.focus = focusEditor
					a.draw()
					return
				}
				// A separator following a tab closer is considered part of the next tab's name.
				nameStart = closerEnd + 1
			}
		}
		return
	}

	if a.console.contains(x, y) {
		a.s.focus = focusConsole
		a.s.consoleCursor = columnFromScreenWidth([]rune(a.s.console), x-a.console.x)
		a.syncCursor()
		return
	}

	if a.status.contains(x, y) {
		return
	}

	a.s.focus = focusEditor
	if a.s.lines.Len() == 0 {
		a.s.row = 0
		a.s.col = 0
	} else {
		row := min(y-a.editor[0].y+a.s.top, a.s.lines.Len()-1)
		line := a.s.line(row).Value.([]rune)
		screenCol := x - a.editor[0].x - a.s.lineNumLen() + a.s.left
		col := columnFromScreenWidth(line, screenCol)
		a.recordPositon(a.s.row, a.s.col)
		a.s.row = row
		a.s.col = col
	}

	if !a.s.selecting {
		a.s.selection = &Selection{
			startRow: a.s.row,
			startCol: a.s.col,
			endRow:   a.s.row,
			endCol:   a.s.col,
		}
		a.s.selecting = true
	} else {
		a.s.selection.endRow = a.s.row
		a.s.selection.endCol = a.s.col
	}

	a.drawStatus()
	a.drawEditor()
	a.syncCursor()
	a.s.upDownCol = -1 // reset up/down column tracking
	// debug
	if line := a.s.line(a.s.row); line != nil {
		log.Printf("clicked line: %s", string(line.Value.([]rune)))
	}
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string, hint ...string) {
	a.s.console = []rune(s)
	a.s.consoleCursor = len(a.s.console)
	if len(hint) == 0 {
		a.console.draw(a.s.console)
	} else {
		a.console.drawText(
			textStyle{text: a.s.console},
			textStyle{text: []rune(hint[0]), style: styleComment},
		)
	}
}

// jump moves the cursor to the specified line and column,
// If row less than 0, jump to the last line.
// If col less than 0, jump to the end of the line.
// To ensure the line is visible, it may scroll and redraw the editor.
func (a *App) jump(row, col int) {
	if row < 0 || row > a.s.lines.Len()-1 {
		row = a.s.lines.Len() - 1
	}
	lineItem := a.s.line(row)
	if lineItem == nil {
		return
	}
	line := lineItem.Value.([]rune)
	if col < 0 || col > len(line) {
		col = len(line)
	}
	a.s.row = row
	a.s.col = col

	var scroll bool
	h := len(a.editor)
	if row == 0 {
		a.s.top = 0
		scroll = true
	} else if row == a.s.lines.Len()-1 {
		a.s.top = max(0, a.s.lines.Len()-h)
		scroll = true
	} else if row < a.s.top {
		if row == a.s.top-1 {
			a.s.top -= 1 // scrolling up one line is more intuitive
		} else {
			a.s.top = max(0, row-h/2)
		}
		scroll = true
	} else if row > a.s.top+h-1 {
		if row == a.s.top+h {
			a.s.top += 1 // scrolling down one line is more intuitive
		} else {
			a.s.top = row - h/2
		}
		scroll = true
	}

	w := a.editor[0].w
	if i := columnToScreenWidth(line, a.s.col); i < a.s.left {
		a.s.left = i
		scroll = true
	} else if i > a.s.left+(w-a.s.lineNumLen()-1) {
		a.s.left = i - (w - a.s.lineNumLen() - 1)
		scroll = true
	}

	if scroll {
		a.drawEditor()
	}
	a.syncCursor()
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	defer func() {
		a.console.draw(a.s.console)
		a.syncCursor()
	}()
	exitConsole := func() {
		a.s.console = nil
		a.s.focus = focusEditor
		a.drawStatus()
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		exitConsole()
		// reset matched text
		if line := a.s.line(a.s.row); line != nil {
			a.drawEditorLine(a.s.row, line.Value.([]rune))
		}
	case tcell.KeyEnter:
		cmd := strings.TrimSpace(string(a.s.console))
		if cmd == "" {
			if len(a.s.options) > 0 && a.s.optionIdx >= 0 {
				a.cmdCh <- ">open " + a.s.options[a.s.optionIdx]
			}
			exitConsole()
			return
		}
		switch cmd[0] {
		case '#', ':', '>':
			if len(cmd[1:]) == 0 {
				exitConsole()
				return
			}
		case '@':
			if len(a.s.options) == 0 || a.s.optionIdx < 0 {
				exitConsole()
				return
			}
			cmd = "@" + a.s.options[a.s.optionIdx]
		default:
			if len(a.s.options) == 0 || a.s.optionIdx < 0 {
				exitConsole()
				return
			}
			cmd = ">open " + a.s.options[a.s.optionIdx]
		}
		a.s.console = nil
		a.cmdCh <- cmd
	case tcell.KeyLeft:
		if len(a.s.console) == 0 {
			return
		}
		a.s.consoleCursor--
	case tcell.KeyRight:
		if len(a.s.console) == 0 || a.s.consoleCursor >= len(a.s.console) {
			return
		}
		a.s.consoleCursor++
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(a.s.console) == 0 {
			return
		}
		a.s.console = slices.Delete(a.s.console, a.s.consoleCursor-1, a.s.consoleCursor)
		a.s.consoleCursor--
		if len(a.s.console) == 0 {
			a.s.options = a.s.files
			a.s.optionIdx = -1
		} else if char := a.s.console[0]; char == '#' || char == ':' || char == '>' {
			return
		} else if char == '@' {
			keyword := string(a.s.console[1:])
			if len(keyword) == 0 {
				a.s.options = nil
				a.status.draw(nil)
				return
			}
			var filter []string
			for _, v := range a.s.symbols {
				for _, sym := range v {
					name := sym.Name
					if sym.Receiver != "" {
						name = sym.Receiver + "." + sym.Name
					}
					if strings.Contains(strings.ToLower(name), strings.ToLower(keyword)) {
						filter = append(filter, name)
					}
				}
			}
			if len(filter) == 0 {
				a.s.options = nil
				a.status.draw(nil)
				return
			}
			slices.Sort(filter)
			j := 0
			for i := range filter {
				if strings.Index(strings.ToLower(filter[i]), strings.ToLower(keyword)) == 0 {
					// move the relevant forward
					filter[i], filter[j] = filter[j], filter[i]
					j++
				}
			}
			a.s.options = filter
			a.s.optionIdx = 0
		} else {
			// search file
			if len(a.s.files) == 0 {
				return
			}
			keyword := string(a.s.console)
			var filter []string
			for _, name := range a.s.files {
				if strings.Contains(strings.ToLower(name), strings.ToLower(keyword)) {
					filter = append(filter, name)
				}
			}
			if len(filter) == 0 {
				a.s.options = nil
				a.status.draw(nil)
				return
			}
			j := 0
			for i := range filter {
				if strings.Index(filter[i], keyword) == 0 {
					// move the relevant forward
					filter[i], filter[j] = filter[j], filter[i]
					j++
				}
			}
			a.s.options = filter
			a.s.optionIdx = 0
		}
		ts := make([]textStyle, 0, len(a.s.options))
		for i, option := range a.s.options {
			if i == a.s.optionIdx {
				ts = append(ts, textStyle{text: []rune(option + " "), style: styleHighlight})
			} else {
				ts = append(ts, textStyle{text: []rune(option + " ")})
			}
		}
		a.status.drawText(ts...)
	case tcell.KeyRune:
		a.s.console = slices.Insert(a.s.console, a.s.consoleCursor, ev.Rune())
		a.s.consoleCursor++
		switch a.s.console[0] {
		case '>', '#', ':':
			return
		case '@':
			keyword := string(a.s.console[1:])
			if keyword == "" {
				return
			}
			var filter []string
			for _, v := range a.s.symbols {
				for _, sym := range v {
					name := sym.Name
					if sym.Receiver != "" {
						name = sym.Receiver + "." + sym.Name
					}
					if strings.Contains(strings.ToLower(name), strings.ToLower(keyword)) {
						filter = append(filter, name)
					}
				}
			}
			if len(filter) == 0 {
				a.s.options = nil
				a.status.draw(nil)
				return
			}
			slices.Sort(filter)
			j := 0
			for i := range filter {
				if strings.Index(strings.ToLower(filter[i]), strings.ToLower(keyword)) == 0 {
					// move the relevant forward
					filter[i], filter[j] = filter[j], filter[i]
					j++
				}
			}
			a.s.options = filter
			a.s.optionIdx = 0
			ts := make([]textStyle, 0, len(a.s.options))
			for i, option := range a.s.options {
				if i == a.s.optionIdx {
					ts = append(ts, textStyle{text: []rune(option + " "), style: styleHighlight})
				} else {
					ts = append(ts, textStyle{text: []rune(option + " ")})
				}
			}
			a.status.drawText(ts...)
		default: // search file
			if len(a.s.files) == 0 {
				return
			}
			keyword := string(a.s.console)
			var filter []string
			for _, name := range a.s.files {
				if strings.Contains(strings.ToLower(name), strings.ToLower(keyword)) {
					filter = append(filter, name)
				}
			}
			if len(filter) == 0 {
				a.s.options = nil
				a.status.draw(nil)
				return
			}
			j := 0
			for i := range filter {
				if strings.Index(filter[i], keyword) == 0 {
					// move the relevant forward
					filter[i], filter[j] = filter[j], filter[i]
					j++
				}
			}
			a.s.options = filter
			a.s.optionIdx = 0
			ts := make([]textStyle, 0, len(a.s.options))
			for i, option := range a.s.options {
				if i == a.s.optionIdx {
					ts = append(ts, textStyle{text: []rune(option + " "), style: styleHighlight})
				} else {
					ts = append(ts, textStyle{text: []rune(option + " ")})
				}
			}
			a.status.drawText(ts...)
		}
	case tcell.KeyTAB, tcell.KeyBacktab:
		if len(a.s.options) <= 0 {
			return
		}
		if ev.Key() == tcell.KeyTAB {
			a.s.optionIdx = (a.s.optionIdx + 1) % len(a.s.options)
		} else {
			a.s.optionIdx = (a.s.optionIdx - 1 + len(a.s.options)) % len(a.s.options)
		}
		ts := make([]textStyle, 0, len(a.s.options))
		for _, option := range a.s.options {
			ts = append(ts, textStyle{text: []rune(option + " ")})
		}
		ts[a.s.optionIdx].style = styleHighlight
		a.status.drawText(ts...)
	case tcell.KeyCtrlUnderscore:
		// go to previous found keyword
		if len(a.s.console) > 0 && a.s.console[0] == '#' {
			a.goBack()
			keyword := a.s.console[1:]
			a.s.selection = &Selection{
				startRow: a.s.row,
				endRow:   a.s.row,
				startCol: a.s.col - len(keyword),
				endCol:   a.s.col,
			}
			a.drawEditor()
		}
	}
}

// closeTab closes the tab at the specified index and adjusts the current tab selection.
// It handles edge cases for tab index management and ensures a valid tab remains active.
func (st *State) closeTab(index int) {
	if index < 0 || index >= len(st.tabs) {
		return
	}

	st.tabs = slices.Delete(st.tabs, index, index+1)
	if len(st.tabs) == 0 {
		st.tabIdx = 0
		return
	}

	switch {
	case index < st.tabIdx:
		// Closed tab was before current tab, shift index left
		st.tabIdx--
	case index == st.tabIdx:
		// Closed the current tab, need to select a new one
		if st.tabIdx >= len(st.tabs) {
			st.tabIdx = len(st.tabs) - 1
		}
		// Switch to the tab now at the current index (could be same position, new tab)
		st.switchTab(st.tabIdx)
	case index > st.tabIdx:
		// Closed tab was after current tab, no index adjustment needed
	}
}

// handleCommand processes a command string and performs actions based on its prefix.
// Commands:
// - :<line_number> go to line
// - @<symbol> go to symbol
// - #<text> find text
// - ><command>
func (a *App) handleCommand(cmd string) {
	// this function is called outside the main goroutine,
	// so ensure to call screen.Show() after making changes to reflect updates.
	defer screen.Show()
	cmd = strings.TrimSpace(cmd)
	switch cmd[0] {
	case '>':
		c := strings.Split(cmd[1:], " ")
		switch c[0] {
		case "open":
			if len(c) == 1 || len(c[1]) == 0 {
				return
			}
			filename := c[1]
			i := -1
			for j, tab := range a.s.tabs {
				if tab.filename == filename {
					i = j
					break
				}
			}
			if i >= 0 {
				a.s.switchTab(i)
				a.s.focus = focusEditor
				a.draw()
				return
			}

			file, err := os.Open(filename)
			if err != nil {
				log.Print(err)
				a.drawStatus(err.Error())
				return
			}
			defer file.Close()
			lines := list.New()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				lines.PushBack([]rune(scanner.Text()))
			}
			if err := scanner.Err(); err != nil {
				log.Print(err)
				a.drawStatus(err.Error())
				return
			}
			a.s.tabs = append(a.s.tabs, &Tab{filename: filename, lines: lines})
			a.s.switchTab(len(a.s.tabs) - 1)
			a.s.focus = focusEditor
			a.draw()
			return
		case "save":
			if len(c) == 1 || len(c[1]) == 0 {
				a.setConsole(">save ", "filename")
				a.s.focus = focusConsole
				a.syncCursor()
				return
			}
			filename := c[1]
			content := make([]string, 0, a.s.lines.Len())
			for e := a.s.lines.Front(); e != nil; e = e.Next() {
				content = append(content, string(e.Value.([]rune)))
			}
			// append newline at end of file
			if len(content) == 0 || content[len(content)-1] != "" {
				content = append(content, "")
				a.s.lines.PushBack([]rune{})
				a.drawEditor()
			}
			err := os.WriteFile(filename, []byte(strings.Join(content, "\n")), 0644)
			if err != nil {
				log.Printf("Failed to save file %s: %v", filename, err)
				a.drawStatus("Failed to save file: " + err.Error())
			} else {
				a.drawStatus("File saved as: " + filename)
				a.s.filename = filename // update current tab
				a.s.focus = focusEditor
			}
		case "linenumber":
			// toogle line number display
			a.s.lineNumber = !a.s.lineNumber
			// horizonal scroll may changed, update the cursor
			a.s.focus = focusEditor
			a.jump(a.s.row, a.s.col)
			a.drawEditor()
		case "bottom":
			a.s.focus = focusEditor
			a.jump(-1, -1)
		case "back":
			a.s.focus = focusEditor
			a.goBack()
		case "forward":
			a.s.focus = focusEditor
			a.goForward()
		default:
			a.drawStatus("unknown command: " + cmd)
		}
	case ':': // go to line
		line, err := strconv.Atoi(cmd[1:])
		if err != nil {
			a.drawStatus("Invalid line number")
			return
		}
		if line < 1 || line > a.s.lines.Len() {
			a.drawStatus("Line number out of range")
			return
		}
		a.s.focus = focusEditor
		a.jump(line-1, 0)
	case '@': // go to symbol
		name := cmd[1:]
		var receiver string
		if i := strings.Index(name, "."); i >= 0 {
			receiver = name[:i]
			name = name[i+1:]
		}
		var matched Symbol
		for _, symbol := range a.s.symbols[name] {
			if symbol.Receiver == receiver {
				matched = symbol
			}
		}
		a.recordPositon(a.s.row, a.s.col)
		a.jump(matched.Line-1, matched.Column-1)
		a.s.focus = focusEditor
		a.s.console = nil
		a.draw()
	case '#': // find
		keyword := []rune(cmd[1:])
		if len(keyword) == 0 {
			return
		}
		row := a.s.row
		col := a.s.col
		var reverse bool
		start := a.s.line(row)
		for e := start; ; e = e.Next() {
			if e == nil {
				// reverse
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
			line := string(e.Value.([]rune))
			if i := strings.Index(strings.ToLower(line[col:]), strings.ToLower(string(keyword))); i >= 0 {
				a.recordPositon(a.s.row, a.s.col)
				a.jump(row, col+i+len(keyword))
				a.s.selection = &Selection{
					startRow: row,
					endRow:   row,
					startCol: col + i,
					endCol:   col + i + len(keyword),
				}
				a.setConsole(cmd) // incremental search
				a.draw()
				return
			}
			row++
			col = 0
		}
	}
}

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

var prevHighlightLine int

// syncCursor sync cursor position and show it.
// when rendering line highlight, call this method after editor redraw.
func (a *App) syncCursor() {
	switch a.s.focus {
	case focusEditor:
		if a.s.row < a.s.top || a.s.row > (a.s.top+len(a.editor)-1) {
			screen.HideCursor()
			return
		}
		lineElement := a.s.line(a.s.row)
		if lineElement == nil {
			if a.s.row == 0 && a.s.col == 0 {
				screen.ShowCursor(a.editor[0].x, a.editor[0].y)
			}
			return
		}

		line := lineElement.Value.([]rune)
		screenCol := columnToScreenWidth(line, a.s.col) - a.s.left
		x := a.editor[0].x + a.s.lineNumLen() + screenCol
		y := a.editor[0].y + a.s.row - a.s.top
		if x < a.editor[0].x || x >= a.editor[0].x+a.editor[0].w {
			screen.HideCursor() // Hide cursor if out of view
			return
		}
		screen.ShowCursor(x, y)

		if sel := a.s.selected(); sel != nil && sel.startRow <= a.s.row && a.s.row <= sel.endRow {
			return
		}
		// Render current line highlight
		w, _ := screen.Size()
		for x := a.editor[0].x + a.s.lineNumLen(); x < w; x++ {
			r, _, style, _ := screen.GetContent(x, y)
			screen.SetContent(x, y, r, nil, style.Background(tcell.ColorLightYellow))
		}
		if a.s.row != prevHighlightLine {
			prevY := a.editor[0].y + prevHighlightLine - a.s.top
			for x := a.editor[0].x + a.s.lineNumLen(); x < w; x++ {
				r, _, style, _ := screen.GetContent(x, prevY)
				screen.SetContent(x, prevY, r, nil, style.Background(tcell.ColorWhite))
			}
			prevHighlightLine = a.s.row
		}
	case focusConsole:
		// Calculate visual width of console text up to cursor
		consoleRunes := []rune(a.s.console)
		if a.s.consoleCursor > len(consoleRunes) {
			a.s.consoleCursor = len(consoleRunes)
		}
		consoleWidth := 0
		for i := 0; i < a.s.consoleCursor && i < len(consoleRunes); i++ {
			consoleWidth += runewidth.RuneWidth(consoleRunes[i])
		}
		screen.ShowCursor(a.console.x+consoleWidth, a.console.y)
	default:
		screen.HideCursor()
	}
}

func leadingWhitespaces(line []rune) int {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return i
		}
	}
	return len(line)
}

var timeLastKey time.Time

func (a *App) editorEvent(ev *tcell.EventKey) {
	defer func() {
		a.syncCursor()
		a.drawStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
			a.s.upDownCol = -1
		}
		timeLastKey = time.Now()
	}()
	switch ev.Key() {
	case tcell.KeyCtrlZ:
		a.s.undo()
		a.drawEditor()
	case tcell.KeyCtrlY:
		a.s.redo()
		a.drawEditor()
	case tcell.KeyCtrlA:
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		a.jump(a.s.row, leadingWhitespaces(e.Value.([]rune)))
	case tcell.KeyCtrlE:
		a.jump(a.s.row, -1)
	case tcell.KeyBacktab:
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		line := e.Value.([]rune)
		if len(line) > 0 && line[0] == '\t' {
			e.Value = line[1:]
			a.drawEditorLine(a.s.row, line[1:])
			a.s.col--
			a.s.recordEdit(Edit{
				row:     a.s.row,
				col:     0,
				oldText: "\t",
				kind:    editDelete,
			})
		}
	case tcell.KeyRune:
		var line []rune
		e := a.s.line(a.s.row)
		if e == nil {
			// No line exists, create a new one
			line = []rune{ev.Rune()}
			a.s.lines.PushBack(line)
			a.s.recordEdit(Edit{
				row:     a.s.row,
				col:     a.s.col,
				newText: string(ev.Rune()),
				kind:    editInsert,
			})
			a.s.col++
			a.drawEditorLine(a.s.row, line)
			return
		}

		if sel := a.s.selected(); sel != nil {
			// Delete the selected text
			deletedText := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil

			// Insert the new rune
			line = a.s.line(a.s.row).Value.([]rune)
			newText := string(ev.Rune())
			line = slices.Insert(line, a.s.col, ev.Rune())
			a.s.line(a.s.row).Value = line
			a.s.col += len(newText)

			a.s.recordEdit(Edit{
				row:     sel.startRow,
				col:     sel.startCol,
				oldText: deletedText,
				newText: newText,
				kind:    editReplace,
			})
			if sel.startRow != sel.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else {
				a.drawEditorLine(a.s.row, line)
			}
			return
		}

		// No selection, insert rune normally
		line = e.Value.([]rune)
		line = slices.Insert(line, a.s.col, ev.Rune())
		e.Value = line
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col,
			newText: string(ev.Rune()),
			kind:    editInsert,
		})
		a.drawEditorLine(a.s.row, line)
		a.jump(a.s.row, a.s.col+1)
	case tcell.KeyEnter:
		a.s.lastEdit = nil
		a.s.undoStack = nil
		a.s.redoStack = nil
		if e := a.s.line(a.s.row); e == nil {
			// file end
			a.s.lines.PushBack([]rune{})
		} else {
			// break the line
			line := e.Value.([]rune)
			n := leadingWhitespaces(line[:a.s.col])
			// TODO: distinct Enter from keyboard and clipboard
			if n == 0 || time.Since(timeLastKey) < 10*time.Millisecond {
				a.s.lines.InsertAfter(line[a.s.col:], e)
			} else {
				// auto-indent
				newLine := slices.Concat(line[:n], line[a.s.col:])
				a.s.lines.InsertAfter(newLine, e)
			}
			e.Value = line[:a.s.col]
			a.s.col = n
		}
		a.s.row++
		a.jump(a.s.row, a.s.col)
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// delete selection
		if sel := a.s.selected(); sel != nil {
			deletedText := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil
			a.s.recordEdit(Edit{
				row:     sel.startRow,
				col:     sel.startCol,
				oldText: deletedText,
				kind:    editDelete,
			})
			if sel.startRow != sel.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else if line := a.s.line(a.s.row); line != nil {
				a.drawEditorLine(a.s.row, line.Value.([]rune))
			}
			return
		}

		// backspace at line start, delete current line and move up
		if a.s.col == 0 {
			if a.s.row == 0 {
				return // no line to delete
			}

			a.s.lastEdit = nil
			a.s.undoStack = nil
			a.s.redoStack = nil
			element := a.s.line(a.s.row)
			prevElement := element.Prev()
			prevLine := prevElement.Value.([]rune)
			a.s.col = len(prevLine)
			prevElement.Value = append(prevLine, element.Value.([]rune)...)
			a.s.lines.Remove(element)
			a.s.row--
			a.jump(a.s.row, a.s.col)
			a.drawEditor()
			return
		}

		element := a.s.line(a.s.row)
		line := element.Value.([]rune)
		deleted := line[a.s.col-1]
		line = append(line[:a.s.col-1], line[a.s.col:]...)
		element.Value = line
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col - 1,
			oldText: string(deleted),
			kind:    editDelete,
		})
		a.s.col--
		a.jump(a.s.row, a.s.col)
		a.drawEditorLine(a.s.row, line)
	case tcell.KeyLeft:
		a.s.lastEdit = nil
		// move cursor to the start of the selection
		if selection := a.s.selected(); selection != nil {
			a.s.row = selection.startRow
			a.s.col = selection.startCol
			a.unselect()
			return
		}

		// file start
		if a.s.row == 0 && a.s.col == 0 {
			return
		}

		if a.s.col == 0 {
			a.jump(a.s.row-1, -1)
			return
		}
		a.jump(a.s.row, a.s.col-1)
	case tcell.KeyRight:
		a.s.lastEdit = nil
		// move cursor to the end of the selection
		if selection := a.s.selected(); selection != nil {
			a.s.row = selection.endRow
			a.s.col = selection.endCol
			a.unselect()
			return
		}

		// command+right go to the end of line, works in kitty
		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.jump(a.s.row, -1)
			return
		}

		lineItem := a.s.line(a.s.row)
		if lineItem == nil {
			return
		}
		line := lineItem.Value.([]rune)
		// middle of the line
		if a.s.col < len(line) {
			a.jump(a.s.row, a.s.col+1)
			return
		}
		// file end
		if lineItem.Next() == nil {
			return
		}
		a.jump(a.s.row+1, 0)
	case tcell.KeyUp:
		a.s.lastEdit = nil
		a.unselect()

		if a.s.row == 0 {
			return // already at the top
		}

		// command+up go to the start of file, works in kitty
		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.jump(0, 0)
			return
		}

		a.s.row--
		line := a.s.line(a.s.row).Value.([]rune)
		if len(line) == 0 {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		a.jump(a.s.row, a.s.col)
	case tcell.KeyDown:
		a.s.lastEdit = nil
		a.unselect()

		if a.s.row == a.s.lines.Len()-1 {
			return // already at the bottom
		}

		// command+down go to the end of file, works in kitty
		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.jump(-1, -1)
			return
		}

		a.s.row++
		line := a.s.line(a.s.row).Value.([]rune)
		if len(line) == 0 {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		a.jump(a.s.row, a.s.col)
	case tcell.KeyHome:
		a.s.lastEdit = nil
		a.unselect()

		// move to the first non-whitespace character
		line := a.s.line(a.s.row)
		if line == nil {
			return
		}
		text := line.Value.([]rune)
		for i, r := range text {
			if r != ' ' && r != '\t' {
				a.s.col = i
				break
			}
		}
		a.jump(a.s.row, a.s.col)
	case tcell.KeyEnd:
		a.s.lastEdit = nil
		a.unselect()
		a.jump(a.s.row, -1)
	case tcell.KeyTAB:
		a.s.recordEdit(Edit{row: a.s.row, col: a.s.col, newText: "\t", kind: editInsert})
		char := '\t'
		e := a.s.line(a.s.row)
		if e == nil {
			e = a.s.lines.PushBack([]rune{char})
		} else {
			line := e.Value.([]rune)
			line = slices.Insert(line, a.s.col, char)
			a.s.col++
			e.Value = line
		}
		a.drawEditorLine(a.s.row, e.Value.([]rune))
	case tcell.KeyPgUp:
		a.unselect()
		// go to previous page or the top of the page
		a.s.row -= len(a.editor) - 2
		if a.s.row < 0 {
			a.s.row = 0
		}
		a.jump(a.s.row, a.s.col)
	case tcell.KeyPgDn:
		a.unselect()
		// go to next page or the bottom of the page
		a.s.row += len(a.editor) - 2
		if a.s.row >= a.s.lines.Len() {
			a.s.row = a.s.lines.Len() - 1
		}
		a.jump(a.s.row, a.s.col)
	case tcell.KeyCtrlC:
		if sel := a.s.selected(); sel != nil {
			e := a.s.line(sel.startRow)
			var copied []rune
			if sel.startRow == sel.endRow {
				// Single line selection
				line := e.Value.([]rune)
				copied = append(copied, line[sel.startCol:sel.endCol]...)
			} else {
				for i := sel.startRow; i <= sel.endRow && e != nil; i++ {
					text := e.Value.([]rune)
					switch i {
					case sel.startRow:
						copied = append(copied, text[sel.startCol:]...)
						copied = append(copied, '\n')
					case sel.endRow:
						copied = append(copied, text[:sel.endCol]...)
					default:
						copied = append(copied, text...)
						copied = append(copied, '\n')
					}
					e = e.Next()
				}
			}
			a.s.clipboard = string(copied)
			return
		}

		// Copy the current e to clipboard
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		line := e.Value.([]rune)
		if len(line) == 0 {
			return
		}
		a.s.clipboard = string(line)
	case tcell.KeyCtrlX:
		if sel := a.s.selected(); sel != nil {
			// Cut the selected text
			deletedText := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil
			a.s.recordEdit(Edit{
				row:     sel.startRow,
				col:     sel.startCol,
				oldText: deletedText,
				kind:    editDelete,
			})
			a.s.clipboard = deletedText
			if sel.startRow != sel.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else if line := a.s.line(a.s.row); line != nil {
				a.drawEditorLine(a.s.row, line.Value.([]rune))
			}
			return
		}

		// Cut the current e
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		line := e.Value.([]rune)
		if len(line) == 0 {
			return
		}
		deletedText := a.s.deleteRange(a.s.row, 0, a.s.row, len(line))
		a.s.clipboard = deletedText
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     0,
			oldText: deletedText,
			kind:    editDelete,
		})
		a.drawEditor()
	case tcell.KeyCtrlV:
		if a.s.clipboard == "" {
			return
		}
		if sel := a.s.selected(); sel != nil {
			deleted := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil
			a.s.insertAt(a.s.clipboard, sel.startRow, sel.startCol)
			a.s.recordEdit(Edit{
				row:     sel.startRow,
				col:     sel.endCol,
				oldText: deleted,
				newText: a.s.clipboard,
				kind:    editReplace,
			})
		} else {
			row, col := a.s.row, a.s.col
			a.s.insertAt(a.s.clipboard, row, col)
			a.s.recordEdit(Edit{
				row:     row,
				col:     col,
				newText: a.s.clipboard,
				kind:    editInsert,
			})
		}
		a.drawEditor()
	case tcell.KeyCtrlUnderscore: // does not work in kitty
		a.goBack()
	case tcell.KeyEscape:
		a.s.selection = nil
		a.drawEditor()
	}
}

// recordPositon record the cursor position.
// Do not make it into method App.jump, for not every jump worth going back
func (a *App) recordPositon(row, col int) {
	a.s.backStack = append(a.s.backStack, row, col)
	// clear forward stack on new jump
	a.s.forwardStack = nil
}

func (a *App) goBack() {
	if len(a.s.backStack) < 2 {
		return
	}
	a.s.forwardStack = append(a.s.forwardStack, a.s.row, a.s.col)
	a.jump(a.s.backStack[len(a.s.backStack)-2], a.s.backStack[len(a.s.backStack)-1])
	a.s.backStack = a.s.backStack[:len(a.s.backStack)-2]
}

func (a *App) goForward() {
	if len(a.s.forwardStack) < 2 {
		return
	}
	a.s.backStack = append(a.s.backStack, a.s.row, a.s.col)
	a.jump(a.s.forwardStack[len(a.s.forwardStack)-2], a.s.forwardStack[len(a.s.forwardStack)-1])
	a.s.forwardStack = a.s.forwardStack[:len(a.s.forwardStack)-2]
}

// insertAt inserts a string at a specific position in the editor.
// If s contains multiple lines, it will be split and inserted accordingly.
// It updates the cursor position to the end of the inserted text.
func (st *State) insertAt(s string, row, col int) {
	if s == "" {
		return
	}

	e := st.line(row)
	if e == nil {
		e = st.lines.PushBack([]rune{})
	}

	lines := strings.Split(s, "\n")
	// Single line
	if len(lines) == 1 {
		chars := []rune(lines[0])
		line := slices.Insert(e.Value.([]rune), col, chars...)
		e.Value = line
		st.row = row
		st.col = col + len(chars)
		return
	}

	// multiple lines
	origin := e.Value.([]rune)
	for i, line := range lines {
		chars := []rune(line)
		if i == 0 {
			// First line
			e.Value = append(origin[:col], chars...)
		} else if i == len(lines)-1 {
			// Last line
			st.lines.InsertAfter(append(chars, origin[col:]...), e)
		} else {
			// Middle lines
			e = st.lines.InsertAfter(chars, e)
		}
	}
	st.row += len(lines) - 1
	st.col = len([]rune(lines[len(lines)-1]))
}

// unselect cancel the selection and redraws the affected lines.
func (a *App) unselect() {
	selection := a.s.selected()
	if selection == nil {
		return
	}

	a.s.selection = nil
	line := a.s.line(selection.startRow)
	for i := selection.startRow; i <= selection.endRow && line != nil; i++ {
		a.drawEditorLine(i, line.Value.([]rune))
		line = line.Next()
	}
}

// selected returns a copy of the current selection,
// ensuring it is in a consistent order.
// It returns nil if no avaiable selection exists.
func (st *State) selected() *Selection {
	if st.selection == nil {
		return nil
	}
	if st.selection.startRow == st.selection.endRow &&
		st.selection.startCol == st.selection.endCol {
		// No selection
		return nil
	}

	sel := *st.selection
	if sel.startRow > sel.endRow ||
		(sel.startRow == sel.endRow && sel.startCol > sel.endCol) {
		// Swap if selection is reversed
		sel.startRow, sel.endRow = sel.endRow, sel.startRow
		sel.startCol, sel.endCol = sel.endCol, sel.startCol
	}
	return &sel
}

// deleteRange deletes a range of text [startRow:startCol, endRow:endCol) from the editor
// and move the cursor to the start of the deleted range.
// It returns the deleted text as a string.
func (st *State) deleteRange(startRow, startCol, endRow, endCol int) string {
	var deleted strings.Builder
	if startRow == endRow {
		// Single-line
		element := st.line(startRow)
		line := element.Value.([]rune)
		deleted.WriteString(string(line[startCol:endCol]))
		line = slices.Delete(line, startCol, endCol)
		element.Value = line
	} else {
		// Multi-element
		element := st.line(startRow)
		firstLineLeft := element.Value.([]rune)[:startCol]
		for i := startRow; i <= endRow && element != nil; i++ {
			line := element.Value.([]rune)
			next := element.Next()
			switch i {
			case startRow:
				deleted.WriteString(string(line[startCol:]))
				deleted.WriteString("\n")
				st.lines.Remove(element)
			case endRow:
				deleted.WriteString(string(line[:endCol]))
				element.Value = append(firstLineLeft, line[endCol:]...)
			default:
				deleted.WriteString(string(line))
				deleted.WriteString("\n")
				st.lines.Remove(element)
			}
			element = next
		}
	}
	st.row = startRow
	st.col = startCol
	return deleted.String()
}

const (
	editInsert = iota
	editDelete
	editReplace
)

type Edit struct {
	row     int
	col     int
	oldText string
	newText string
	kind    int
	time    time.Time
}

func reverse(e Edit) Edit {
	switch e.kind {
	case editInsert:
		return Edit{
			row:     e.row,
			col:     e.col,
			oldText: e.newText,
			kind:    editDelete,
		}
	case editDelete:
		return Edit{
			row:     e.row,
			col:     e.col,
			newText: e.oldText,
			kind:    editInsert,
		}
	case editReplace:
		return Edit{
			row:     e.row,
			col:     e.col,
			oldText: e.newText,
			newText: e.oldText,
			kind:    editReplace,
		}
	default:
		return Edit{}
	}
}

func (st *State) undo() {
	if len(st.undoStack) == 0 {
		return
	}

	e := st.undoStack[len(st.undoStack)-1]
	st.undoStack = st.undoStack[:len(st.undoStack)-1]
	st.applyEdit(reverse(e))
	st.redoStack = append(st.redoStack, e)
}

func (st *State) redo() {
	if len(st.redoStack) == 0 {
		return
	}
	e := st.redoStack[len(st.redoStack)-1]
	st.redoStack = st.redoStack[:len(st.redoStack)-1]
	st.applyEdit(e)
	st.undoStack = append(st.undoStack, e)
}

func (st *State) applyEdit(e Edit) {
	switch e.kind {
	case editInsert:
		st.insertAt(e.newText, e.row, e.col)
	case editDelete:
		lines := strings.Split(e.oldText, "\n")
		if len(lines) == 1 {
			st.deleteRange(e.row, e.col, e.row, e.col+len(e.oldText))
		} else {
			st.deleteRange(e.row, e.col, e.row+len(lines)-1, len(lines[len(lines)-1]))
		}
	case editReplace:
		lines := strings.Split(e.oldText, "\n")
		if len(lines) == 1 {
			st.deleteRange(e.row, e.col, e.row, e.col+len(e.oldText))
		} else {
			st.deleteRange(e.row, e.col, e.row+len(lines)-1, len(lines[len(lines)-1]))
		}
		st.insertAt(e.newText, e.row, e.col)
	}
}

// recordEdit adds an edit operation to the undo stack with intelligent coalescing.
// It merges consecutive edits of the same type that occur within 1 second on the same row
// to create more intuitive undo/redo behavior.
func (st *State) recordEdit(e Edit) {
	now := time.Now()

	if st.lastEdit != nil && e.kind == st.lastEdit.kind &&
		e.kind != editReplace && // Skip coalescing for replaces
		e.row == st.lastEdit.row && now.Sub(st.lastEdit.time) < time.Second {
		if e.kind == editInsert && st.lastEdit.col+len(st.lastEdit.newText) == e.col {
			st.lastEdit.newText += e.newText
			st.lastEdit.time = now
			return
		}

		if e.kind == editDelete && e.col == st.lastEdit.col-len(e.oldText) {
			st.lastEdit.oldText = e.oldText + st.lastEdit.oldText
			st.lastEdit.col = e.col
			st.lastEdit.time = now
			return
		}
	}

	e.time = now
	st.undoStack = append(st.undoStack, e)
	st.redoStack = nil
	st.lastEdit = &st.undoStack[len(st.undoStack)-1]
}

type SymbolKind string

const (
	SymbolFunc   SymbolKind = "func"
	SymbolType   SymbolKind = "type"
	SymbolVar    SymbolKind = "var"
	SymbolConst  SymbolKind = "const"
	SymbolImport SymbolKind = "import"
	SymbolField  SymbolKind = "field"
)

type Symbol struct {
	Name     string     // e.g., "Foo"
	Kind     SymbolKind // e.g., "func", "type"
	File     string     // absolute or relative path
	Line     int        // line number
	Column   int        // optional, for precision
	Receiver string     // for method: struct name, for field: struct name
}

func ParseSymbol(filename string, src string) (map[string][]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}

	index := make(map[string][]Symbol)
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {

		case *ast.FuncDecl:
			pos := fset.Position(node.Pos())
			receiver := ""
			if node.Recv != nil && len(node.Recv.List) > 0 {
				typ := node.Recv.List[0].Type
				switch t := typ.(type) {
				case *ast.Ident:
					receiver = t.Name
				case *ast.StarExpr:
					if ident, ok := t.X.(*ast.Ident); ok {
						receiver = ident.Name
					}
				}
			}
			sym := Symbol{
				Name:     node.Name.Name,
				Kind:     SymbolFunc,
				File:     filename,
				Line:     pos.Line,
				Column:   pos.Column,
				Receiver: receiver,
			}
			index[sym.Name] = append(index[sym.Name], sym)

		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch ts := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(ts.Pos())
					sym := Symbol{
						Name:   ts.Name.Name,
						Kind:   SymbolType,
						File:   filename,
						Line:   pos.Line,
						Column: pos.Column,
					}
					index[sym.Name] = append(index[sym.Name], sym)

					// struct fields
					if structType, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range structType.Fields.List {
							for _, name := range field.Names {
								fieldPos := fset.Position(name.Pos())
								fieldSym := Symbol{
									Name:     name.Name,
									Kind:     SymbolField,
									File:     filename,
									Line:     fieldPos.Line,
									Column:   fieldPos.Column,
									Receiver: ts.Name.Name,
								}
								index[fieldSym.Name] = append(index[fieldSym.Name], fieldSym)
							}
						}
					}

				case *ast.ValueSpec:
					for _, name := range ts.Names {
						pos := fset.Position(name.Pos())
						kind := SymbolVar
						if node.Tok == token.CONST {
							kind = SymbolConst
						}
						sym := Symbol{
							Name:   name.Name,
							Kind:   kind,
							File:   filename,
							Line:   pos.Line,
							Column: pos.Column,
						}
						index[sym.Name] = append(index[sym.Name], sym)
					}
				}
			}
		}
		return true
	})
	return index, nil
}

var (
	styleBase      = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorWhite)
	styleKeyword   = styleBase.Foreground(tcell.ColorRebeccaPurple).Italic(true).Bold(true)
	styleString    = styleBase.Foreground(tcell.ColorDarkRed)
	styleComment   = styleBase.Foreground(tcell.ColorGray)
	styleNumber    = styleBase.Foreground(tcell.ColorBrown)
	styleHighlight = styleBase.Foreground(tcell.ColorBlack).Background(tcell.ColorLightSteelBlue)

	cursorColor = tcell.ColorDarkGray
)

func highlightGoLine(line []rune) []textStyle {
	var parts []textStyle

	var inString bool
	var inComment bool
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			w := word.String()
			if token.IsKeyword(w) {
				parts = append(parts, textStyle{text: []rune(w), style: styleKeyword})
			} else if _, err := strconv.Atoi(w); err == nil {
				parts = append(parts, textStyle{text: []rune(w), style: styleNumber})
			} else {
				parts = append(parts, textStyle{text: []rune(w), style: styleBase})
			}
			word.Reset()
		}
	}

	for i, c := range line {
		// Line comment
		if !inString && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			flushWord()
			parts = append(parts, textStyle{text: line[i:], style: styleComment})
			return parts
		}

		// Strings
		if c == '"' && !inComment {
			if inString {
				word.WriteRune(c)
				parts = append(parts, textStyle{text: []rune(word.String()), style: styleString})
				word.Reset()
				inString = false
			} else {
				flushWord()
				word.WriteRune(c)
				inString = true
			}
			continue
		}

		if inString {
			word.WriteRune(c)
			continue
		}

		// Word boundary
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' {
			word.WriteRune(c)
		} else {
			flushWord()
			parts = append(parts, textStyle{text: []rune{c}, style: styleBase})
		}
	}

	flushWord()
	return parts
}
