package main

import (
	"bufio"
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"

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

type App struct {
	s       *State
	tabbar  View
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
	command       []rune // command in the console
	commandCursor int    // Cursor position in the console
	focus         int    // focus on editor or console
	lineNumber    bool   // Whether to show line numbers in the editor
	clipboard     string
	files         []string // top level file names
	options       []string // options listed in the status bar
	optionIdx     int      // current option index
}

type Tab struct {
	filename     string
	lines        *list.List          // element is rune slice
	row          int                 // Current row position (starts from 0)
	col          int                 // Current column position (starts from 0)
	top          int                 // vertical scroll  (starts from 0)
	left         int                 // horizontal scroll  (starts from 0)
	upDownCol    int                 // Column to maintain while navigating up/down
	symbols      map[string][]Symbol // symbol name to list of symbols
	hint         string
	hintOff      int
	selecting    bool
	selection    *Selection
	changes      []Change
	changeIndex  int
	lastChange   *Change
	backStack    []int
	forwardStack []int
	prevLineNum  int
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

	if i < t.lines.Len()/2 {
		e := t.lines.Front()
		for range i {
			e = e.Next()
		}
		return e
	}

	e := t.lines.Back()
	for j := t.lines.Len() - 1; j > i; j-- {
		e = e.Prev()
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
	st.focus = focusEditor
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

// drawTexts draw inline texts with different styles.
func (v *View) drawTexts(texts []textStyle) {
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
	a.tabbar = View{0, 0, w, 1, styleComment}
	a.editor = make([]*View, h-3)
	for i := range a.editor {
		a.editor[i] = &View{0, i + a.tabbar.h, w, 1, tcell.StyleDefault}
	}
	a.status = View{0, h - 2, w, 1, styleComment}
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
	a.console.draw(a.s.command)
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
		style := a.tabbar.style
		if i == a.s.tabIdx {
			style = styleBase
		}
		ts = append(ts, textStyle{text: []rune(name), style: style})
		ts = append(ts, textStyle{text: []rune{' '}})
		ts = append(ts, textStyle{text: []rune(labelClose)})
		ts = append(ts, textStyle{text: []rune{' '}})
		for _, c := range name {
			totalTabWidth += runewidth.RuneWidth(c)
		}
		totalTabWidth += len(labelClose) + 2
	}

	menuS := strings.Join(menu, " ")
	padding := a.tabbar.w - totalTabWidth - len(menuS)
	if padding > 0 {
		ts = append(ts, textStyle{text: []rune(strings.Repeat(" ", padding))})
	}
	ts = append(ts, textStyle{text: []rune(menuS)})
	a.tabbar.drawTexts(ts)
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

// drawEditorLine draws the line with automatic tab expansion and syntax highlight,
// highlights the line number in the gutter if necessary.
func (a *App) drawEditorLine(row int, line []rune) {
	if row < a.s.top || row >= a.s.top+len(a.editor) {
		// out of viewport
		return
	}

	var lineNum textStyle
	if a.s.lineNumber {
		lineNum.text = []rune(a.s.newLineNum(row))
		lineNum.style = styleComment
		if row == a.s.row {
			lineNum.style = styleBase.Background(tcell.ColorLightGray)
		}
	}
	if len(line) == 0 {
		texts := []textStyle{lineNum}
		if sel := a.s.selected(); sel != nil && sel.startRow <= row && row <= sel.endRow {
			// make selection visible on empty line
			style := styleBase.Background(tcell.ColorLightSteelBlue)
			texts = append(texts, textStyle{text: []rune{' '}, style: style})
		}
		a.editor[row-a.s.top].drawTexts(texts)
		return
	}

	// Adjust for horizontal scroll
	screenLine := expandTabs(line)
	if a.s.left > 0 {
		screenCol := 0
		for i, r := range screenLine {
			screenCol += runewidth.RuneWidth(r)
			if screenCol >= a.s.left {
				screenLine = screenLine[i+1:]
				break
			}
		}
		if screenCol < a.s.left {
			a.editor[row-a.s.top].drawTexts([]textStyle{lineNum})
			return
		}
	}

	// highlight syntax
	var coloredLine []textStyle
	if filepath.Ext(a.s.filename) == ".go" {
		coloredLine = highlightGoLine(screenLine)
	} else {
		coloredLine = []textStyle{{text: screenLine, style: styleBase}}
	}

	// highlight selection
	if sel := a.s.selected(); sel != nil && sel.startRow <= row && row <= sel.endRow {
		start, end := 0, len(screenLine)
		if sel.startRow == row {
			start = columnToVisual(line, sel.startCol) - a.s.left
		}
		if sel.endRow == row {
			end = columnToVisual(line, sel.endCol) - a.s.left
		}

		i := 0
		newLine := make([]textStyle, 0, len(screenLine))
		for _, ts := range coloredLine {
			for _, r := range ts.text {
				if start <= i && i < end {
					style := ts.style.Background(tcell.ColorLightSteelBlue)
					newLine = append(newLine, textStyle{text: []rune{r}, style: style})
				} else {
					newLine = append(newLine, textStyle{text: []rune{r}, style: ts.style})
				}
				i++
			}
		}
		coloredLine = newLine
	} else if a.s.hint != "" && row == a.s.row {
		hint := a.s.hint[a.s.hintOff:]
		coloredLine = append(coloredLine, textStyle{text: []rune(hint), style: styleComment})
	}
	a.editor[row-a.s.top].drawTexts(slices.Concat([]textStyle{lineNum}, coloredLine))
}

func (a *App) drawEditor() {
	if a.s.lines.Len() == 0 {
		// clear the editor area
		for _, lineView := range a.editor {
			lineView.draw(nil)
		}
		return
	}

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

const (
	focusEditor = iota
	focusConsole
)

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

	app := &App{
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
		s: &State{
			lineNumber: true,
			tabs:       []*Tab{{filename: "", lines: list.New()}},
		},
	}
	app.s.Tab = app.s.tabs[0]
	go app.commandLoop()
	if len(os.Args) >= 2 {
		filename := os.Args[1]
		app.s.filename = filename
		f, err := os.Open(filename)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Println(err)
				return
			}
		} else {
			err = app.s.loadSource(f)
			f.Close()
			if err != nil {
				fmt.Println(err)
				return
			}
		}
	}

	// Initialize screen
	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("%+v", err)
	}
	if err := s.Init(); err != nil {
		log.Fatalf("%+v", err)
	}
	screen = s
	s.SetStyle(styleBase)
	s.SetCursorStyle(tcell.CursorStyleBlinkingBar, cursorColor)
	s.EnableMouse()
	s.EnablePaste()
	s.Clear()
	quit := func() {
		err := recover()
		s.Fini()
		if err != nil {
			panic(err)
		}
	}
	defer quit()
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
					var git bool
					root, err := filepath.Abs(".")
					if err != nil {
						log.Print(err)
						continue
					}
					entries, err := os.ReadDir(root)
					if err != nil {
						log.Print(err)
						continue
					}
					for _, entry := range entries {
						if entry.IsDir() && entry.Name() == ".git" {
							git = true
							break
						}
					}
					if !git {
						// only read sub-folder recursively for git project
						continue
					}

					var files []string
					err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if strings.HasPrefix(d.Name(), ".") && d.IsDir() {
							return filepath.SkipDir
						}
						if strings.HasPrefix(d.Name(), ".") || d.IsDir() {
							return nil
						}
						rel, err := filepath.Rel(root, path)
						if err != nil {
							return err
						}
						files = append(files, rel)
						return nil
					})
					if err != nil {
						log.Print(err)
						continue
					}
					app.s.files = files
					app.s.options = files
					app.s.optionIdx = -1 // no selected option by default
					ts := make([]textStyle, 0, len(app.s.options))
					for _, option := range app.s.options {
						ts = append(ts, textStyle{text: []rune(option + " ")})
					}
					app.status.drawTexts(ts)

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
					app.draw()
					continue
				}

				switch app.s.focus {
				case focusEditor:
					app.editorEvent(ev)
				case focusConsole:
					app.consoleEvent(ev)
				}
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
	if a.tabbar.contains(x, y) {
		if a.s.selecting {
			return
		}
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
		padding := max(0, a.tabbar.w-totalTabWidth-len(menuS))
		// click menu
		menuStart := a.tabbar.x + totalTabWidth + padding
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
		if x < a.tabbar.x+totalTabWidth {
			nameStart := a.tabbar.x
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
		a.s.commandCursor = columnFromScreenWidth([]rune(a.s.command), x-a.console.x)
		a.syncCursor()
		return
	}

	if a.status.contains(x, y) {
		return
	}

	// click editor area
	a.s.focus = focusEditor
	row, col := 0, 0
	if a.s.lines.Len() > 0 {
		row = min(y-a.editor[0].y+a.s.top, a.s.lines.Len()-1)
		line := a.s.line(row).Value.([]rune)
		screenCol := x - a.editor[0].x - a.s.lineNumLen() + a.s.left
		col = columnFromScreenWidth(line, screenCol)
	}

	if !a.s.selecting {
		a.s.selection = &Selection{startRow: row, startCol: col, endRow: row, endCol: col}
		a.s.selecting = true
	} else {
		a.s.selection.endRow = row
		a.s.selection.endCol = col
	}

	a.recordPositon(a.s.row, a.s.col)
	a.jump(row, col)
	a.drawEditor()     // render selection
	a.s.upDownCol = -1 // reset up/down column tracking
	// debug
	if line := a.s.line(row); line != nil {
		log.Printf("clicked line: %s", string(line.Value.([]rune)))
	}
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string, placeholder ...string) {
	a.s.command = []rune(s)
	a.s.commandCursor = len(a.s.command)
	if len(placeholder) == 0 {
		a.console.draw(a.s.command)
		return
	}
	a.console.drawTexts([]textStyle{
		{text: a.s.command},
		{text: []rune(placeholder[0]), style: styleComment},
	})
}

// jump moves the cursor to the specified line and column,
// If row less than 0, jump to the last line.
// If col less than 0, jump to the end of the line.
// To ensure the line is visible, it will redraw the line or the whole editor.
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

	a.s.hint = ""
	if scroll {
		a.drawEditor()
	} else {
		// highlight current line number, cancel previous
		a.drawEditorLine(row, line)
		if row != a.s.prevLineNum && (a.s.top <= a.s.prevLineNum && a.s.prevLineNum < a.s.top+len(a.editor)) {
			if e := a.s.line(a.s.prevLineNum); e != nil {
				a.drawEditorLine(a.s.prevLineNum, e.Value.([]rune))
			}
		}
	}
	a.s.prevLineNum = row
	a.syncCursor()
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	defer func() {
		a.console.draw(a.s.command)
		a.syncCursor()
	}()
	exitConsole := func() {
		a.s.command = nil
		a.s.focus = focusEditor
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		exitConsole()
		// reset matched text
		if line := a.s.line(a.s.row); line != nil {
			a.drawEditorLine(a.s.row, line.Value.([]rune))
		}
	case tcell.KeyEnter:
		cmd := strings.TrimSpace(string(a.s.command))
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
		a.s.command = nil
		a.cmdCh <- cmd
	case tcell.KeyLeft:
		if a.s.commandCursor > 1 {
			a.s.commandCursor--
		}
	case tcell.KeyRight:
		if a.s.commandCursor < len(a.s.command) {
			a.s.commandCursor++
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(a.s.command) == 0 {
			return
		}
		a.s.command = slices.Delete(a.s.command, a.s.commandCursor-1, a.s.commandCursor)
		a.s.commandCursor--
		if len(a.s.command) == 0 {
			a.s.options = a.s.files
			a.s.optionIdx = -1
		} else if char := a.s.command[0]; char == '#' || char == ':' || char == '>' {
			return
		} else if char == '@' {
			keyword := string(a.s.command[1:])
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
				if strings.HasPrefix(strings.ToLower(filter[i]), strings.ToLower(keyword)) {
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
			keyword := string(a.s.command)
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
		a.showOptions()
	case tcell.KeyRune:
		a.s.command = slices.Insert(a.s.command, a.s.commandCursor, ev.Rune())
		a.s.commandCursor++
		switch a.s.command[0] {
		case '>', '#', ':':
			return
		case '@':
			keyword := string(a.s.command[1:])
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
			a.showOptions()
		default: // search file
			if len(a.s.files) == 0 {
				return
			}
			keyword := string(a.s.command)
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
			a.showOptions()
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
		a.showOptions()
	case tcell.KeyCtrlUnderscore:
		// go to previous found keyword
		if len(a.s.command) > 0 && a.s.command[0] == '#' {
			a.goBack()
			keyword := a.s.command[1:]
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
				a.draw()
				return
			}

			file, err := os.Open(filename)
			if err != nil {
				log.Print(err)
				a.status.draw([]rune(err.Error()))
				return
			}
			defer file.Close()
			a.s.tabs = append(a.s.tabs, &Tab{filename: filename})
			a.s.switchTab(len(a.s.tabs) - 1)
			err = a.s.loadSource(file)
			if err != nil {
				log.Print(err)
				a.status.draw([]rune(err.Error()))
				return
			}
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
			lines := make([]string, 0, a.s.lines.Len()+1)
			for e := a.s.lines.Front(); e != nil; e = e.Next() {
				lines = append(lines, string(e.Value.([]rune)))
			}
			// ensure a single newline at the end of file
			if len(lines) == 0 || lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			src := []byte(strings.Join(lines, "\n"))
			// format on save
			if filepath.Ext(filename) == ".go" {
				bs, err := format.Source(src)
				if err != nil {
					a.status.draw([]rune(err.Error()))
					log.Print(err)
				} else {
					src = bs
				}
			}
			err := os.WriteFile(filename, src, 0644)
			if err != nil {
				log.Printf("Failed to save file %s: %v", filename, err)
				a.status.draw([]rune("Failed to save file: " + err.Error()))
			} else {
				a.status.draw([]rune("File saved as: " + filename))
				a.s.filename = filename // update current tab
				a.drawTabs()
				a.s.focus = focusEditor
				if err := a.s.loadSource(bytes.NewReader(src)); err != nil {
					a.status.draw([]rune(err.Error()))
					return
				}
				a.s.row = min(a.s.row, a.s.lines.Len()-1)
				a.s.col = 0
				a.drawEditor()
				a.syncCursor()
			}
		case "linenumber":
			// toogle line number display
			a.s.lineNumber = !a.s.lineNumber
			// horizonal scroll may changed, update the cursor
			a.s.focus = focusEditor
			a.jump(a.s.row, a.s.col)
			a.drawEditor()
		case "back":
			a.s.focus = focusEditor
			a.goBack()
		case "forward":
			a.s.focus = focusEditor
			a.goForward()
		default:
			a.status.draw([]rune("unknown command: " + cmd))
		}
	case ':': // go to line
		a.s.focus = focusEditor
		defer a.syncCursor()
		n, err := strconv.Atoi(cmd[1:])
		if err != nil {
			a.status.draw([]rune("Invalid line number"))
			return
		}
		if n > a.s.lines.Len() {
			a.status.draw([]rune("Line number out of range"))
			return
		}
		if n < 0 {
			// input -1, scroll to bottom line
			n = a.s.lines.Len()
		}
		a.jump(n-1, 0)
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
		a.s.command = nil
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

// syncCursor sync cursor position and show it.
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
		a.status.draw([]rune(fmt.Sprintf("Line %d, Column %d ", a.s.row+1, screenCol+1)))
	case focusConsole:
		// Calculate visual width of console text up to cursor
		consoleRunes := []rune(a.s.command)
		if a.s.commandCursor > len(consoleRunes) {
			a.s.commandCursor = len(consoleRunes)
		}
		consoleWidth := 0
		for i := 0; i < a.s.commandCursor && i < len(consoleRunes); i++ {
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
		if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
			a.s.upDownCol = -1
		}
		timeLastKey = time.Now()
	}()
	switch ev.Key() {
	case tcell.KeyCtrlU:
		// delete to line start
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		line := e.Value.([]rune)
		if len(line) == 0 {
			return
		}
		e.Value = line[a.s.col:]
		a.s.recordChange(Change{row: a.s.row, col: 0, oldText: string(line[:a.s.col]), kind: editDelete})
		a.jump(a.s.row, 0)
	case tcell.KeyCtrlZ:
		a.s.undo()
		a.drawEditor()
	case tcell.KeyCtrlY:
		a.s.redo()
		a.drawEditor()
	case tcell.KeyRune:
		defer func() {
			a.s.setHint()
			a.drawEditorLine(a.s.row, a.s.line(a.s.row).Value.([]rune))
		}()
		var line []rune
		e := a.s.line(a.s.row)
		if e == nil {
			line = []rune{ev.Rune()}
			a.s.lines.PushBack(line)
			a.s.recordChange(Change{
				row:     a.s.row,
				col:     a.s.col,
				newText: string(ev.Rune()),
				kind:    editInsert,
			})
			a.jump(a.s.row, a.s.col+1)
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

			a.s.recordChange(Change{
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
		e.Value = slices.Insert(line, a.s.col, ev.Rune())
		a.s.recordChange(Change{
			row:     a.s.row,
			col:     a.s.col,
			newText: string(ev.Rune()),
			kind:    editInsert,
		})
		a.jump(a.s.row, a.s.col+1)
	case tcell.KeyEnter:
		e := a.s.line(a.s.row)
		if e == nil {
			// file end
			a.s.lines.PushBack([]rune{})
			a.s.recordChange(Change{
				newText: "\n",
				row:     a.s.row,
				col:     a.s.col,
				kind:    editInsert,
			})
			a.jump(a.s.row+1, a.s.col)
			a.drawEditor()
			return
		}

		// break the line
		line := e.Value.([]rune)
		e.Value = line[:a.s.col]
		// no auto-indent for the Enter from clipboard
		if a.s.col == 0 || time.Since(timeLastKey) < 10*time.Millisecond {
			a.s.lines.InsertAfter(line[a.s.col:], e)
			a.s.recordChange(Change{newText: "\n", row: a.s.row, col: a.s.col, kind: editInsert})
			a.jump(a.s.row+1, a.s.col)
			a.drawEditor()
			return
		}

		// auto-indent
		var inserted string
		n := leadingWhitespaces(line[:a.s.col])
		indent := make([]rune, 0, n)
		for range n {
			indent = append(indent, '\t')
		}
		if line[a.s.col-1] == '{' && a.s.col < len(line) && line[a.s.col] == '}' {
			// Enter inside {}
			indent = append(indent, '\t')
			nextE := a.s.lines.InsertAfter(indent, e)
			a.s.lines.InsertAfter(slices.Concat(indent[:n], line[a.s.col:]), nextE)
			inserted = "\n" + string(indent) + "\n" + string(indent[:n])
			a.s.recordChange(Change{newText: inserted, row: a.s.row, col: a.s.col, kind: editInsert})
			a.jump(a.s.row+1, len(indent))
		} else {
			newLine := slices.Concat(indent, line[a.s.col:])
			a.s.lines.InsertAfter(newLine, e)
			inserted = "\n" + string(indent)
			a.s.recordChange(Change{newText: inserted, row: a.s.row, col: a.s.col, kind: editInsert})
			a.jump(a.s.row+1, len(indent))
		}
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		defer func() {
			a.s.setHint()
			a.drawEditorLine(a.s.row, a.s.line(a.s.row).Value.([]rune))
		}()
		// delete selection
		if sel := a.s.selected(); sel != nil {
			deletedText := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil
			a.s.recordChange(Change{
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

			element := a.s.line(a.s.row)
			prevElement := element.Prev()
			prevLine := prevElement.Value.([]rune)
			prevElement.Value = append(prevLine, element.Value.([]rune)...)
			a.s.lines.Remove(element)
			a.s.recordChange(Change{
				row:     a.s.row - 1,
				col:     len(prevLine),
				oldText: "\n",
				kind:    editDelete,
			})
			a.jump(a.s.row-1, len(prevLine))
			a.drawEditor()
			return
		}

		element := a.s.line(a.s.row)
		line := element.Value.([]rune)
		deleted := line[a.s.col-1]
		line = append(line[:a.s.col-1], line[a.s.col:]...)
		element.Value = line
		a.s.recordChange(Change{
			row:     a.s.row,
			col:     a.s.col - 1,
			oldText: string(deleted),
			kind:    editDelete,
		})
		a.s.col--
		a.jump(a.s.row, a.s.col)
		// a.drawEditorLine(a.s.row, line)
	case tcell.KeyLeft:
		a.s.lastChange = nil
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
		a.s.lastChange = nil
		// move cursor to the end of the selection
		if selection := a.s.selected(); selection != nil {
			a.s.row = selection.endRow
			a.s.col = selection.endCol
			a.unselect()
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
		a.s.lastChange = nil
		a.unselect()

		if a.s.row == 0 {
			return // already at the top
		}

		lineE := a.s.line(a.s.row)
		prevLineE := lineE.Prev()
		if a.s.upDownCol < 0 {
			a.s.upDownCol = columnToScreenWidth(lineE.Value.([]rune), a.s.col)
		}
		// moving up/down, keep previous column
		col := columnFromScreenWidth(prevLineE.Value.([]rune), a.s.upDownCol)
		a.jump(a.s.row-1, col)
	case tcell.KeyDown:
		a.s.lastChange = nil
		a.unselect()

		if a.s.row == a.s.lines.Len()-1 {
			return // already at the bottom
		}

		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.jump(-1, -1)
			return
		}

		lineE := a.s.line(a.s.row)
		nextE := lineE.Next()
		if a.s.upDownCol < 0 {
			a.s.upDownCol = columnToScreenWidth(lineE.Value.([]rune), a.s.col)
		}
		// moving up/down, keep previous column
		col := columnFromScreenWidth(nextE.Value.([]rune), a.s.upDownCol)
		a.jump(a.s.row+1, col)
	case tcell.KeyHome, tcell.KeyCtrlA:
		a.s.lastChange = nil
		a.unselect()

		// move to the first non-whitespace character
		line := a.s.line(a.s.row)
		if line == nil {
			return
		}
		a.jump(a.s.row, leadingWhitespaces(line.Value.([]rune)))
	case tcell.KeyEnd, tcell.KeyCtrlE:
		a.s.lastChange = nil
		a.unselect()
		a.jump(a.s.row, -1)
	case tcell.KeyTAB:
		// increase indent for selection
		if sel := a.s.selected(); sel != nil {
			a.s.selection = &Selection{
				startRow: sel.startRow,
				startCol: sel.startCol + 1,
				endRow:   sel.endRow,
				endCol:   sel.endCol + 1,
			}
			e := a.s.line(sel.startRow)
			for row := sel.startRow; row <= sel.endRow; row++ {
				if e == nil {
					break
				}
				line := e.Value.([]rune)
				newLine := make([]rune, 0, len(line)+1)
				newLine = append(newLine, '\t')
				newLine = append(newLine, line...)
				e.Value = newLine
				a.drawEditorLine(row, newLine)
				a.s.recordChange(Change{row: row, col: 0, newText: "\t", kind: editInsert})
				if row == a.s.row {
					a.s.col++
				}
				e = e.Next()
			}
			return
		}

		e := a.s.line(a.s.row)
		if e == nil {
			e = a.s.lines.PushBack([]rune{'\t'})
			a.s.recordChange(Change{row: a.s.row, col: a.s.col, newText: string("\t"), kind: editInsert})
			a.s.col++
		} else {
			line := e.Value.([]rune)
			if a.s.hint != "" {
				line = slices.Concat(line[:a.s.col-a.s.hintOff], []rune(a.s.hint), line[a.s.col:])
				a.s.recordChange(Change{
					row:     a.s.row,
					col:     a.s.col - a.s.hintOff,
					oldText: string(line[a.s.col-a.s.hintOff : a.s.col]),
					newText: a.s.hint,
					kind:    editReplace,
				})
				a.s.col += len([]rune(a.s.hint)) - a.s.hintOff
				a.s.hint = ""
			} else {
				line = slices.Insert(line, a.s.col, '\t')
				a.s.recordChange(Change{row: a.s.row, col: a.s.col, newText: string("\t"), kind: editInsert})
				a.s.col++
			}
			e.Value = line
		}
		a.drawEditorLine(a.s.row, e.Value.([]rune))
	case tcell.KeyBacktab:
		// decrease indent
		unindent := func(row int, e *list.Element) {
			if e == nil {
				return
			}
			line := e.Value.([]rune)
			if len(line) == 0 || line[0] != '\t' {
				return
			}
			e.Value = line[1:]
			a.drawEditorLine(row, line[1:])
			if row == a.s.row {
				a.s.col--
			}
			a.s.recordChange(Change{
				row:     row,
				col:     0,
				oldText: "\t",
				kind:    editDelete,
			})
		}
		if sel := a.s.selected(); sel != nil {
			a.s.selection = &Selection{
				startRow: sel.startRow,
				startCol: sel.startCol - 1,
				endRow:   sel.endRow,
				endCol:   sel.endCol - 1,
			}
			e := a.s.line(sel.startRow)
			for row := sel.startRow; row <= sel.endRow; row++ {
				unindent(row, e)
				e = e.Next()
			}
			return
		}

		e := a.s.line(a.s.row)
		unindent(a.s.row, e)
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
			screen.SetClipboard([]byte(string(copied)))
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
		screen.SetClipboard([]byte(string(line)))
	case tcell.KeyCtrlX:
		if sel := a.s.selected(); sel != nil {
			// Cut the selected text
			deletedText := a.s.deleteRange(sel.startRow, sel.startCol, sel.endRow, sel.endCol)
			a.s.selection = nil
			a.s.recordChange(Change{
				row:     sel.startRow,
				col:     sel.startCol,
				oldText: deletedText,
				kind:    editDelete,
			})
			a.s.clipboard = deletedText
			screen.SetClipboard([]byte(deletedText))
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
		screen.SetClipboard([]byte(deletedText))
		a.s.clipboard = deletedText
		a.s.recordChange(Change{
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
			a.s.insertText([]rune(a.s.clipboard), sel.startRow, sel.startCol)
			a.s.recordChange(Change{
				row:     sel.startRow,
				col:     sel.startCol,
				oldText: deleted,
				newText: a.s.clipboard,
				kind:    editReplace,
			})
		} else {
			row, col := a.s.row, a.s.col
			a.s.insertText([]rune(a.s.clipboard), row, col)
			a.s.recordChange(Change{
				row:     row,
				col:     col,
				newText: a.s.clipboard,
				kind:    editInsert,
			})
		}
		a.drawEditor()
	case tcell.KeyCtrlUnderscore:
		a.goBack()
	case tcell.KeyEscape:
		a.s.selection = nil
		a.s.hint = ""
		a.drawEditor()
	case tcell.KeyCtrlB: // go to symbol under cursor
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		line := e.Value.([]rune)
		start := a.s.col - 1
		for start >= 0 && (unicode.IsLetter(line[start]) || unicode.IsDigit(line[start]) || line[start] == '_') {
			start--
		}
		stop := a.s.col
		for stop < len(line) && (unicode.IsLetter(line[stop]) || unicode.IsDigit(line[stop]) || line[stop] == '_') {
			stop++
		}
		word := string(line[start+1 : stop])
		if len(word) == 0 {
			return
		}
		symbols, ok := a.s.symbols[word]
		if !ok {
			return
		}

		if len(symbols) == 1 {
			a.recordPositon(a.s.row, a.s.col)
			a.jump(symbols[0].Line-1, symbols[0].Column-1)
			return
		}
		// multiple symbols found, show options
		var options []string
		for _, sym := range symbols {
			if sym.Receiver != "" {
				options = append(options, sym.Receiver+"."+sym.Name)
			} else {
				options = append(options, sym.Name)
			}
		}
		slices.Sort(options)
		a.setConsole("@" + word)
		a.s.focus = focusConsole
		a.s.options = options
		a.s.optionIdx = 0
		a.showOptions()
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

// insertText inserts the text at the specific position in the editor.
// If s contains multiple lines, it will be split and inserted accordingly.
// It updates the cursor position to the end of the inserted text.
func (st *State) insertText(runes []rune, row, col int) {
	if len(runes) == 0 {
		return
	}

	e := st.line(row)
	if e == nil {
		e = st.lines.PushBack([]rune{})
	}
	line := e.Value.([]rune)
	for _, r := range runes {
		if r == '\n' {
			// break the line
			e.Value = line[:col]
			newLine := line[col:]
			e = st.lines.InsertAfter(newLine, e)
			row++
			col = 0
			line = newLine
		} else {
			line = slices.Insert(line, col, r)
			col++
		}
	}
	e.Value = line
	st.row = row
	st.col = col
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
		// single line
		element := st.line(startRow)
		line := element.Value.([]rune)
		deleted.WriteString(string(line[startCol:endCol]))
		line = slices.Delete(line, startCol, endCol)
		element.Value = line
		st.row = startRow
		st.col = startCol
		return deleted.String()
	}

	// mutiple lines
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
	st.row = startRow
	st.col = startCol
	return deleted.String()
}

const (
	editInsert = iota
	editDelete
	editReplace
)

type Change struct {
	row     int
	col     int
	oldText string
	newText string
	kind    int
	time    time.Time
}

func reverse(c Change) Change {
	switch c.kind {
	case editInsert:
		return Change{
			row:     c.row,
			col:     c.col,
			oldText: c.newText,
			kind:    editDelete,
		}
	case editDelete:
		return Change{
			row:     c.row,
			col:     c.col,
			newText: c.oldText,
			kind:    editInsert,
		}
	case editReplace:
		return Change{
			row:     c.row,
			col:     c.col,
			oldText: c.newText,
			newText: c.oldText,
			kind:    editReplace,
		}
	default:
		return Change{}
	}
}

func (st *State) undo() {
	if st.changeIndex < 0 {
		return
	}
	st.applyChange(reverse(st.changes[st.changeIndex]))
	st.changeIndex--
}

func (st *State) redo() {
	if st.changeIndex >= len(st.changes)-1 {
		return
	}
	st.changeIndex++
	st.applyChange(st.changes[st.changeIndex])
}

func (st *State) applyChange(c Change) {
	switch c.kind {
	case editInsert:
		st.insertText([]rune(c.newText), c.row, c.col)
	case editDelete:
		lines := strings.Split(c.oldText, "\n")
		if len(lines) == 1 {
			st.deleteRange(c.row, c.col, c.row, c.col+len([]rune(c.oldText)))
		} else {
			st.deleteRange(c.row, c.col, c.row+len(lines)-1, len([]rune(lines[len(lines)-1])))
		}
	case editReplace:
		lines := strings.Split(c.oldText, "\n")
		if len(lines) == 1 {
			st.deleteRange(c.row, c.col, c.row, c.col+len([]rune(c.oldText)))
		} else {
			st.deleteRange(c.row, c.col, c.row+len(lines)-1, len([]rune(lines[len(lines)-1])))
		}
		st.insertText([]rune(c.newText), c.row, c.col)
	}
}

// recordChange record change with intelligent coalescing.
// It merges consecutive edits of the same type that occur within 1 second on the same row
// to create more intuitive undo/redo behavior.
func (st *State) recordChange(c Change) {
	now := time.Now()
	if st.lastChange != nil && c.kind == st.lastChange.kind &&
		c.kind != editReplace && // Skip coalescing for replaces
		c.row == st.lastChange.row && now.Sub(st.lastChange.time) < time.Second {
		if c.kind == editInsert && st.lastChange.col+len(st.lastChange.newText) == c.col {
			st.lastChange.newText += c.newText
			st.lastChange.time = now
			return
		}

		if c.kind == editDelete && c.col == st.lastChange.col-len(c.oldText) {
			st.lastChange.oldText = c.oldText + st.lastChange.oldText
			st.lastChange.col = c.col
			st.lastChange.time = now
			return
		}
	}

	c.time = now
	if st.changeIndex < len(st.changes) {
		// clear redo stack on new change
		st.changes = st.changes[:st.changeIndex+1]
	}
	st.changes = append(st.changes, c)
	st.changeIndex = len(st.changes) - 1
	st.lastChange = &st.changes[st.changeIndex]
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

// ParseSymbol parses Go source code and extracts symbols such as functions,
// types, variables, constants, and struct fields.
// If src != nil, it must be string, []byte, or io.Reader.
func ParseSymbol(filename string, src any) (map[string][]Symbol, error) {
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
	styleHighlight = styleBase.Background(tcell.ColorLightSteelBlue)

	cursorColor = tcell.ColorBlack
)

// highlight Go syntax
func highlightGoLine(line []rune) []textStyle {
	var parts []textStyle

	var inString bool
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
		if c == '"' {
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

// loadSource reads lines from r and puts them to current tab's buffer.
// If the file is a Go source file, it also parses and indexes its symbols.
func (st *State) loadSource(r io.Reader) error {
	var lines list.List
	var buf bytes.Buffer
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines.PushBack([]rune(scanner.Text()))
		buf.Write(scanner.Bytes())
		buf.WriteByte('\n')
	}
	back := lines.Back()
	if back == nil || len(back.Value.([]rune)) != 0 {
		// append newline
		lines.PushBack([]rune{})
	}
	err := scanner.Err()
	if err != nil {
		return err
	}
	st.lines = &lines

	if !strings.HasSuffix(st.filename, ".go") {
		return nil
	}
	symbols, err := ParseSymbol(st.filename, buf.Bytes())
	if err != nil {
		log.Printf("parse symbol: %s", err.Error())
		return nil
	}
	st.symbols = symbols
	return nil
}

func (st *State) setHint() {
	if len(st.symbols) == 0 {
		return
	}
	e := st.line(st.row)
	if e == nil {
		return
	}
	line := e.Value.([]rune)
	if st.col != len(line) {
		// only show hint when cursor is at the end of the line
		st.hint = ""
		return
	}

	i := st.col - 1
	for i >= 0 && (unicode.IsLetter(line[i]) || unicode.IsDigit(line[i]) || line[i] == '_') {
		i--
	}
	word := string(line[i+1 : st.col])
	if len(word) < 2 {
		st.hint = ""
		return
	}

	for k := range st.symbols {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(word)) {
			st.hint = k
			st.hintOff = len(word)
			return
		}
	}
	st.hint = ""
}

// showOptions draw options in the status line
func (a *App) showOptions() {
	ts := make([]textStyle, 0, len(a.s.options))
	for i, opt := range a.s.options {
		if i == a.s.optionIdx {
			ts = append(ts, textStyle{text: []rune(opt + " "), style: styleHighlight})
		} else {
			ts = append(ts, textStyle{text: []rune(opt + " ")})
		}
	}
	a.status.drawTexts(ts)
}
