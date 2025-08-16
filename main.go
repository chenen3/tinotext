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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	*Tab          // active tab
	tabs          []*Tab
	activeTabIdx  int
	status        string // Status message displayed in the status bar
	console       string // Console input text
	consoleCursor int    // Cursor position in the console
	focus         int    // Current focus (editor or console)
}

type Tab struct {
	filename     string
	lines        *list.List
	row          int                 // Current row position (starts from 0)
	col          int                 // Current column position (starts from 0)
	scroll       int                 // Scroll position for the editor (starts from 0)
	upDownCol    int                 // Column to maintain while navigating up/down
	symbols      map[string][]Symbol // symbol name to list of symbols
	matchSymbols []Symbol
	matchIdx     int
	completion   string
	selecting    bool
	selection    *Selection
	undoStack    []Edit
	redoStack    []Edit
	lastEdit     *Edit
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

	st.activeTabIdx = i
	st.Tab = st.tabs[i]
}

type View struct {
	x, y, w, h int
	style      tcell.Style
}

// draw draws a line and clears the remaining space
func (v *View) draw(line string) {
	for row := range v.h {
		for col := range v.w {
			if col < len(line) {
				screen.SetContent(v.x+col, v.y+row, rune(line[col]), nil, v.style)
			} else {
				screen.SetContent(v.x+col, v.y+row, ' ', nil, v.style)
			}
		}
	}
}

type textStyle struct {
	text  string
	style tcell.Style
}

// drawText draw inline texts with multiple styles.
// Note that it does not handle tab expansion.
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
func expandTabs(line string) string {
	var result strings.Builder
	for _, char := range line {
		if char == '\t' {
			// Add spaces to reach the next tab stop
			spaces := tabSize - (result.Len() % tabSize)
			result.WriteString(strings.Repeat(" ", spaces))
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}

// columnToScreen converts a column position in the original line to the screen line
func columnToScreen(line string, col int) int {
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
			screenCol++
		}
	}
	return screenCol
}

// columnFromScreen converts a column position in the screen line back to the original line
func columnFromScreen(line string, screenCol int) int {
	if screenCol <= 0 {
		return 0
	}

	originalCol := 0
	currentScreenCol := 0

	for i, char := range line {
		if currentScreenCol >= screenCol {
			return originalCol
		}

		if char == '\t' {
			spaces := tabSize - (currentScreenCol % tabSize)
			currentScreenCol += spaces
		} else {
			currentScreenCol++
		}
		originalCol = i + 1
	}

	return originalCol
}

// draw the whole layout and cursor
func (a *App) draw() {
	a.drawTabs()
	a.drawEditor()
	a.status.draw(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.console.draw(a.s.console)
	a.syncCursor()
}

const (
	labelClose = " x|"
	labelNew   = " New "
	labelOpen  = " Open "
	labelSave  = " Save "
	labelQuit  = " Quit "
)

func (a *App) drawTabs() {
	var ts []textStyle
	var totalTabWidth int
	for i, tab := range a.s.tabs {
		name := tab.filename
		if name == "" {
			name = "untitled"
		}
		if i == a.s.activeTabIdx {
			ts = append(ts, textStyle{text: name, style: a.tab.style.Bold(true).Underline(true).Italic(true)})
			ts = append(ts, textStyle{text: labelClose, style: a.tab.style.Bold(true)})
		} else {
			ts = append(ts, textStyle{text: name})
			ts = append(ts, textStyle{text: labelClose})
		}
		totalTabWidth += len(name) + len(labelClose)
	}

	labels := labelNew + labelOpen + labelSave + labelQuit
	padding := max(0, a.tab.w-totalTabWidth-len(labels))
	if padding > 0 {
		ts = append(ts, textStyle{text: strings.Repeat(" ", padding)})
	}
	ts = append(ts, textStyle{text: labels})
	a.tab.drawText(ts...)
}

// drawEditorLine draws the line with automatic tab expansion and syntax highlight
func (a *App) drawEditorLine(row int, line string) {
	line = expandTabs(line)
	if a.s.filename == "" || !strings.HasSuffix(a.s.filename, ".go") {
		a.editor[row-a.s.scroll].draw(line)
		return
	}
	a.editor[row-a.s.scroll].drawText(highlightGoLine(line)...)
}

func (a *App) drawEditor() {
	e := a.s.lines.Front()
	for range a.s.scroll {
		e = e.Next()
	}
	remainLines := a.s.lines.Len() - a.s.scroll

	var seleStartRow, seleStartCol, seleEndRow, seleEndCol int
	if a.s.selection != nil {
		seleStartRow, seleStartCol = a.s.selection.startRow, a.s.selection.startCol
		seleEndRow, seleEndCol = a.s.selection.endRow, a.s.selection.endCol
		if seleStartRow > seleEndRow {
			seleStartRow, seleEndRow = seleEndRow, seleStartRow
			seleStartCol, seleEndCol = seleEndCol, seleStartCol
		}
	}

	for i, lineView := range a.editor {
		if i >= remainLines {
			lineView.draw("")
			continue
		}

		line := e.Value.(string)
		screenLine := expandTabs(line)
		e = e.Next()

		// selection highlight
		if (seleStartRow != seleEndRow || seleStartCol != seleEndCol) &&
			seleStartRow <= a.s.scroll+i && a.s.scroll+i <= seleEndRow {
			start, end := 0, len(screenLine)
			if seleStartRow == a.s.scroll+i {
				start = columnToScreen(line, seleStartCol)
			}
			if seleEndRow == a.s.scroll+i {
				end = columnToScreen(line, seleEndCol)
			}
			if start > end {
				start, end = end, start
			}
			lineView.drawText(
				textStyle{text: screenLine[:start]},
				textStyle{text: screenLine[start:end], style: styleHighlight},
				textStyle{text: screenLine[end:]},
			)
			continue
		}

		if a.s.filename == "" || !strings.HasSuffix(a.s.filename, ".go") {
			lineView.draw(screenLine)
			continue
		}
		// syntax highlight
		lineView.drawText(highlightGoLine(screenLine)...)
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
				app.s.lines.PushBack(scanner.Text())
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
					app.s.closeTab(app.s.activeTabIdx)
					if len(app.s.tabs) == 0 {
						close(app.done)
						return
					}
					app.draw()
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
					if !strings.HasSuffix(app.s.filename, ".go") {
						app.setStatus("Not a Go file, cannot parse symbols")
						continue
					}
					var src strings.Builder
					for e := app.s.lines.Front(); e != nil; e = e.Next() {
						src.WriteString(e.Value.(string))
						if e != app.s.lines.Back() {
							src.WriteByte('\n')
						}
					}
					symbols, err := ParseSymbol(app.s.filename, src.String())
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
		var totalTabWidth int
		for _, tab := range a.s.tabs {
			tabName := tab.filename
			if tabName == "" {
				tabName = "untitled"
			}
			totalTabWidth += len(tabName) + len(labelClose)
		}
		padding := max(0, a.tab.w-totalTabWidth-len(labelNew)-len(labelOpen)-len(labelSave)-len(labelQuit))

		// special labels area
		specialLabelsStart := a.tab.x + totalTabWidth + padding

		// Check if click is in special labels area
		if x >= specialLabelsStart {
			// New label
			if x < specialLabelsStart+len(labelNew) {
				a.s.tabs = slices.Insert(a.s.tabs, a.s.activeTabIdx+1, &Tab{filename: "", lines: list.New()})
				a.s.switchTab(a.s.activeTabIdx + 1)
				a.draw()
				return
			}
			// Open label
			if x < specialLabelsStart+len(labelNew)+len(labelOpen) {
				a.s.focus = focusConsole
				a.setStatus("open file")
				a.setConsole("")
				a.syncCursor()
				return
			}
			// Save label
			if x < specialLabelsStart+len(labelNew)+len(labelOpen)+len(labelSave) {
				if len(a.s.tabs) > 0 && a.s.activeTabIdx < len(a.s.tabs) {
					a.cmdCh <- ">save " + a.s.filename
				}
				return
			}
			// Quit label
			if x < specialLabelsStart+len(labelNew)+len(labelOpen)+len(labelSave)+len(labelQuit) {
				close(a.done)
				return
			}
			return
		}

		// Check tabs - only if click is in tabs area (not in special labels)
		if x < a.tab.x+totalTabWidth {
			var currentWidth int
			for i, tab := range a.s.tabs {
				tabName := tab.filename
				if tabName == "" {
					tabName = "untitled"
				}

				tabStart := a.tab.x + currentWidth
				tabEnd := tabStart + len(tabName)
				closerEnd := tabEnd + len(labelClose)

				// Check if click is within this tab's area
				if x >= tabStart && x < closerEnd {
					if x < tabEnd {
						// Clicked on tab name - switch tab
						if i != a.s.activeTabIdx {
							a.s.switchTab(i)
							a.s.focus = focusEditor
							a.draw()
						}
					} else {
						// Clicked on tab closer - close tab
						a.s.closeTab(i)
						if len(a.s.tabs) == 0 {
							close(a.done)
							return
						}
						a.s.focus = focusEditor
						a.draw()
					}
					return
				}

				currentWidth += len(tabName) + len(labelClose)
			}
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
		screenCol := x - a.editor[0].x
		// Convert screen position back to original line position
		a.s.col = min(len(line), columnFromScreen(line, screenCol))
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
		// draging the selection
		a.s.selection.endRow = a.s.row
		a.s.selection.endCol = a.s.col
	}

	a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.syncCursor()
	a.drawEditor()
	a.s.upDownCol = -1 // reset up/down column tracking
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

// jump moves the cursor to the specified line and column,
// adjusting the scroll position to keep the line in view.
// Note: it does not render the editor
func (a *App) jump(row, col int) {
	a.s.row = row
	a.s.col = col
	if a.s.row < len(a.editor) {
		a.s.scroll = 0
	} else if a.s.row > a.s.lines.Len()-len(a.editor) {
		a.s.scroll = a.s.lines.Len() - len(a.editor)
	} else {
		a.s.scroll = a.s.row - len(a.editor)/2 // center the viewport
	}
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		// exit console
		a.s.console = ""
		a.s.consoleCursor = 0
		a.s.focus = focusEditor
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		// reset matched text
		if line := a.s.line(a.s.row); line != nil {
			a.drawEditorLine(a.s.row, line.Value.(string))
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
			text := sym.Name + " "
			if sym.Receiver != "" {
				text = sym.Receiver + "." + sym.Name + " "
			}
			if i == a.s.matchIdx {
				ts[i] = textStyle{text: text, style: styleHighlight}
			} else {
				ts[i] = textStyle{text: text}
			}
		}
		a.status.drawText(ts...)
	default:
		return
	}
	a.console.draw(a.s.console)
}

// closeTab closes the tab at the specified index and adjusts the current tab selection.
// It handles edge cases for tab index management and ensures a valid tab remains active.
func (st *State) closeTab(index int) {
	if index < 0 || index >= len(st.tabs) {
		return
	}

	st.tabs = slices.Delete(st.tabs, index, index+1)
	if len(st.tabs) == 0 {
		st.activeTabIdx = 0
		return
	}

	switch {
	case index < st.activeTabIdx:
		// Closed tab was before current tab, shift index left
		st.activeTabIdx--
	case index == st.activeTabIdx:
		// Closed the current tab, need to select a new one
		if st.activeTabIdx >= len(st.tabs) {
			st.activeTabIdx = len(st.tabs) - 1
		}
		// Switch to the tab now at the current index (could be same position, new tab)
		st.switchTab(st.activeTabIdx)
	case index > st.activeTabIdx:
		// Closed tab was after current tab, no index adjustment needed
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
				a.s.filename = filename // update current tab
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
		for _, v := range a.s.symbols {
			for _, sym := range v {
				name := sym.Name
				if sym.Receiver != "" {
					name = sym.Receiver + "." + sym.Name
				}
				if strings.Contains(strings.ToLower(name), strings.ToLower(symbolStr)) {
					matched = append(matched, sym)
				}
			}
		}
		slices.SortFunc(matched, func(a, b Symbol) int {
			nameA := a.Name
			nameB := b.Name
			if a.Receiver != "" {
				nameA = a.Receiver + "." + a.Name
			}
			if b.Receiver != "" {
				nameB = b.Receiver + "." + b.Name
			}
			return strings.Compare(nameA, nameB)
		})
		a.s.matchSymbols = matched
		a.s.matchIdx = 0
		if len(matched) == 0 {
			a.setStatus("no matching symbols")
			return
		}
		ts := make([]textStyle, len(matched))
		for i, sym := range matched {
			text := sym.Name + " "
			if sym.Receiver != "" {
				text = sym.Receiver + "." + sym.Name + " "
			}
			if i == a.s.matchIdx {
				ts[i] = textStyle{text: text, style: styleHighlight}
			} else {
				ts[i] = textStyle{text: text}
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
				// highlight the found text with tab expansion
				screenLine := expandTabs(line)
				screenStart := columnToScreen(line, col+i)
				screenEnd := columnToScreen(line, a.s.col)
				lineView.drawText(
					textStyle{text: screenLine[:screenStart]},
					textStyle{text: screenLine[screenStart:screenEnd], style: styleHighlight},
					textStyle{text: screenLine[screenEnd:]},
				)
				return
			}
			row++
			col = 0
		}
	default:
		// open file
		filename := cmd
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
			a.setStatus(err.Error())
			return
		}
		defer file.Close()
		lines := list.New()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			lines.PushBack(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Print(err)
			a.setStatus(err.Error())
			return
		}
		a.s.tabs = append(a.s.tabs, &Tab{filename: filename, lines: lines})
		a.s.switchTab(len(a.s.tabs) - 1)
		a.s.focus = focusEditor
		a.draw()
		return
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

func (a *App) syncCursor() {
	switch a.s.focus {
	case focusEditor:
		if a.s.row < a.s.scroll || a.s.row > (a.s.scroll+len(a.editor)-1) {
			// out of viewport
			screen.HideCursor()
			return
		}
		lineElement := a.s.line(a.s.row)
		if lineElement == nil {
			if a.s.row == 0 && a.s.col == 0 {
				// No line exists, cursor at start of editor
				screen.ShowCursor(a.editor[0].x, a.editor[0].y)
			}
			return
		}
		line := lineElement.Value.(string)
		screenCol := columnToScreen(line, a.s.col)
		screen.ShowCursor(a.editor[0].x+screenCol, a.editor[0].y+a.s.row-a.s.scroll)
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
	case tcell.KeyCtrlZ:
		a.s.undo()
		a.drawEditor()
	case tcell.KeyCtrlY:
		a.s.redo()
		a.drawEditor()
	case tcell.KeyRune:
		var line string
		e := a.s.line(a.s.row)
		if e == nil {
			// No line exists, create a new one
			line = string(ev.Rune())
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

		if selection := a.s.Selection(); selection != nil {
			// Delete the selected text
			deletedText := a.s.deleteRange(selection.startRow, selection.startCol, selection.endRow, selection.endCol)
			a.s.selection = nil

			// Insert the new rune
			line = a.s.line(a.s.row).Value.(string)
			newText := string(ev.Rune())
			line = line[:a.s.col] + newText + line[a.s.col:]
			a.s.line(a.s.row).Value = line
			a.s.col += len(newText)

			a.s.recordEdit(Edit{
				row:     selection.startRow,
				col:     selection.startCol,
				oldText: deletedText,
				newText: newText,
				kind:    editReplace,
			})
			if selection.startRow != selection.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else {
				a.drawEditorLine(a.s.row, line)
			}
			return
		}

		// No selection, insert rune normally
		line = e.Value.(string)
		line = line[:a.s.col] + string(ev.Rune()) + line[a.s.col:]
		e.Value = line
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col,
			newText: string(ev.Rune()),
			kind:    editInsert,
		})
		a.s.col++
		a.drawEditorLine(a.s.row, line)
	case tcell.KeyEnter, tcell.KeyCtrlJ:
		// for line breaks on pasting multiple lines,
		// macOS Terminal and iTerm2 sends Enter, kitty sends Ctrl-J
		a.s.lastEdit = nil
		a.s.undoStack = nil
		a.s.redoStack = nil
		if e := a.s.line(a.s.row); e == nil {
			// file end
			a.s.lines.PushBack("")
		} else {
			// break the line
			// a Enter may comes from paste, do not auto-indent
			line := e.Value.(string)
			a.s.lines.InsertAfter(line[a.s.col:], e)
			e.Value = line[:a.s.col]
			a.s.col = 0
		}
		a.s.row++
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
		}
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// Handle selection deletion
		if selection := a.s.Selection(); selection != nil {
			deletedText := a.s.deleteRange(selection.startRow, selection.startCol, selection.endRow, selection.endCol)
			a.s.selection = nil
			a.s.recordEdit(Edit{
				row:     selection.startRow,
				col:     selection.startCol,
				oldText: deletedText,
				kind:    editDelete,
			})
			if selection.startRow != selection.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else if line := a.s.line(a.s.row); line != nil {
				a.drawEditorLine(a.s.row, line.Value.(string))
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
		deleted := text[a.s.col-1]
		text = text[:a.s.col-1] + text[a.s.col:]
		line.Value = text
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col - 1,
			oldText: string(deleted),
			kind:    editDelete,
		})
		a.s.col--
		a.drawEditorLine(a.s.row, text)
	case tcell.KeyLeft:
		a.s.lastEdit = nil
		// move cursor to the start of the selection
		if selection := a.s.Selection(); selection != nil {
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
		a.s.lastEdit = nil
		// move cursor to the end of the selection
		if selection := a.s.Selection(); selection != nil {
			a.s.row = selection.endRow
			a.s.col = selection.endCol
			a.unselect()
			return
		}

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
		a.s.lastEdit = nil
		a.unselect()

		if a.s.row == 0 {
			return // already at the top
		}

		// command+up go to the start of file, works in kitty
		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.jump(0, 0)
			a.drawEditor()
			return
		}

		a.s.row--
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		if a.s.row < a.s.scroll {
			a.s.scroll--
			a.drawEditor()
			return
		}
	case tcell.KeyDown:
		a.s.lastEdit = nil
		a.unselect()

		if a.s.row == a.s.lines.Len()-1 {
			return // already at the bottom
		}

		// command+down go to the end of file, works in kitty
		if ev.Modifiers()&tcell.ModMeta != 0 {
			a.s.row = a.s.lines.Len() - 1
			bottomLine := a.s.lines.Back()
			if bottomLine == nil {
				a.s.col = 0
			} else {
				a.s.col = len(bottomLine.Value.(string)) - 1
			}
			a.jump(a.s.row, a.s.col)
			a.drawEditor()
			return
		}

		a.s.row++
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
			a.drawEditor()
			return
		}
	case tcell.KeyCtrlA, tcell.KeyHome:
		a.s.lastEdit = nil
		a.unselect()

		// move to the first non-whitespace character
		line := a.s.line(a.s.row)
		if line == nil {
			return
		}
		text := line.Value.(string)
		for i, r := range text {
			if r != ' ' && r != '\t' {
				a.s.col = i
				return
			}
		}
	case tcell.KeyCtrlE, tcell.KeyEnd:
		a.s.lastEdit = nil
		a.unselect()

		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		a.s.col = len(e.Value.(string))
	case tcell.KeyTAB:
		a.s.recordEdit(Edit{row: a.s.row, col: a.s.col, newText: "\t", kind: editInsert})
		s := "\t"
		e := a.s.line(a.s.row)
		if e == nil {
			e = a.s.lines.PushBack(s)
		} else {
			line := e.Value.(string)
			line = line[:a.s.col] + s + line[a.s.col:]
			a.s.col += len(s)
			e.Value = line
		}
		a.drawEditorLine(a.s.row, e.Value.(string))
	case tcell.KeyPgUp:
		a.unselect()
		// go to previous page or the top of the page
		a.s.row -= len(a.editor) - 2
		if a.s.row < 0 {
			a.s.row = 0
		}
		a.s.scroll = a.s.row
		a.drawEditor()
	case tcell.KeyPgDn:
		a.unselect()
		// go to next page or the bottom of the page
		a.s.row += len(a.editor) - 2
		if a.s.row >= a.s.lines.Len() {
			a.s.row = a.s.lines.Len() - 1
		}
		a.s.scroll = max(a.s.row-len(a.editor)+2, 0)
		a.drawEditor()
	case tcell.KeyCtrlC:
		if selection := a.s.Selection(); selection != nil {
			line := a.s.line(selection.startRow)
			var s strings.Builder
			if selection.startRow == selection.endRow {
				// Single line selection
				s.WriteString(line.Value.(string)[selection.startCol:selection.endCol])
			} else {
				for i := selection.startRow; i <= selection.endRow && line != nil; i++ {
					text := line.Value.(string)
					switch i {
					case selection.startRow:
						s.WriteString(text[selection.startCol:])
						s.WriteString("\n")
					case selection.endRow:
						s.WriteString(text[:selection.endCol])
					default:
						s.WriteString(text)
						s.WriteString("\n")
					}
					line = line.Next()
				}
			}
			screen.SetClipboard([]byte(s.String()))
			return
		}

		// Copy the current line to clipboard
		line := a.s.line(a.s.row)
		if line == nil {
			return
		}
		text := line.Value.(string)
		if text == "" {
			return
		}
		screen.SetClipboard([]byte(text))
	case tcell.KeyCtrlX:
		if selection := a.s.Selection(); selection != nil {
			// Cut the selected text
			deletedText := a.s.deleteRange(selection.startRow, selection.startCol, selection.endRow, selection.endCol)
			a.s.selection = nil
			a.s.recordEdit(Edit{
				row:     selection.startRow,
				col:     selection.startCol,
				oldText: deletedText,
				kind:    editDelete,
			})
			screen.SetClipboard([]byte(deletedText))
			if selection.startRow != selection.endRow {
				a.drawEditor() // Refresh full editor for multi-line changes
			} else if line := a.s.line(a.s.row); line != nil {
				a.drawEditorLine(a.s.row, line.Value.(string))
			}
			return
		}

		// Cut the current line
		line := a.s.line(a.s.row)
		if line == nil {
			return
		}
		text := line.Value.(string)
		if text == "" {
			return
		}
		screen.SetClipboard([]byte(text))
		deletedText := a.s.deleteRange(a.s.row, 0, a.s.row, len(text))
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     0,
			oldText: deletedText,
			kind:    editDelete,
		})
		a.drawEditor()
	}
}

// insertAt inserts a string at a specific position in the editor.
// If s contains multiple lines, it will be split and inserted accordingly.
// It updates the cursor position to the end of the inserted text.
func (st *State) insertAt(s string, row, col int) {
	e := st.line(row)
	if e == nil {
		return
	}

	lines := strings.Split(s, "\n")
	if len(lines) == 1 {
		// Single line insertion
		line := e.Value.(string)
		line = line[:col] + s + line[col:]
		e.Value = line
		st.row = row
		st.col = col + len(s)
		return
	}

	origin := e.Value.(string)

	// multi-line insertion
	for i, line := range lines {
		if i == 0 {
			// First line
			e.Value = origin[:col] + line
		} else if i == len(lines)-1 {
			// Last line
			st.lines.InsertAfter(line+origin[col:], e)
		} else {
			// Middle lines
			e = st.lines.InsertAfter(line, e)
		}
	}
	st.row += len(lines) - 1
	st.col = len(lines[len(lines)-1])
}

// unselect cancel the selection and redraws the affected lines.
func (a *App) unselect() {
	selection := a.s.Selection()
	if selection == nil {
		return
	}

	a.s.selection = nil
	line := a.s.line(selection.startRow)
	for i := selection.startRow; i <= selection.endRow && line != nil; i++ {
		a.drawEditorLine(i, line.Value.(string))
		line = line.Next()
	}
}

// Selection returns the current selection, ensuring it is in a consistent order.
// TODO: do this on mouse button release?
func (st *State) Selection() *Selection {
	if st.selection == nil {
		return nil
	}
	if st.selection.startRow == st.selection.endRow &&
		st.selection.startCol == st.selection.endCol {
		// No selection
		return nil
	}

	if st.selection.startRow > st.selection.endRow ||
		(st.selection.startRow == st.selection.endRow && st.selection.startCol > st.selection.endCol) {
		// Swap if selection is reversed
		st.selection.startRow, st.selection.endRow = st.selection.endRow, st.selection.startRow
		st.selection.startCol, st.selection.endCol = st.selection.endCol, st.selection.startCol
	}
	return st.selection
}

// deleteRange deletes a range of text [startRow:startCol, endRow:endCol) from the editor
// and move the cursor to the start of the deleted range.
// It returns the deleted text as a string.
func (st *State) deleteRange(startRow, startCol, endRow, endCol int) string {
	var deleted strings.Builder
	if startRow == endRow {
		// Single-line
		line := st.line(startRow).Value.(string)
		deleted.WriteString(line[startCol:endCol])
		line = line[:startCol] + line[endCol:]
		st.line(startRow).Value = line
	} else {
		// Multi-line
		line := st.line(startRow)
		firstLineLeft := line.Value.(string)[:startCol]
		for i := startRow; i <= endRow && line != nil; i++ {
			text := line.Value.(string)
			next := line.Next()
			switch i {
			case startRow:
				deleted.WriteString(text[startCol:])
				deleted.WriteString("\n")
				st.lines.Remove(line)
			case endRow:
				deleted.WriteString(text[:endCol])
				line.Value = firstLineLeft + text[endCol:]
			default:
				deleted.WriteString(text)
				deleted.WriteString("\n")
				st.lines.Remove(line)
			}
			line = next
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

func highlightGoLine(line string) []textStyle {
	var parts []textStyle
	runes := []rune(line)

	var inString bool
	var inComment bool
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			w := word.String()
			if token.IsKeyword(w) {
				parts = append(parts, textStyle{text: w, style: styleKeyword})
			} else if _, err := strconv.Atoi(w); err == nil {
				parts = append(parts, textStyle{text: w, style: styleNumber})
			} else {
				parts = append(parts, textStyle{text: w})
			}
			word.Reset()
		}
	}

	for i := range runes {
		c := runes[i]

		// Line comment
		if !inString && c == '/' && i+1 < len(runes) && runes[i+1] == '/' {
			flushWord()
			parts = append(parts, textStyle{text: string(runes[i:]), style: styleComment})
			return parts
		}

		// Strings
		if c == '"' && !inComment {
			if inString {
				word.WriteRune(c)
				parts = append(parts, textStyle{text: word.String(), style: styleString})
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
			parts = append(parts, textStyle{text: string(c)})
		}
	}

	flushWord()
	return parts
}
